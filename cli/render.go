package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const markerPrefix = "[maelle:tool "

// toolMarker mirrors the proxy's [maelle:tool {...}] payload.
type toolMarker struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
	Result    string `json:"result"`
}

// ---------- color ----------

var useColor bool

// initColor decides once whether ANSI styling is enabled:
// MAELLE_COLOR=always|never wins, then NO_COLOR, then auto (tty + TERM).
func initColor() {
	switch strings.ToLower(os.Getenv("MAELLE_COLOR")) {
	case "always", "1", "true":
		useColor = true
	case "never", "0", "false":
		useColor = false
	default:
		useColor = os.Getenv("NO_COLOR") == "" &&
			os.Getenv("TERM") != "dumb" &&
			isCharDevice(os.Stdout)
	}
}

func paint(code, s string) string {
	if !useColor || s == "" {
		return s
	}
	return code + s + "\x1b[0m"
}

func bold(s string) string        { return paint("\x1b[1m", s) }
func dim(s string) string         { return paint("\x1b[2m", s) }
func italic(s string) string      { return paint("\x1b[3m", s) }
func red(s string) string         { return paint("\x1b[31m", s) }
func green(s string) string       { return paint("\x1b[32m", s) }
func yellow(s string) string      { return paint("\x1b[33m", s) }
func cyan(s string) string        { return paint("\x1b[36m", s) }
func magenta(s string) string     { return paint("\x1b[35m", s) }
func boldCyan(s string) string    { return paint("\x1b[1;36m", s) }
func boldMagenta(s string) string { return paint("\x1b[1;35m", s) }

// ---------- terminal writer ----------

// term serializes all REPL output and tracks the cursor column so streamed
// text can be wrapped to the terminal width. ANSI escapes pass through
// without affecting the column count.
type term struct {
	mu    sync.Mutex
	out   io.Writer
	raw   bool // terminal in raw mode: newlines must be emitted as \r\n
	tty   bool
	width int // 0 = no wrapping
	col   int
}

func newTerm(out io.Writer) *term {
	return &term{out: out, width: detectWidth()}
}

func (t *term) emit(s string) { io.WriteString(t.out, s) }

func (t *term) write(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\x1b': // ANSI escape sequence: pass through, zero width
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
					j++
				}
				if j < len(s) {
					j++
				}
			} else if j < len(s) {
				j++
			}
			t.emit(s[i:j])
			i = j
		case c == '\n':
			if t.raw {
				t.emit("\r\n")
			} else {
				t.emit("\n")
			}
			t.col = 0
			i++
		case c == '\r':
			t.emit("\r")
			t.col = 0
			i++
		case c == '\t':
			t.emit("\t")
			t.col = (t.col/8 + 1) * 8
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			w := runeWidth(r)
			if t.width > 0 && t.col > 0 && t.col+w > t.width {
				if t.raw {
					t.emit("\r\n")
				} else {
					t.emit("\n")
				}
				t.col = 0
			}
			t.emit(s[i : i+size])
			t.col += w
			i += size
		}
	}
}

func (t *term) print(s string)            { t.write(s) }
func (t *term) printf(f string, a ...any) { t.write(fmt.Sprintf(f, a...)) }
func (t *term) line(s string)             { t.write(s + "\n") }
func (t *term) linef(f string, a ...any)  { t.write(fmt.Sprintf(f, a...) + "\n") }

// runeWidth is a small wcwidth: 0 for control chars, 2 for the common East
// Asian wide ranges, 1 otherwise (box drawing, braille, arrows are narrow).
func runeWidth(r rune) int {
	switch {
	case r == 0, r < 32, r == 0x7f:
		return 0
	case r < 0x1100:
		return 1
	case r <= 0x115f, r == 0x2329, r == 0x232a,
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f,
		r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe30 && r <= 0xfe6f,
		r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x20000 && r <= 0x3fffd:
		return 2
	default:
		return 1
	}
}

// dispWidth is runeWidth over a string, skipping ANSI escape sequences.
func dispWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if s[i] == '\x1b' {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
					j++
				}
				if j < len(s) {
					j++
				}
			} else if j < len(s) {
				j++
			}
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w += runeWidth(r)
		i += size
	}
	return w
}

// ---------- markdown-lite renderer ----------

// mdRenderer renders assistant text line by line (streaming-safe: it buffers
// until a newline arrives). It understands fenced code blocks, headings,
// bullets and inline **bold**, *italic*, `code` and [text](url).
type mdRenderer struct {
	t      *term
	inCode bool
	pend   string
}

func newMdRenderer(t *term) *mdRenderer { return &mdRenderer{t: t} }

func (m *mdRenderer) feed(s string) {
	m.pend += s
	for {
		i := strings.IndexByte(m.pend, '\n')
		if i < 0 {
			return
		}
		line := m.pend[:i]
		m.pend = m.pend[i+1:]
		m.renderLine(line)
	}
}

func (m *mdRenderer) flush() {
	if m.pend != "" {
		m.renderLine(m.pend)
		m.pend = ""
	}
}

func (m *mdRenderer) renderLine(line string) {
	trimmed := strings.TrimSpace(line)
	if m.inCode {
		if strings.HasPrefix(trimmed, "```") {
			m.inCode = false
			m.t.line(dim("  └" + strings.Repeat("─", 24)))
			return
		}
		m.t.line(dim("  │ ") + line)
		return
	}
	if strings.HasPrefix(trimmed, "```") {
		m.inCode = true
		m.t.line(dim("  ┌─ " + strings.TrimSpace(trimmed[3:])))
		return
	}
	if n := len(line) - len(strings.TrimLeft(line, "#")); n > 0 && n <= 6 &&
		strings.HasPrefix(line[n:], " ") {
		m.t.line(bold(paint("\x1b[97m", strings.TrimSpace(line[n:]))))
		return
	}
	if strings.HasPrefix(trimmed, "- ") {
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		line = indent + "• " + trimmed[2:]
	}
	m.t.line(renderInline(line))
}

