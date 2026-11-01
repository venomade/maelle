// Maelle's server-side tools: the implementations the proxy runs when the
// model calls a maelle_* tool, plus the [maelle:tool {...}] markers that
// report the run to the client — and the history rewriter that turns those
// markers back into real tool_calls + tool results on the next request, so
// the proxy stays stateless and OpenAI-compatible end to end.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	markerPrefix = "[maelle:tool "
	resultCap    = 2048 // stored in the marker (replay fidelity cap)
	readCap      = 64 * 1024
	dirCap       = 512
	cmdOutCap    = 64 * 1024
	cmdTimeout   = 60

	webSearchCap    = 16 * 1024
	webFetchCap     = 32 * 1024
	webFetchBodyMax = 512 * 1024
	webFetchTimeout = 30 * time.Second
)

// Full server access: commands run via passwordless sudo (the NixOS config
// gives the maelle user passwordless wheel). Set MAELLE_TOOL_SUDO=0 for
// local testing without sudo.
var toolSudo = os.Getenv("MAELLE_TOOL_SUDO") != "0"

// SearXNG (the NixOS module) serves JSON on the loopback interface.
var searxngURL = getenv("MAELLE_SEARXNG_URL", "http://127.0.0.1:8888")

var (
	// crude HTML -> text, tuned by fetching real pages (Wikipedia, news
	// sites): 1. drop chrome blocks (script/style/nav/header/footer/aside/
	// form/noscript/svg/iframe/template/select) wholesale; 2. start the
	// text at the first <h1> — on article pages everything before it is
	// site chrome (menus, search boxes, language lists), which used to eat
	// the whole output cap before the article began; 3. strip comments and
	// tags, unescape entities, drop wiki-style {{template}} markup (leaks
	// from Parsoid round-trip data), collapse all whitespace. RE2 has no
	// backreferences, so the closing-tag alternation repeats the opening.
	reBlocks = regexp.MustCompile(`(?is)<(?:script|style|nav|header|footer|aside|form|noscript|svg|iframe|template|select)[^>]*>.*?</(?:script|style|nav|header|footer|aside|form|noscript|svg|iframe|template|select)\s*>`)
	reTags   = regexp.MustCompile(`(?s)<!--.*?-->|<[^>]*>`)
	// RE2 caps repeat counts at 1000 — {0,2000} panics at init (exit 2).
	reWiki   = regexp.MustCompile(`\{\{[^{}]{0,1000}\}\}`)
)

type toolMarker struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
	Result    string `json:"result"`
}

type toolFunc func(map[string]any) (string, error)

var toolImpls = map[string]toolFunc{
	"maelle_read_file":   toolReadFile,
	"maelle_list_dir":    toolListDir,
	"maelle_run_command": toolRunCommand,
	"maelle_web_search":  toolWebSearch,
	"maelle_web_fetch":   toolWebFetch,
}

// executeTool runs one maelle_* call. Errors are tool *results* (status
// "error"), so the model can read them and self-correct — not HTTP failures.
func executeTool(name, arguments string) (status, result string) {
	var args map[string]any
	if strings.TrimSpace(arguments) != "" {
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "error", "invalid arguments JSON: " + err.Error()
		}
	}
	fn, ok := toolImpls[name]
	if !ok {
		return "error", "unknown tool: " + name
	}
	out, err := fn(args)
	if err != nil {
		return "error", err.Error()
	}
	return "ok", out
}

func toolReadFile(a map[string]any) (string, error) {
	path, _ := a["path"].(string)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(data) > readCap {
		return string(data[:readCap]) +
			fmt.Sprintf("\n…[truncated, file larger than %d bytes]", readCap), nil
	}
	return string(data), nil
}

func toolListDir(a map[string]any) (string, error) {
	path, _ := a["path"].(string)
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) > dirCap {
		return strings.Join(names[:dirCap], "\n") +
			fmt.Sprintf("\n…[%d more entries]", len(names)-dirCap), nil
	}
	return strings.Join(names, "\n"), nil
}

func toolRunCommand(a map[string]any) (string, error) {
	cmd, _ := a["command"].(string)
	timeout := cmdTimeout
	if v, ok := a["timeout_seconds"].(float64); ok && v >= 1 {
		timeout = int(v)
	}
	if timeout < 1 {
		timeout = 1
	}
	if timeout > 300 {
		timeout = 300
	}
	argv := []string{"bash", "-c", cmd}
	if toolSudo {
		// -n: fail fast instead of hanging if sudo ever asks for a password
		// (the service has no tty to answer on).
		argv = []string{"sudo", "-n", "bash", "-c", cmd}
	}
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(timeout)*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("error: timed out after %ds", timeout), nil
	}
	exit := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			return "", err
		}
	}
	s := string(out)
	if len(s) > cmdOutCap {
		s = s[:cmdOutCap] + fmt.Sprintf("\n…[truncated, more than %d bytes]", cmdOutCap)
	}
	return fmt.Sprintf("exit=%d\n%s", exit, s), nil
}

