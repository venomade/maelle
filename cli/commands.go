package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var commandNames = []string{
	"help", "exit", "quit", "reset", "clear", "model", "models", "ping",
	"stream", "temp", "max-tokens", "system", "reasoning", "tools", "history",
	"undo", "retry", "save", "load", "compact", "raw", "usage",
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// handleCommand runs one slash command; it returns true when the REPL should
// quit.
func (s *session) handleCommand(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	cmd := strings.TrimPrefix(fields[0], "/")
	arg := strings.TrimSpace(line[len(fields[0]):])
	switch cmd {
	case "help":
		s.cmdHelp()
	case "exit", "quit":
		return true
	case "reset":
		s.history = nil
		s.note("conversation cleared")
	case "clear":
		s.t.write("\x1b[2J\x1b[H")
	case "model":
		s.cmdModel(arg)
	case "models":
		s.cmdModels()
	case "ping":
		s.cmdPing()
	case "stream":
		s.cmdStream(arg)
	case "temp":
		s.cmdTemp(arg)
	case "max-tokens":
		s.cmdMaxTokens(arg)
	case "system":
		s.cmdSystem(arg)
	case "reasoning":
		s.cmdReasoning(arg)
	case "tools":
		s.cmdTools()
	case "history":
		s.cmdHistory()
	case "undo":
		s.cmdUndo(false)
	case "retry":
		s.cmdRetry()
	case "save":
		s.cmdSave(arg)
	case "load":
		s.cmdLoad(arg)
	case "compact":
		s.cmdCompact()
	case "raw":
		s.rawMarkers = !s.rawMarkers
		s.note("raw markers " + onOff(s.rawMarkers))
	case "usage":
		s.cmdUsage()
	default:
		s.err(fmt.Errorf("unknown command /%s — /help lists them all", cmd))
	}
	return false
}

func (s *session) note(msg string) { s.t.line(dim("  · " + msg)) }
func (s *session) err(err error)   { s.t.line(red("  error: ") + err.Error()) }

func (s *session) cmdHelp() {
	t := s.t
	t.line(dim("  commands:"))
	rows := [][2]string{
		{"/help", "show this help"},
		{"/model [name]", "show or switch the model"},
		{"/models", "list models from the API (GET /v1/models)"},
		{"/ping", "check the proxy (GET /healthz)"},
		{"/stream [on|off]", "toggle streaming responses"},
		{"/temp <0-2>", "set the sampling temperature"},
		{"/max-tokens <n>|off", "set max_tokens"},
		{"/system [text|off]", "set/clear a client-side system prompt"},
		{"/reasoning [none|low|medium|high|off]", "set reasoning_effort (reasoning models)"},
		{"/tools", "list the client tools maelle can call on this machine"},
		{"/history", "print the conversation so far"},
		{"/undo", "drop the last exchange"},
		{"/retry", "resend the last message"},
		{"/save <file>", "save the conversation to a JSON file"},
		{"/load <file>", "load a conversation from a JSON file"},
		{"/compact", "summarise the conversation into the context"},
		{"/raw", "toggle raw [maelle:tool] marker display"},
		{"/usage", "token usage of the last turn"},
		{"/reset", "clear the conversation"},
		{"/clear", "clear the screen"},
		{"/exit, /quit", "quit"},
		{"!<cmd>", "run a shell command locally; output joins the context"},
		{"<text>\\", "end a line with a backslash for multi-line input"},
	}
	for _, r := range rows {
		t.line("  " + cyan(fmt.Sprintf("%-22s", r[0])) + dim(r[1]))
	}
}

func (s *session) cmdModel(arg string) {
	if arg == "" {
		s.note("model: " + s.cfg.Model)
		return
	}
	s.cfg.Model = arg
	s.cfg.save()
	s.note("model set to " + arg)
}

func (s *session) cmdModels() {
	ids, err := s.listModels()
	if err != nil {
		s.err(err)
		return
	}
	if s.ed != nil {
		s.ed.models = ids
	}
	s.note(fmt.Sprintf("%d models from %s/models", len(ids), s.baseURL))
	for _, id := range ids {
		if id == s.cfg.Model {
			s.t.line("  " + green("*") + " " + id + dim("  (current)"))
		} else {
			s.t.line("    " + id)
		}
	}
}

func (s *session) cmdPing() {
	d, err := s.ping()
	if err != nil {
		s.err(err)
		return
	}
	s.t.line(dim(fmt.Sprintf("  ok · %s/healthz · %s", s.rootURL(), d.Round(time.Microsecond))))
}

func (s *session) cmdStream(arg string) {
	switch strings.ToLower(arg) {
	case "":
		s.note("stream " + onOff(s.cfg.Stream))
		return
	case "on", "true", "1", "yes":
		s.cfg.Stream = true
	default:
		s.cfg.Stream = false
	}
	s.cfg.save()
	s.note("stream " + onOff(s.cfg.Stream))
}

func (s *session) cmdTemp(arg string) {
	if arg == "" {
		if s.cfg.Temp != nil {
			s.note(fmt.Sprintf("temperature %g", *s.cfg.Temp))
		} else {
			s.note("temperature unset (API default)")
		}
		return
	}
	v, err := strconv.ParseFloat(arg, 64)
	if err != nil || v < 0 || v > 2 {
		s.err(errors.New("temperature must be a number between 0 and 2"))
		return
	}
	s.cfg.Temp = &v
	s.cfg.save()
	s.note(fmt.Sprintf("temperature %g", v))
}

func (s *session) cmdMaxTokens(arg string) {
	if arg == "" {
		if s.cfg.MaxTokens != nil {
			s.note(fmt.Sprintf("max_tokens %d", *s.cfg.MaxTokens))
		} else {
			s.note("max_tokens unset (API default)")
		}
		return
	}
	if arg == "off" || arg == "none" {
		s.cfg.MaxTokens = nil
		s.cfg.save()
		s.note("max_tokens unset")
		return
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 {
		s.err(errors.New("max-tokens must be a positive integer or 'off'"))
		return
	}
	s.cfg.MaxTokens = &n
	s.cfg.save()
	s.note(fmt.Sprintf("max_tokens %d", n))
}

func (s *session) cmdSystem(arg string) {
	if arg == "" {
		if s.cfg.System != "" {
			s.note(fmt.Sprintf("client system prompt (%d chars): %s", len(s.cfg.System), preview(s.cfg.System, 80)))
		} else {
			s.note("no client system prompt (the proxy's persona is the only one)")
		}
		return
	}
	if arg == "off" || arg == "clear" {
		s.cfg.System = ""
		s.cfg.save()
		s.note("client system prompt cleared")
		return
	}
	s.cfg.System = arg
	s.cfg.save()
	s.note(fmt.Sprintf("client system prompt set (%d chars)", len(arg)))
}

var reasoningLevels = []string{"none", "low", "medium", "high"}

func (s *session) cmdReasoning(arg string) {
	if arg == "" {
		if s.cfg.Reasoning != "" {
			s.note("reasoning_effort: " + s.cfg.Reasoning)
		} else {
			s.note("reasoning_effort unset (API default) — one of: " + strings.Join(reasoningLevels, " "))
		}
		return
	}
	arg = strings.ToLower(arg)
	if arg == "off" || arg == "clear" {
		s.cfg.Reasoning = ""
		s.cfg.save()
		s.note("reasoning_effort unset")
		return
	}
	ok := false
	for _, l := range reasoningLevels {
		if arg == l {
			ok = true
			break
		}
	}
	if !ok {
		s.err(fmt.Errorf("reasoning must be one of %s, or 'off'", strings.Join(reasoningLevels, " ")))
		return
	}
	s.cfg.Reasoning = arg
	s.cfg.save()
	s.note("reasoning_effort " + arg)
}

func (s *session) cmdTools() {
	s.t.line(dim("  client tools — declared here, run on YOUR machine when maelle calls them:"))
	for _, t := range clientTools {
		m, _ := t.(map[string]any)["function"].(map[string]any)
		name, _ := m["name"].(string)
		desc, _ := m["description"].(string)
		s.t.line("  " + cyan(name) + "  " + dim(desc))
	}
	s.t.line(dim("  maelle_* tools run on Maelle's server inside the proxy; each run is"))
	s.t.line(dim("  reported back as a [maelle:tool {...}] marker (see /raw)."))
}

func (s *session) cmdHistory() {
	if len(s.history) == 0 {
		s.note("conversation is empty")
		return
	}
	for _, m := range s.history {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content, _ := msg["content"].(string)
		switch role {
		case "user":
			s.t.line(boldMagenta("you ❯"))
			md := newMdRenderer(s.t)
			md.feed(content)
			md.flush()
		case "assistant":
			s.t.line(boldCyan("maelle"))
			ms := newMarkerStream(s.t, newMdRenderer(s.t), s.rawMarkers)
			ms.feed(content)
			ms.flush()
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					cm, _ := tc.(map[string]any)
					fn, _ := cm["function"].(map[string]any)
					name, _ := fn["name"].(string)
					args, _ := fn["arguments"].(string)
					s.t.line(dim(fmt.Sprintf("  ⠿ %s(%s)", name, args)))
				}
			}
		case "tool":
			id, _ := msg["tool_call_id"].(string)
			s.t.line(dim(fmt.Sprintf("  tool %s → %s", id, preview(content, 80))))
		case "system":
			s.t.line(dim(italic("system: " + preview(content, 120))))
		}
	}
}

