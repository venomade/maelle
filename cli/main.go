// maelle cli — an interactive REPL for the maelle proxy.
//
// This is the existence proof for the design: it is a *stock* agent loop
// (declare your own tools, stream, run tool_calls, send results back,
// repeat) with zero proxy-specific protocol. Everything Maelle does on the
// server arrives as ordinary assistant content — including the
// [maelle:tool {...}] activity markers, which this REPL renders as activity
// boxes instead of raw text.
//
// Env: MAELLE_URL (default http://127.0.0.1:8080/v1),
//
//	MAELLE_PROXY_KEY (required), MODEL (default mistral-small-latest),
//	MAELLE_COLOR (auto|always|never; NO_COLOR respected)
//
// Inside the REPL: /help lists the slash commands, !<cmd> runs a local shell
// command, a trailing backslash makes multi-line input, and on a terminal
// the line editor supports arrows, Ctrl-A/E/U/K/W/L, history (up/down) and
// Tab completion. Settings persist in ~/.config/maelle/config.json; input
// history in ~/.cache/maelle/history.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type session struct {
	t          *term
	ed         *editor
	baseURL    string
	apiKey     string
	cfg        config
	history    []any
	lastUsage  *usage
	rawMarkers bool
}

func main() {
	initColor()
	apiKey := os.Getenv("MAELLE_PROXY_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "MAELLE_PROXY_KEY is not set")
		os.Exit(1)
	}
	t := newTerm(os.Stdout)
	t.tty = isCharDevice(os.Stdin) && isCharDevice(os.Stdout)
	s := &session{
		t:       t,
		baseURL: strings.TrimSuffix(getenv("MAELLE_URL", "http://127.0.0.1:8080/v1"), "/"),
		apiKey:  apiKey,
		cfg:     loadConfig(),
	}
	ed := newEditor(t)
	s.ed = ed
	defer ed.close()

	sigc := make(chan os.Signal, 1)
	watchResize(sigc)
	defer stopWatchResize(sigc)
	go func() {
		for range sigc {
			t.mu.Lock()
			t.width = detectWidth()
			t.mu.Unlock()
		}
	}()

	s.printBanner()
	prompt := boldMagenta("maelle ❯ ")
	if !t.tty {
		prompt = "" // piped input is echoed by the REPL itself as "> line"
	}
loop:
	for {
		line, eof := ed.readLine(prompt)
		if eof {
			t.line("")
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !t.tty {
			t.line(dim("> ") + line)
		}
		switch {
		case strings.HasPrefix(line, "/"):
			if s.handleCommand(line) {
				break loop
			}
		case strings.HasPrefix(line, "!"):
			s.runLocal(line[1:])
		default:
			s.history = append(s.history, map[string]any{"role": "user", "content": line})
			s.runTurn()
		}
	}
	t.line(dim("bye"))
}

func (s *session) printBanner() {
	t := s.t
	t.line("")
	t.line("  " + boldCyan("maelle") + dim(" — interactive REPL for the maelle proxy"))
	t.line(dim("  endpoint    ") + s.baseURL)
	t.line(dim("  model       ") + s.cfg.Model + dim("  ·  stream "+onOff(s.cfg.Stream)))
	if s.cfg.Temp != nil {
		t.line(dim(fmt.Sprintf("  temperature %g", *s.cfg.Temp)))
	}
	if s.cfg.MaxTokens != nil {
		t.line(dim(fmt.Sprintf("  max_tokens  %d", *s.cfg.MaxTokens)))
	}
	if s.cfg.System != "" {
		t.line(dim(fmt.Sprintf("  system      %d chars (client-side)", len(s.cfg.System))))
	}
	t.line(dim("  /help for commands · /exit or Ctrl-D to quit · end a line with \\ for multi-line"))
	t.line("")
}

// runTurn runs the agent loop for the current history (which must end with a
// user message): stream or fetch a turn, execute any client tool_calls the
// model made, send the results back, repeat until a plain answer. Maelle's
// own server-side tools never appear as tool_calls here — the proxy ran
// them and reported them as [maelle:tool] markers inside the content.
func (s *session) runTurn() {
	t := s.t
	start := time.Now()
	sp := startSpinner(t, "thinking")
	labelPrinted := false
	printLabel := func() {
		if !labelPrinted {
			t.print(boldCyan("maelle") + " ")
			labelPrinted = true
		}
	}
	ms := newMarkerStream(t, newMdRenderer(t), s.rawMarkers)
	for round := 0; round < 10; round++ {
		var res chatResult
		var err error
		if s.cfg.Stream {
			res, err = s.chatStream(func(c string) {
				if sp != nil {
					sp.stopNow()
					sp = nil
				}
				printLabel()
				ms.feed(c)
			})
		} else {
			res, err = s.chatNonStream()
			if sp != nil {
				sp.stopNow()
				sp = nil
			}
			printLabel()
			ms.feed(res.Content)
		}
		if err != nil {
			if sp != nil {
				sp.stopNow()
			}
			ms.flush()
			printLabel()
			t.line(red("error: ") + err.Error())
			t.write("\n")
			return
		}
		s.lastUsage = res.Usage
		msg := map[string]any{"role": "assistant", "content": res.Content}
		if len(res.Calls) > 0 {
			var calls []any
			for _, c := range res.Calls {
				calls = append(calls, map[string]any{
					"id": c.ID, "type": "function",
					"function": map[string]any{"name": c.Name, "arguments": c.Arguments},
				})
			}
			msg["tool_calls"] = calls
		}
		s.history = append(s.history, msg)
		if len(res.Calls) == 0 {
			break
		}
		for _, c := range res.Calls {
			t.line("  " + yellow("⠿") + " " + cyan(c.Name) + dim("("+c.Arguments+")") + " …")
			result := runClientTool(c.Name, c.Arguments)
			t.line("  └─ " + dim(preview(result, 160)))
			s.history = append(s.history, map[string]any{
				"role": "tool", "tool_call_id": c.ID, "content": result,
			})
		}
	}
	ms.flush()
	if sp != nil {
		sp.stopNow()
	}
	printLabel()
	t.write("\n")
	t.line(dim(fmt.Sprintf("  · %s · %s · %s",
		s.cfg.Model, usageStr(s.lastUsage), time.Since(start).Round(time.Millisecond))))
}

func usageStr(u *usage) string {
	if u == nil {
		return "no usage"
	}
	return fmt.Sprintf("%d tok (%d in / %d out)", u.Total, u.Prompt, u.Completion)
}