// toolWebSearch queries the local SearXNG instance (JSON API) and formats
// the results as title / url / snippet blocks. SearXNG needs format=json
// enabled in its settings — the NixOS config does that.
func toolWebSearch(a map[string]any) (string, error) {
	query, _ := a["query"].(string)
	maxResults := 8
	if v, ok := a["max_results"].(float64); ok && v >= 1 {
		maxResults = int(v)
	}
	if maxResults < 1 {
		maxResults = 1
	}
	if maxResults > 20 {
		maxResults = 20
	}
	u := searxngURL + "/search?q=" + url.QueryEscape(query) + "&format=json"
	hr, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), webFetchTimeout)
	defer cancel()
	hr = hr.WithContext(ctx)
	resp, err := httpClient.Do(hr)
	if err != nil {
		return "", fmt.Errorf("searxng unreachable: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("searxng returned status %d", resp.StatusCode)
	}
	var data struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", fmt.Errorf("searxng returned invalid JSON: %v", err)
	}
	if len(data.Results) == 0 {
		return "no results for: " + query, nil
	}
	if len(data.Results) > maxResults {
		data.Results = data.Results[:maxResults]
	}
	blocks := make([]string, 0, len(data.Results))
	for _, r := range data.Results {
		blocks = append(blocks, r.Title+"\n"+r.URL+"\n"+r.Content)
	}
	out := strings.Join(blocks, "\n\n")
	if len(out) > webSearchCap {
		out = out[:webSearchCap] +
			fmt.Sprintf("\n…[truncated, more than %d bytes]", webSearchCap)
	}
	return out, nil
}

// toolWebFetch GETs a page and returns readable text. JS-rendered pages
// stay empty and nobody checks robots.txt — acceptable for a personal
// agent, documented as such.
func toolWebFetch(a map[string]any) (string, error) {
	target, _ := a["url"].(string)
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		return "", fmt.Errorf("url must start with http:// or https://")
	}
	hr, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	// a real browser UA: sites serve layouted HTML to it and soft-block
	// obvious bot UAs with 403s.
	hr.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "+
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	hr.Header.Set("Accept", "text/html, text/plain, */*")
	hr.Header.Set("Accept-Language", "en")
	ctx, cancel := context.WithTimeout(context.Background(), webFetchTimeout)
	defer cancel()
	hr = hr.WithContext(ctx)
	resp, err := httpClient.Do(hr)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %v", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: status %d", target, resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, webFetchBodyMax+1))
	if len(body) > webFetchBodyMax {
		body = body[:webFetchBodyMax]
	}
	text := string(body)
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "html") {
		text = htmlToText(text)
	} else {
		text = strings.Join(strings.Fields(text), " ")
	}
	if len(text) > webFetchCap {
		text = text[:webFetchCap] +
			fmt.Sprintf(" …[truncated, more than %d chars]", webFetchCap)
	}
	return text, nil
}

func htmlToText(s string) string {
	s = reBlocks.ReplaceAllString(s, " ")
	if i := strings.Index(strings.ToLower(s), "<h1"); i > 0 {
		s = s[i:]
	}
	s = reTags.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = reWiki.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

// encodeMarker renders one executed call as a single-line content marker:
// [maelle:tool {"id":...,"name":...,"arguments":...,"status":...,"result":...}]
func encodeMarker(id, name, args, status, result string) string {
	if len(result) > resultCap {
		result = result[:resultCap] + fmt.Sprintf("…[+%d chars]", len(result)-resultCap)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(toolMarker{ID: id, Name: name, Arguments: args,
		Status: status, Result: result})
	return markerPrefix + strings.TrimSpace(buf.String()) + "]"
}

// parseMarkers splits content into (clean text, markers). A "[maelle:tool "
// prefix whose JSON does not parse — or is not closed by "]" — stays in the
// text as ordinary characters, so model-generated look-alikes can't corrupt
// history unless they are well-formed markers.
func parseMarkers(content string) (string, []toolMarker) {
	var clean strings.Builder
	var markers []toolMarker
	pos := 0
	for {
		rel := strings.Index(content[pos:], markerPrefix)
		if rel < 0 {
			clean.WriteString(content[pos:])
			break
		}
		start := pos + rel
		clean.WriteString(content[pos:start])
		idx := start + len(markerPrefix)
		dec := json.NewDecoder(strings.NewReader(content[idx:]))
		var m toolMarker
		next := -1
		if err := dec.Decode(&m); err == nil {
			next = markerClosed(content, idx+int(dec.InputOffset()))
		}
		if next < 0 {
			clean.WriteString(markerPrefix)
			pos = idx
			continue
		}
		markers = append(markers, m)
		pos = next
	}
	return clean.String(), markers
}

// markerClosed returns the offset just past the closing "]" after the JSON
// value that ends at off, or -1 if there is none.
func markerClosed(s string, off int) int {
	for off < len(s) && (s[off] == ' ' || s[off] == '\t') {
		off++
	}
	if off < len(s) && s[off] == ']' {
		return off + 1
	}
	return -1
}

// rewriteHistory turns client-round-tripped markers back into real upstream
// history: the assistant message regains its maelle_* tool_calls (with the
// client's own tool_calls kept), and the stored results are re-injected as
// tool messages right after it. This is what lets a stateless proxy and a
// stock OpenAI client close the mixed-batch loop: the markers ride inside
// assistant content, which every OpenAI client preserves verbatim.
func rewriteHistory(messages []any) []any {
	out := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		content, isStr := msg["content"].(string)
		if msg["role"] != "assistant" || !isStr ||
			!strings.Contains(content, markerPrefix) {
			out = append(out, m)
			continue
		}
		clean, markers := parseMarkers(content)
		if len(markers) == 0 {
			out = append(out, m)
			continue
		}
		nm := map[string]any{"role": "assistant", "content": clean}
		for k, v := range msg {
			if k != "role" && k != "content" && k != "tool_calls" {
				nm[k] = v
			}
		}
		calls, _ := msg["tool_calls"].([]any)
		var toolMsgs []any
		for _, mk := range markers {
			calls = append(calls, map[string]any{
				"id": mk.ID, "type": "function",
				"function": map[string]any{"name": mk.Name, "arguments": mk.Arguments},
			})
			toolMsgs = append(toolMsgs, map[string]any{
				"role": "tool", "tool_call_id": mk.ID, "content": mk.Result,
			})
		}
		nm["tool_calls"] = calls
		out = append(out, nm)
		out = append(out, toolMsgs...)
	}
	return out
}
