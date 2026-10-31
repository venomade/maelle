// maelle cli — a minimal streaming OpenAI client for the maelle proxy.
//
// This is the existence proof for the design: it is a *stock* agent loop
// (declare your own tools, stream, run tool_calls, send results back,
// repeat) with zero proxy-specific protocol. Everything Maelle does on the
// server arrives as ordinary assistant content — including the
// [maelle:tool {...}] activity markers, which a stock client would just
// show as text. The only extra here is pretty-printing those markers.
//
// Env: MAELLE_URL (default http://127.0.0.1:8080/v1),
//
//	MAELLE_PROXY_KEY (required), MODEL (default mistral-small-latest)
//
// Commands: /reset clears history, /exit quits.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const markerPrefix = "[maelle:tool "

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var (
	baseURL = getenv("MAELLE_URL", "http://127.0.0.1:8080/v1")
	apiKey  = os.Getenv("MAELLE_PROXY_KEY")
	model   = getenv("MODEL", "mistral-small-latest")
)

// The client's OWN tools. Maelle sees these merged with her maelle_* tools
// and must decide where to run each one; the descriptions say so.
var clientTools = []any{
	map[string]any{"type": "function", "function": map[string]any{
		"name":        "client_get_time",
		"description": "Get the current local time on the USER's machine (not Maelle's server).",
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
	}},
	map[string]any{"type": "function", "function": map[string]any{
		"name":        "client_read_local",
		"description": "Read a text file (cwd-relative, max 4 KB) on the USER's machine.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "File path on the user's machine"},
		}, "required": []any{"path"}},
	}},
}

func runClientTool(name, args string) string {
	var a map[string]any
	_ = json.Unmarshal([]byte(args), &a)
	switch name {
	case "client_get_time":
		return time.Now().Format(time.RFC1123)
	case "client_read_local":
		p, _ := a["path"].(string)
		data, err := os.ReadFile(p)
		if err != nil {
			return "error: " + err.Error()
		}
		if len(data) > 4096 {
			data = data[:4096]
		}
		return string(data)
	}
	return "error: unknown tool " + name
}

func main() {
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "MAELLE_PROXY_KEY is not set")
		os.Exit(1)
	}
	var history []any
	fmt.Printf("maelle cli — %s (model %s)\ncommands: /reset /exit\n", baseURL, model)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		fmt.Print("\n> ")
		if !sc.Scan() {
			return
		}
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
			continue
		case line == "/exit", line == "/quit":
			return
		case line == "/reset":
			history = nil
			fmt.Println("(history cleared)")
			continue
		}
		history = append(history, map[string]any{"role": "user", "content": line})

		// Agent loop: stream a turn, execute any client tool_calls the model
		// made, send the results back, repeat until a plain answer. Maelle's
		// own server-side tools never appear here — the proxy ran them.
		for {
			content, calls, err := streamTurn(history)
			if err != nil {
				fmt.Println("\n[error]", err)
				break
			}
			msg := map[string]any{"role": "assistant", "content": content}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			history = append(history, msg)
			if len(calls) == 0 {
				break
			}
			for _, c := range calls {
				cm, _ := c.(map[string]any)
				id, _ := cm["id"].(string)
				fn, _ := cm["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				fmt.Printf("\n  └─ %s(%s) …", name, args)
				result := runClientTool(name, args)
				fmt.Println(" done")
				history = append(history, map[string]any{
					"role": "tool", "tool_call_id": id, "content": result,
				})
			}
		}
	}
}

// streamTurn POSTs one streaming request and prints content live, rendering
// [maelle:tool {...}] markers as a compact activity log. It returns the
// full assistant content (markers included — the history needs them for
// the proxy's rewriteHistory) and the assembled client tool_calls.
func streamTurn(history []any) (string, []any, error) {
	req := map[string]any{
		"model": model, "stream": true,
		"messages": history, "tools": clientTools,
	}
	b, err := json.Marshal(req)
	if err != nil {
		return "", nil, err
	}
	hr, err := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", nil, err
	}
	hr.Header.Set("Authorization", "Bearer "+apiKey)
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var content strings.Builder
	fragments := map[int]map[string]string{} // tool_call index -> {id,name,args}

	reader := bufio.NewReader(resp.Body)
	for {
		line, rerr := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimSpace(line[len("data: "):])
			if payload == "[DONE]" {
				break
			}
			var data map[string]any
			if json.Unmarshal([]byte(payload), &data) == nil {
				if choices, _ := data["choices"].([]any); len(choices) > 0 {
					choice, _ := choices[0].(map[string]any)
					delta, _ := choice["delta"].(map[string]any)
					if c, ok := delta["content"].(string); ok && c != "" {
						content.WriteString(c)
						renderDelta(c)
					}
					if tcs, ok := delta["tool_calls"].([]any); ok {
						for _, raw := range tcs {
							tc, _ := raw.(map[string]any)
							idx := 0
							if f, ok := tc["index"].(float64); ok {
								idx = int(f)
							}
							fr := fragments[idx]
							if fr == nil {
								fr = map[string]string{}
								fragments[idx] = fr
							}
							if id, ok := tc["id"].(string); ok && id != "" {
								fr["id"] = id
							}
							if fn, ok := tc["function"].(map[string]any); ok {
								if n, ok := fn["name"].(string); ok && n != "" {
									fr["name"] = n
								}
								if a, ok := fn["arguments"].(string); ok {
									fr["args"] += a
								}
							}
						}
					}
				}
			}
		}
		if rerr != nil {
			break
		}
	}

	idxs := make([]int, 0, len(fragments))
	for i := range fragments {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	var calls []any
	for _, i := range idxs {
		fr := fragments[i]
		calls = append(calls, map[string]any{
			"id": fr["id"], "type": "function",
			"function": map[string]any{"name": fr["name"], "arguments": fr["args"]},
		})
	}
	return content.String(), calls, nil
}

// renderDelta prints streamed content, pretty-printing any complete
// [maelle:tool {...}] marker as one activity line. Markers are emitted as
// single deltas by the proxy, so a per-delta scan is sufficient here.
func renderDelta(c string) {
	rest := c
	for {
		i := strings.Index(rest, markerPrefix)
		if i < 0 {
			fmt.Print(rest)
			return
		}
		fmt.Print(rest[:i])
		j := jsonEnd(rest, i+len(markerPrefix))
		if j < 0 {
			fmt.Print(rest[i:])
			return
		}
		end := j
		for end < len(rest) && rest[end] == ' ' {
			end++
		}
		if end >= len(rest) || rest[end] != ']' {
			fmt.Print(rest[i:])
			return
		}
		end++
		var m struct {
			Name, Arguments, Status, Result string
		}
		if err := json.Unmarshal([]byte(rest[i+len(markerPrefix):j]), &m); err == nil {
			res := m.Result
			if len(res) > 120 {
				res = res[:120] + "…"
			}
			fmt.Printf("\n  ┌ %s(%s)\n  └ %s: %s\n", m.Name, m.Arguments, m.Status, res)
		}
		rest = rest[end:]
	}
}

// jsonEnd returns the offset just past the JSON object starting at '{'
// (string-aware brace counting), or -1 if the object is incomplete.
func jsonEnd(s string, start int) int {
	if start >= len(s) || s[start] != '{' {
		return -1
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		if inStr {
			switch c {
			case '\\':
				esc = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}