// cmdUndo drops the last user message and everything after it. Returns false
// when there is nothing to undo.
func (s *session) cmdUndo(silent bool) bool {
	h, n := trimLastTurn(s.history)
	if n == 0 {
		if !silent {
			s.note("nothing to undo")
		}
		return false
	}
	s.history = h
	if !silent {
		s.note(fmt.Sprintf("removed %d message(s)", n))
	}
	return true
}

// trimLastTurn cuts history back to just before the last user message,
// returning the cut history and how many messages were removed.
func trimLastTurn(h []any) ([]any, int) {
	for i := len(h) - 1; i >= 0; i-- {
		msg, ok := h[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "user" {
			return h[:i], len(h) - i
		}
	}
	return h, 0
}

// trimLastAnswer drops only the assistant/tool messages after the last user
// message, keeping that user message so it can be resent (/retry).
func trimLastAnswer(h []any) ([]any, int) {
	i := len(h)
	for i > 0 {
		msg, ok := h[i-1].(map[string]any)
		if !ok {
			i--
			continue
		}
		if role, _ := msg["role"].(string); role == "user" {
			break
		}
		i--
	}
	return h[:i], len(h) - i
}

// cmdRetry resends the last user message, dropping the answer it produced.
func (s *session) cmdRetry() {
	h, n := trimLastAnswer(s.history)
	s.history = h
	if len(s.history) == 0 {
		s.note("nothing to retry")
		return
	}
	last, _ := s.history[len(s.history)-1].(map[string]any)
	if role, _ := last["role"].(string); role != "user" {
		s.note("nothing to retry")
		return
	}
	if n > 0 {
		s.note(fmt.Sprintf("dropped %d message(s), resending", n))
	}
	s.runTurn()
}

func expandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func (s *session) cmdSave(arg string) {
	if arg == "" {
		s.err(errors.New("usage: /save <file>"))
		return
	}
	path := expandPath(arg)
	b, err := json.MarshalIndent(s.history, "", "  ")
	if err != nil {
		s.err(err)
		return
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		s.err(err)
		return
	}
	s.note(fmt.Sprintf("saved %d messages to %s", len(s.history), path))
}

func (s *session) cmdLoad(arg string) {
	if arg == "" {
		s.err(errors.New("usage: /load <file>"))
		return
	}
	path := expandPath(arg)
	b, err := os.ReadFile(path)
	if err != nil {
		s.err(err)
		return
	}
	var msgs []any
	if err := json.Unmarshal(b, &msgs); err != nil {
		s.err(fmt.Errorf("%s is not a conversation JSON array: %w", path, err))
		return
	}
	s.history = msgs
	s.note(fmt.Sprintf("loaded %d messages from %s", len(msgs), path))
}

// cmdCompact asks the model to summarise the conversation and replaces the
// history with the summary, keeping context small.
func (s *session) cmdCompact() {
	if len(s.history) == 0 {
		s.note("nothing to compact")
		return
	}
	msgs := make([]any, 0, len(s.history)+2)
	if s.cfg.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": s.cfg.System})
	}
	msgs = append(msgs, s.history...)
	msgs = append(msgs, map[string]any{"role": "user", "content": "Summarize the conversation above into a concise, factual summary: keep decisions, facts, file paths, commands and open questions. Reply with only the summary, no preamble."})
	summary, err := s.simpleComplete(msgs)
	if err != nil {
		s.err(err)
		return
	}
	n := len(s.history)
	s.history = []any{map[string]any{
		"role":    "user",
		"content": "[summary of the earlier conversation]\n" + summary,
	}}
	s.note(fmt.Sprintf("compacted %d messages into a summary (%d chars)", n, len(summary)))
}

func (s *session) cmdUsage() {
	if s.lastUsage == nil {
		s.note("no usage recorded yet")
		return
	}
	u := s.lastUsage
	s.note(fmt.Sprintf("last turn: %d prompt + %d completion = %d tokens", u.Prompt, u.Completion, u.Total))
}

// runLocal executes !<cmd> locally, prints the output, and appends it to the
// conversation so the next turn can reason about it.
func (s *session) runLocal(cmd string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		s.note("usage: !<shell command>")
		return
	}
	s.t.line(dim("  $ ") + cmd)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "bash", "-c", cmd).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		out = append(out, []byte("\n…[timed out after 30s]")...)
	}
	text := string(out)
	if len(text) > 8192 {
		text = text[:8192] + "\n…[truncated]"
	}
	if text != "" {
		s.t.line(dim("  │ ") + strings.ReplaceAll(text, "\n", "\n"+dim("  │ ")))
	}
	if err != nil {
		s.t.line(red("  exit: " + err.Error()))
	}
	s.history = append(s.history, map[string]any{
		"role":    "user",
		"content": fmt.Sprintf("I ran `%s` on my local machine. Output:\n```\n%s\n```", cmd, text),
	})
	s.note("command output added to the conversation context")
}
