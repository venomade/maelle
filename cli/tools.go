package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The client's OWN tools. Maelle sees these merged with her maelle_* tools by
// the proxy and decides where each runs; the descriptions say so. These all
// execute on the USER's machine.
var clientTools = []any{
	map[string]any{"type": "function", "function": map[string]any{
		"name":        "client_get_time",
		"description": "Get the current local time on the USER's machine (not Maelle's server).",
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
	}},
	map[string]any{"type": "function", "function": map[string]any{
		"name":        "client_read_local",
		"description": "Read a text file (max 8 KB) on the USER's machine.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "File path on the user's machine"},
		}, "required": []any{"path"}},
	}},
	map[string]any{"type": "function", "function": map[string]any{
		"name":        "client_list_local",
		"description": "List the entries of a directory on the USER's machine (directories get a trailing /).",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Directory path on the user's machine (default .)"},
		}, "required": []any{"path"}},
	}},
	map[string]any{"type": "function", "function": map[string]any{
		"name":        "client_search_local",
		"description": "Search file contents with a regex on the USER's machine (skips .git and node_modules, files over 1 MB). Returns path:line: text matches.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"pattern":     map[string]any{"type": "string", "description": "Regular expression to match"},
			"path":        map[string]any{"type": "string", "description": "File or directory to search (default .)"},
			"max_matches": map[string]any{"type": "number", "description": "Stop after this many matches (default 50)"},
		}, "required": []any{"pattern"}},
	}},
	map[string]any{"type": "function", "function": map[string]any{
		"name":        "client_write_local",
		"description": "Write a text file (max 64 KB) on the USER's machine, overwriting it. Use for small edits the user asked for.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "File path on the user's machine"},
			"content": map[string]any{"type": "string", "description": "Full new content of the file"},
		}, "required": []any{"path", "content"}},
	}},
	map[string]any{"type": "function", "function": map[string]any{
		"name":        "client_run_local",
		"description": "Run a shell command on the USER's machine (as the user, no sudo). Returns the exit code plus combined stdout/stderr. Times out after timeout_seconds (default 30, max 120).",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"command":         map[string]any{"type": "string", "description": "The shell command to run"},
			"timeout_seconds": map[string]any{"type": "number", "description": "Optional timeout, 1-120 seconds (default 30)"},
		}, "required": []any{"command"}},
	}},
}

func runClientTool(name, args string) string {
	var a map[string]any
	_ = json.Unmarshal([]byte(args), &a)
	var out string
	var err error
	switch name {
	case "client_get_time":
		out = time.Now().Format(time.RFC1123)
	case "client_read_local":
		out, err = toolReadLocal(a)
	case "client_list_local":
		out, err = toolListLocal(a)
	case "client_search_local":
		out, err = toolSearchLocal(a)
	case "client_write_local":
		out, err = toolWriteLocal(a)
	case "client_run_local":
		out, err = toolRunLocal(a)
	default:
		return "error: unknown tool " + name
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}

const (
	readLocalCap  = 8 * 1024
	dirLocalCap   = 512
	writeLocalCap = 64 * 1024
	runLocalCap   = 32 * 1024
)

func toolReadLocal(a map[string]any) (string, error) {
	p, _ := a["path"].(string)
	if p == "" {
		return "", errors.New("path required")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	if len(data) > readLocalCap {
		return string(data[:readLocalCap]) +
			fmt.Sprintf("\n…[truncated, file larger than %d bytes]", readLocalCap), nil
	}
	return string(data), nil
}

func toolListLocal(a map[string]any) (string, error) {
	p, _ := a["path"].(string)
	if p == "" {
		p = "."
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > dirLocalCap {
		return strings.Join(names[:dirLocalCap], "\n") +
			fmt.Sprintf("\n…[%d more entries]", len(names)-dirLocalCap), nil
	}
	return strings.Join(names, "\n"), nil
}

var errStopWalk = errors.New("stop walking")

func toolSearchLocal(a map[string]any) (string, error) {
	pattern, _ := a["pattern"].(string)
	if pattern == "" {
		return "", errors.New("pattern required")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("bad regex: %w", err)
	}
	root, _ := a["path"].(string)
	if root == "" {
		root = "."
	}
	maxMatches := 50
	if v, ok := a["max_matches"].(float64); ok && v > 0 {
		maxMatches = int(v)
	}
	var out []string
	count := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if count >= maxMatches {
			return errStopWalk
		}
		fi, err := d.Info()
		if err != nil || fi.Size() > 1<<20 {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				if len(line) > 200 {
					line = line[:200] + "…"
				}
				out = append(out, fmt.Sprintf("%s:%d: %s", p, i+1, line))
				count++
				if count >= maxMatches {
					return errStopWalk
				}
			}
		}
		return nil
	})
	if len(out) == 0 {
		return "no matches", nil
	}
	return strings.Join(out, "\n"), nil
}

func toolWriteLocal(a map[string]any) (string, error) {
	p, _ := a["path"].(string)
	if p == "" {
		return "", errors.New("path required")
	}
	content, _ := a["content"].(string)
	if len(content) > writeLocalCap {
		return "", fmt.Errorf("content too large: %d bytes (cap %d)", len(content), writeLocalCap)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), p), nil
}

func toolRunLocal(a map[string]any) (string, error) {
	cmd, _ := a["command"].(string)
	if cmd == "" {
		return "", errors.New("command required")
	}
	timeout := 30
	if v, ok := a["timeout_seconds"].(float64); ok && v >= 1 {
		timeout = int(v)
	}
	if timeout > 120 {
		timeout = 120
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "bash", "-c", cmd).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("error: timed out after %ds\n%s", timeout, string(out)), nil
	}
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			return "", err
		}
	}
	s := string(out)
	if len(s) > runLocalCap {
		s = s[:runLocalCap] + "\n…[truncated]"
	}
	return fmt.Sprintf("exit=%d\n%s", exit, s), nil
}
