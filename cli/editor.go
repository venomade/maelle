package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// editor reads input lines. On a linux TTY it is a raw-mode single-line
// editor (arrows, kill keys, history navigation, tab completion); when stdin
// is piped it falls back to plain scanner lines so the REPL stays scriptable.
// A trailing backslash continues the input on the next line (multi-line).
type editor struct {
	t        *term
	raw      *rawState
	sc       *bufio.Scanner
	hist     []string
	histPath string
	hpos     int
	models   []string // cached by /models for tab completion
}

const histCap = 1000
const contPrompt = "  … "

func newEditor(t *term) *editor {
	e := &editor{t: t, histPath: historyPath()}
	e.sc = bufio.NewScanner(os.Stdin)
	e.sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	e.loadHist()
	if t.tty {
		if rs, err := makeRaw(os.Stdin); err == nil {
			e.raw = rs
			t.raw = true
		}
	}
	return e
}

func (e *editor) close() {
	if e.raw != nil {
		e.raw.restore(os.Stdin)
		e.raw = nil
	}
}

// readLine returns the next submitted line, or eof=true on Ctrl-D / EOF.
func (e *editor) readLine(prompt string) (string, bool) {
	acc := ""
	p := prompt
	for {
		var line string
		var status int // 0 ok, 1 eof, 2 interrupt (Ctrl-C)
		if e.raw != nil {
			line, status = e.readRawLine(p)
		} else {
			e.t.print(p)
			if e.sc.Scan() {
				line, status = e.sc.Text(), 0
			} else {
				status = 1
			}
		}
		if status == 1 {
			return "", true
		}
		if status == 2 {
			acc = ""
			p = prompt
			continue
		}
		if strings.HasSuffix(line, `\`) && !strings.HasSuffix(line, `\\`) {
			acc += line[:len(line)-1] + "\n"
			p = contPrompt
			continue
		}
		line = acc + line
		if strings.TrimSpace(line) != "" {
			e.addHist(line)
		}
		return line, false
	}
}

// readRawLine is the raw-mode single-line editor. Returns status 0 (line), 1
// (eof) or 2 (interrupted).
//
// Every input byte must advance the scan index exactly once: the cases below
// either return, continue with i set past what they consumed, or fall through
// to the single i++ at the bottom of the loop. (An earlier version forgot that
// for the edit keys — one backspace then spun on the same byte forever,
// erasing the whole line and wedging the REPL.)
func (e *editor) readRawLine(prompt string) (string, int) {
	var buf []rune
	cursor := 0
	redraw := func() {
		var b strings.Builder
		b.WriteString("\r\x1b[2K")
		b.WriteString(prompt)
		b.WriteString(string(buf))
		if tail := dispWidth(string(buf[cursor:])); tail > 0 {
			b.WriteString(fmt.Sprintf("\x1b[%dD", tail))
		}
		e.t.write(b.String())
	}
	redraw()
	var data [512]byte
	var pre []byte // escape-sequence / UTF-8 bytes split across reads
	for {
		n, err := os.Stdin.Read(data[:])
		if n == 0 {
			if err != nil {
				return "", 1
			}
			continue
		}
		d := data[:n]
		if len(pre) > 0 {
			d = append(pre, d...)
			pre = nil
		}
		i := 0
		for i < len(d) {
			c := d[i]
			switch {
			case c == '\r' || c == '\n': // Enter
				e.t.write("\n")
				return string(buf), 0
			case c == 3: // Ctrl-C: cancel the line
				e.t.write("^C\n")
				return "", 2
			case c == 4: // Ctrl-D: eof on empty line, else delete at cursor
				if len(buf) == 0 {
					e.t.write("\n")
					return "", 1
				}
				if cursor < len(buf) {
					buf = append(buf[:cursor], buf[cursor+1:]...)
					redraw()
				}
			case c == 127 || c == 8: // Backspace
				if cursor > 0 {
					buf = append(buf[:cursor-1], buf[cursor:]...)
					cursor--
					redraw()
				}
			case c == 1: // Ctrl-A: home
				cursor = 0
				redraw()
			case c == 5: // Ctrl-E: end
				cursor = len(buf)
				redraw()
			case c == 9: // Tab: completion
				e.complete(&buf, &cursor)
				redraw()
			case c == 11: // Ctrl-K: kill to end of line
				buf = buf[:cursor]
				redraw()
			case c == 21: // Ctrl-U: kill to start of line
				buf = buf[cursor:]
				cursor = 0
				redraw()
			case c == 23: // Ctrl-W: kill previous word
				for cursor > 0 && buf[cursor-1] == ' ' {
					cursor--
				}
				for cursor > 0 && buf[cursor-1] != ' ' {
					cursor--
				}
				buf = buf[:cursor]
				redraw()
			case c == 12: // Ctrl-L: clear screen
				e.t.write("\x1b[2J\x1b[H")
				redraw()
			case c == 27: // escape sequence (arrows, home/end, delete)
				if i+1 >= len(d) { // ESC is the last byte of the read: stash, wait for more
					pre = append([]byte(nil), d[i:]...)
					i = len(d)
					continue
				}
				if d[i+1] == '[' || d[i+1] == 'O' {
					j := i + 2
					start := j
					for j < len(d) && !(d[j] >= 0x40 && d[j] <= 0x7e) {
						j++
					}
					if j >= len(d) { // sequence split across reads: stash and wait
						pre = append([]byte(nil), d[i:]...)
						i = len(d)
						continue
					}
					seq := string(d[start:j])
					fin := d[j]
					i = j + 1
					switch {
					case fin == 'A': // up: older history
						if e.hpos > 0 {
							e.hpos--
							buf = []rune(e.hist[e.hpos])
							cursor = len(buf)
							redraw()
						}
					case fin == 'B': // down: newer history
						if e.hpos < len(e.hist) {
							e.hpos++
							if e.hpos == len(e.hist) {
								buf = nil
							} else {
								buf = []rune(e.hist[e.hpos])
							}
							cursor = len(buf)
							redraw()
						}
					case fin == 'C': // right
						if cursor < len(buf) {
							cursor++
							redraw()
						}
					case fin == 'D': // left
						if cursor > 0 {
							cursor--
							redraw()
						}
					case fin == 'H' || seq == "1~" || seq == "7~": // home
						cursor = 0
						redraw()
					case fin == 'F' || seq == "4~" || seq == "8~": // end
						cursor = len(buf)
						redraw()
					case fin == '~' && seq == "3": // delete at cursor
						if cursor < len(buf) {
							buf = append(buf[:cursor], buf[cursor+1:]...)
							redraw()
						}
					}
					continue
				}
				// ESC + other byte (e.g. Alt-x): skip the ESC, handle the next
			default:
				if c < 32 { // other control chars: ignore
					break
				}
				if c >= 0x80 && !utf8.FullRune(d[i:]) { // UTF-8 char split across reads
					pre = append([]byte(nil), d[i:]...)
					i = len(d)
					continue
				}
				r, size := utf8.DecodeRune(d[i:])
				buf = append(buf, 0)
				copy(buf[cursor+1:], buf[cursor:])
				buf[cursor] = r
				cursor++
				redraw()
				i += size
				continue
			}
			i++
		}
	}
}

// complete handles Tab: slash-command names, file paths after /save and
// /load, model ids after /model.
func (e *editor) complete(buf *[]rune, cursor *int) {
	line := string(*buf)
	setLine := func(l string) {
		*buf = []rune(l)
		*cursor = len(*buf)
	}
	if strings.HasPrefix(line, "/") && !strings.Contains(line, " ") {
		prefix := strings.TrimPrefix(line, "/")
		var matches []string
		for _, c := range commandNames {
			if strings.HasPrefix(c, prefix) && c != prefix {
				matches = append(matches, c)
			}
		}
		switch len(matches) {
		case 1:
			setLine("/" + matches[0] + " ")
		default:
			e.showOptions(matches)
		}
		return
	}
	if strings.HasPrefix(line, "/save ") || strings.HasPrefix(line, "/load ") {
		sp := strings.Index(line, " ")
		prefix := line[sp+1:]
		dir, base := filepath.Split(prefix)
		if dir == "" {
			dir = "."
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		var matches, names []string
		for _, en := range entries {
			if strings.HasPrefix(en.Name(), base) {
				name := en.Name()
				if en.IsDir() {
					name += string(filepath.Separator)
				}
				matches = append(matches, line[:sp+1]+filepath.Join(dir, name))
				names = append(names, name)
			}
		}
		sort.Strings(matches)
		if len(matches) == 1 {
			setLine(matches[0])
		} else if len(matches) > 1 {
			e.showOptions(names)
		}
		return
	}
	if strings.HasPrefix(line, "/model ") {
		prefix := strings.TrimPrefix(line, "/model ")
		var matches []string
		for _, m := range e.models {
			if strings.HasPrefix(m, prefix) && m != prefix {
				matches = append(matches, m)
			}
		}
		if len(matches) == 1 {
			setLine("/model " + matches[0])
		} else if len(matches) > 1 {
			e.showOptions(matches)
		}
	}
}

func (e *editor) showOptions(options []string) {
	if len(options) == 0 {
		return
	}
	e.t.write("\n")
	for _, o := range options {
		e.t.write("  " + o + "\n")
	}
}

func (e *editor) loadHist() {
	if e.histPath == "" {
		return
	}
	b, err := os.ReadFile(e.histPath)
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line != "" {
			e.hist = append(e.hist, strings.ReplaceAll(line, `\n`, "\n"))
		}
	}
	if len(e.hist) > histCap {
		e.hist = e.hist[len(e.hist)-histCap:]
	}
	e.hpos = len(e.hist)
}

func (e *editor) addHist(line string) {
	if len(e.hist) > 0 && e.hist[len(e.hist)-1] == line {
		e.hpos = len(e.hist)
		return
	}
	e.hist = append(e.hist, line)
	if len(e.hist) > histCap {
		e.hist = e.hist[len(e.hist)-histCap:]
	}
	e.hpos = len(e.hist)
	if e.histPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(e.histPath), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(e.histPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	// one entry per line; embedded newlines are escaped
	fmt.Fprintln(f, strings.ReplaceAll(line, "\n", `\n`))
}
