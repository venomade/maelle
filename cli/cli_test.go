package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testTerm(buf *bytes.Buffer) *term { return &term{out: buf, width: 0} }

func TestJSONEnd(t *testing.T) {
	if got := jsonEnd(`{"a": "x}y"} rest`, 0); got != len(`{"a": "x}y"}`) {
		t.Fatalf("jsonEnd complete = %d, want %d", got, len(`{"a": "x}y"}`))
	}
	if got := jsonEnd(`{"a": `, 0); got != -1 {
		t.Fatalf("jsonEnd incomplete = %d, want -1", got)
	}
	if got := jsonEnd(`{"a": "esc\"}`+"}"+`}`, 0); got != -1 {
		t.Fatalf("jsonEnd escaped quote should stay inside the string, got %d", got)
	}
	if got := jsonEnd("no brace", 0); got != -1 {
		t.Fatalf("jsonEnd non-object = %d, want -1", got)
	}
}

func TestMarkerStreamSplitAcrossDeltas(t *testing.T) {
	var buf bytes.Buffer
	useColor = false
	tr := testTerm(&buf)
	ms := newMarkerStream(tr, newMdRenderer(tr), false)
	marker := `[maelle:tool {"id":"c1","name":"maelle_read_file","arguments":"{\"path\":\"/etc/hostname\"}","status":"ok","result":"myhost"}]`
	ms.feed("before " + marker[:20])
	ms.feed(marker[20:40])
	ms.feed(marker[40:] + " after")
	ms.flush()
	out := buf.String()
	for _, want := range []string{"before", "after", "maelle_read_file", "myhost", "┌─", "└─"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[maelle:tool") {
		t.Fatalf("raw marker leaked into pretty render:\n%s", out)
	}
}

func TestMarkerStreamRaw(t *testing.T) {
	var buf bytes.Buffer
	useColor = false
	tr := testTerm(&buf)
	ms := newMarkerStream(tr, newMdRenderer(tr), true)
	ms.feed(`text [maelle:tool {"id":"c1","name":"x","arguments":"{}","status":"ok","result":"r"}] end`)
	ms.flush()
	if !strings.Contains(buf.String(), "[maelle:tool ") {
		t.Fatalf("raw mode must show the marker verbatim, got:\n%s", buf.String())
	}
}

func TestMarkerStreamIncompleteAtFlush(t *testing.T) {
	var buf bytes.Buffer
	useColor = false
	tr := testTerm(&buf)
	ms := newMarkerStream(tr, newMdRenderer(tr), false)
	ms.feed("hello [maelle:tool {\"id\":\"x\"") // never completed
	ms.flush()
	if !strings.Contains(buf.String(), "[maelle:tool {\"id\":\"x\"") {
		t.Fatalf("incomplete marker must be flushed as literal text, got:\n%s", buf.String())
	}
}

func TestRenderInline(t *testing.T) {
	useColor = true
	defer func() { useColor = false }()
	out := renderInline("a **bold** b `code` c [txt](http://x) d *it*")
	for _, want := range []string{
		"\x1b[1mbold\x1b[0m", "\x1b[36mcode\x1b[0m", "\x1b[4mtxt\x1b[0m", "\x1b[3mit\x1b[0m",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("renderInline missing %q in %q", want, out)
		}
	}
}

func TestMdCodeFence(t *testing.T) {
	var buf bytes.Buffer
	useColor = false
	tr := testTerm(&buf)
	md := newMdRenderer(tr)
	md.feed("intro\n```go\nfmt.Println(1)\n```\noutro\n")
	md.flush()
	out := buf.String()
	if !strings.Contains(out, "┌─ go") || !strings.Contains(out, "│ fmt.Println(1)") ||
		!strings.Contains(out, "└") || !strings.Contains(out, "outro") {
		t.Fatalf("code fence rendering wrong:\n%s", out)
	}
}

func TestWrap(t *testing.T) {
	var buf bytes.Buffer
	useColor = false
	tr := &term{out: &buf, width: 10}
	tr.write("hello world this is long")
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected >=3 wrapped lines, got %d: %q", len(lines), buf.String())
	}
	for _, l := range lines {
		if len(l) > 10 {
			t.Fatalf("line exceeds width: %q", l)
		}
	}
}