// renderInline applies inline markdown formatting, returning ANSI-styled
// text. Delimiters are ASCII so slicing never splits a UTF-8 rune.
func renderInline(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				b.WriteString(cyan(s[i+1 : i+1+j]))
				i += 1 + j + 1
				continue
			}
		case c == '*' && i+1 < len(s) && s[i+1] == '*':
			if j := strings.Index(s[i+2:], "**"); j > 0 {
				b.WriteString(bold(s[i+2 : i+2+j]))
				i += 2 + j + 2
				continue
			}
		case c == '*':
			if j := strings.IndexByte(s[i+1:], '*'); j > 0 {
				inner := s[i+1 : i+1+j]
				if !strings.HasPrefix(inner, " ") && !strings.HasSuffix(inner, " ") {
					b.WriteString(italic(inner))
					i += 1 + j + 1
					continue
				}
			}
		case c == '[':
			if close := strings.IndexByte(s[i:], ']'); close > 1 && i+close+1 < len(s) &&
				s[i+close+1] == '(' {
				if end := strings.IndexByte(s[i+close+2:], ')'); end >= 0 {
					text := s[i+1 : i+close]
					url := s[i+close+2 : i+close+2+end]
					b.WriteString(paint("\x1b[4m", text) + dim(" ("+url+")"))
					i += close + 2 + end + 1
					continue
				}
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// ---------- [maelle:tool {...}] marker stream ----------

// markerStream separates streamed content into plain text (fed to the
// markdown renderer) and complete [maelle:tool {...}] markers (rendered as
// activity boxes). It buffers partial markers across deltas, so a marker
// split over several SSE chunks still renders once, whole.
type markerStream struct {
	t   *term
	md  *mdRenderer
	buf string
	raw bool // raw mode: show markers verbatim instead of as boxes
}

func newMarkerStream(t *term, md *mdRenderer, raw bool) *markerStream {
	return &markerStream{t: t, md: md, raw: raw}
}

func (m *markerStream) feed(s string) {
	if m.raw {
		m.md.feed(s)
		return
	}
	m.buf += s
	for {
		i := strings.Index(m.buf, markerPrefix)
		if i < 0 {
			// Keep a tail that could still grow into a marker prefix.
			keep := len(markerPrefix) - 1
			if len(m.buf) > keep {
				m.md.feed(m.buf[:len(m.buf)-keep])
				m.buf = m.buf[len(m.buf)-keep:]
			}
			return
		}
		m.md.feed(m.buf[:i])
		m.buf = m.buf[i+len(markerPrefix):]
		j := jsonEnd(m.buf, 0)
		if j < 0 { // JSON incomplete — wait for more deltas
			m.buf = markerPrefix + m.buf
			return
		}
		end := j
		for end < len(m.buf) && m.buf[end] == ' ' {
			end++
		}
		if end >= len(m.buf) || m.buf[end] != ']' { // closing bracket not arrived yet
			m.buf = markerPrefix + m.buf
			return
		}
		end++
		var mk toolMarker
		if err := json.Unmarshal([]byte(m.buf[:j]), &mk); err != nil {
			// Not a well-formed marker after all: emit the prefix as text.
			m.md.feed(markerPrefix)
			continue
		}
		m.renderMarker(mk)
		m.buf = m.buf[end:]
	}
}

func (m *markerStream) flush() {
	m.md.feed(m.buf)
	m.buf = ""
	m.md.flush()
}

func (m *markerStream) renderMarker(mk toolMarker) {
	head := "  ┌─ " + cyan(mk.Name)
	if mk.Arguments != "" && mk.Arguments != "{}" {
		head += " " + dim(mk.Arguments)
	}
	m.t.line(head)
	status := mk.Status
	switch status {
	case "ok":
		status = green("ok")
	case "":
	default:
		status = red(status)
	}
	res := mk.Result
	const maxChars = 1500
	truncated := false
	if len(res) > maxChars {
		res = res[:maxChars]
		truncated = true
	}
	lines := strings.Split(res, "\n")
	// a trailing newline in the result should not render as an empty │ line
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	const maxLines = 10
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	if len(lines) == 1 && lines[0] == "" {
		m.t.line("  └─ " + status)
	} else {
		m.t.line("  └─ " + status + " " + lines[0])
		for _, l := range lines[1:] {
			m.t.line("    │ " + l)
		}
	}
	if truncated {
		m.t.line(dim("    … [truncated]"))
	}
}

// jsonEnd returns the offset just past the JSON object starting at s[start]
// ('{' expected, string-aware brace counting), or -1 if incomplete.
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

// ---------- spinner ----------

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinner animates a braille frame on the current line while waiting for the
// first token. TTY only — piped output stays clean.
type spinner struct {
	t    *term
	stop chan struct{}
	done chan struct{}
}

func startSpinner(t *term, label string) *spinner {
	if !t.tty {
		return nil
	}
	s := &spinner{t: t, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for i := 0; ; i++ {
			select {
			case <-s.stop:
				return
			case <-time.After(90 * time.Millisecond):
				s.t.write("\r\x1b[2K" + dim(spinFrames[i%len(spinFrames)]+" "+label))
			}
		}
	}()
	return s
}

func (s *spinner) stopNow() {
	if s == nil {
		return
	}
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
	s.t.write("\r\x1b[2K")
}

// preview collapses s to one short line for inline display.
func preview(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