func TestWrapSkipsANSI(t *testing.T) {
	var buf bytes.Buffer
	useColor = true
	defer func() { useColor = false }()
	tr := &term{out: &buf, width: 10}
	tr.write(bold("abcdefghij") + "xy")
	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		visible := strings.ReplaceAll(l, "\x1b[1m", "")
		visible = strings.ReplaceAll(visible, "\x1b[0m", "")
		if len(visible) > 10 {
			t.Fatalf("visible width exceeds limit: %q", l)
		}
	}
}

func TestTrimLastTurn(t *testing.T) {
	h := []any{
		map[string]any{"role": "user", "content": "one"},
		map[string]any{"role": "assistant", "content": "two"},
		map[string]any{"role": "user", "content": "three"},
		map[string]any{"role": "assistant", "content": "four"},
		map[string]any{"role": "tool", "tool_call_id": "c", "content": "r"},
	}
	got, n := trimLastTurn(h)
	if n != 3 || len(got) != 2 {
		t.Fatalf("trimLastTurn n=%d len=%d, want 3 and 2", n, len(got))
	}
	if got[1].(map[string]any)["content"] != "two" {
		t.Fatalf("wrong prefix kept: %v", got)
	}
	if _, n := trimLastTurn(nil); n != 0 {
		t.Fatalf("empty history should remove nothing, got %d", n)
	}
}

func TestTrimLastAnswer(t *testing.T) {
	h := []any{
		map[string]any{"role": "user", "content": "one"},
		map[string]any{"role": "assistant", "content": "two"},
		map[string]any{"role": "user", "content": "three"},
		map[string]any{"role": "assistant", "content": "four"},
		map[string]any{"role": "tool", "tool_call_id": "c", "content": "r"},
	}
	got, n := trimLastAnswer(h)
	if n != 2 || len(got) != 3 {
		t.Fatalf("trimLastAnswer n=%d len=%d, want 2 and 3", n, len(got))
	}
	if got[2].(map[string]any)["content"] != "three" {
		t.Fatalf("last user message must be kept: %v", got)
	}
	// history ending in a user message (e.g. after an error): nothing to drop
	got, n = trimLastAnswer(h[:3])
	if n != 0 || len(got) != 3 {
		t.Fatalf("trimLastAnswer on user-terminated history n=%d len=%d", n, len(got))
	}
}

func TestClientTools(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("hello maelle"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := runClientTool("client_read_local", `{"path":"`+p+`"}`); !strings.Contains(out, "hello maelle") {
		t.Fatalf("read_local: %q", out)
	}
	if out := runClientTool("client_list_local", `{"path":"`+dir+`"}`); !strings.Contains(out, "f.txt") {
		t.Fatalf("list_local: %q", out)
	}
	if out := runClientTool("client_search_local", `{"pattern":"hello","path":"`+dir+`"}`); !strings.Contains(out, "f.txt:1") {
		t.Fatalf("search_local: %q", out)
	}
	w := filepath.Join(dir, "w.txt")
	if out := runClientTool("client_write_local", `{"path":"`+w+`","content":"written"}`); !strings.Contains(out, "wrote") {
		t.Fatalf("write_local: %q", out)
	}
	if b, _ := os.ReadFile(w); string(b) != "written" {
		t.Fatalf("write_local content: %q", b)
	}
	if out := runClientTool("client_run_local", `{"command":"echo hi-123"}`); !strings.Contains(out, "hi-123") || !strings.Contains(out, "exit=0") {
		t.Fatalf("run_local: %q", out)
	}
	if out := runClientTool("client_run_local", `{"command":"exit 3"}`); !strings.Contains(out, "exit=3") {
		t.Fatalf("run_local exit code: %q", out)
	}
	if out := runClientTool("client_get_time", `{}`); out == "" {
		t.Fatal("get_time returned empty")
	}
	if out := runClientTool("nope", `{}`); !strings.Contains(out, "unknown tool") {
		t.Fatalf("unknown tool: %q", out)
	}
	if out := runClientTool("client_search_local", `{"pattern":"(","path":"`+dir+`"}`); !strings.Contains(out, "bad regex") {
		t.Fatalf("search_local bad regex: %q", out)
	}
}

func TestCommandsWithoutNetwork(t *testing.T) {
	os.Setenv("MAELLE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	defer os.Unsetenv("MAELLE_CONFIG")
	var buf bytes.Buffer
	useColor = false
	s := &session{t: testTerm(&buf), cfg: defaultConfig()}
	if s.handleCommand("/stream off") {
		t.Fatal("/stream off should not quit")
	}
	if s.cfg.Stream {
		t.Fatal("stream should be off")
	}
	s.handleCommand("/model test-model")
	if s.cfg.Model != "test-model" {
		t.Fatal("model not set")
	}
	s.handleCommand("/temp 0.7")
	if s.cfg.Temp == nil || *s.cfg.Temp != 0.7 {
		t.Fatal("temp not set")
	}
	buf.Reset()
	s.handleCommand("/temp banana")
	if !strings.Contains(buf.String(), "error") {
		t.Fatal("bad temp should produce an error")
	}
	s.handleCommand("/max-tokens 42")
	if s.cfg.MaxTokens == nil || *s.cfg.MaxTokens != 42 {
		t.Fatal("max-tokens not set")
	}
	s.handleCommand("/max-tokens off")
	if s.cfg.MaxTokens != nil {
		t.Fatal("max-tokens should be cleared")
	}
	s.handleCommand("/system hello")
	if s.cfg.System != "hello" {
		t.Fatal("system not set")
	}
	s.handleCommand("/reasoning high")
	if s.cfg.Reasoning != "high" {
		t.Fatal("reasoning not set")
	}
	body := s.requestBody()
	if body["reasoning_effort"] != "high" {
		t.Fatalf("requestBody missing reasoning_effort: %v", body)
	}
	buf.Reset()
	s.handleCommand("/reasoning ultra")
	if !strings.Contains(buf.String(), "error") {
		t.Fatal("bad reasoning level should error")
	}
	if s.cfg.Reasoning != "high" {
		t.Fatal("bad reasoning level should not change the setting")
	}
	s.handleCommand("/reasoning off")
	if s.cfg.Reasoning != "" {
		t.Fatal("reasoning should be cleared")
	}
	if _, ok := s.requestBody()["reasoning_effort"]; ok {
		t.Fatal("reasoning_effort should be absent when unset")
	}
	s.handleCommand("/raw")
	if !s.rawMarkers {
		t.Fatal("raw should be on")
	}
	s.handleCommand("/reset")
	if len(s.history) != 0 {
		t.Fatal("reset should clear history")
	}
	s.handleCommand("/undo")
	if !strings.Contains(buf.String(), "nothing to undo") {
		t.Fatal("undo on empty history should say so")
	}
	s.handleCommand("/nosuchcmd")
	if !strings.Contains(buf.String(), "unknown command") {
		t.Fatal("unknown command should warn")
	}
	if !s.handleCommand("/exit") {
		t.Fatal("/exit should quit")
	}
	// settings persisted to disk
	cfg := loadConfig()
	if cfg.Model != "test-model" || cfg.Stream || cfg.System != "hello" {
		t.Fatalf("config not persisted: %+v", cfg)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	os.Setenv("MAELLE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	defer os.Unsetenv("MAELLE_CONFIG")
	var buf bytes.Buffer
	useColor = false
	s := &session{t: testTerm(&buf), cfg: defaultConfig()}
	s.history = []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello"},
	}
	f := filepath.Join(t.TempDir(), "conv.json")
	s.handleCommand("/save " + f)
	s.handleCommand("/reset")
	if len(s.history) != 0 {
		t.Fatal("reset failed")
	}
	s.handleCommand("/load " + f)
	if len(s.history) != 2 || s.history[0].(map[string]any)["content"] != "hi" {
		t.Fatalf("load failed: %v", s.history)
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	if got := expandPath("~/x.json"); got != filepath.Join(home, "x.json") {
		t.Fatalf("expandPath ~/x.json = %q", got)
	}
	if got := expandPath("/abs/x"); got != "/abs/x" {
		t.Fatalf("expandPath /abs/x = %q", got)
	}
}

func TestEditorHistoryFile(t *testing.T) {
	hp := filepath.Join(t.TempDir(), "history")
	os.Setenv("MAELLE_HISTORY", hp)
	defer os.Unsetenv("MAELLE_HISTORY")
	var buf bytes.Buffer
	useColor = false
	tr := testTerm(&buf)
	tr.tty = false
	e := newEditor(tr)
	e.addHist("first")
	e.addHist("second\nline")
	e.addHist("second\nline") // duplicate tail is not re-added
	e2 := newEditor(tr)
	if len(e2.hist) != 2 {
		t.Fatalf("history reload: %v", e2.hist)
	}
	if e2.hist[1] != "second\nline" {
		t.Fatalf("multi-line entry not unescaped: %q", e2.hist[1])
	}
}
