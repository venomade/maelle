package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

type toolCall struct {
	ID        string
	Name      string
	Arguments string
}

type usage struct {
	Prompt     int
	Completion int
	Total      int
}

func parseUsage(raw map[string]any) *usage {
	g := func(k string) int {
		if v, ok := raw[k].(float64); ok {
			return int(v)
		}
		return 0
	}
	return &usage{Prompt: g("prompt_tokens"), Completion: g("completion_tokens"), Total: g("total_tokens")}
}

type chatResult struct {
	Content string
	Calls   []toolCall
	Usage   *usage
}

// No overall timeout: streams may run for minutes. Dial, TLS and the wait for
// the first response byte fail fast instead (same policy as the proxy).
var httpClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	},
}

func (s *session) newRequest(method, url string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// apiError renders a non-200 response as a Go error, preferring the
// OpenAI-shaped {"error":{"message":...}} body the proxy guarantees.
func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return fmt.Errorf("%s: %s", resp.Status, e.Error.Message)
	}
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
}

// requestBody builds the chat/completions payload from the current history
// plus the persisted settings. A client-side system prompt (if set) is
// prepended here, not stored in history; the proxy prepends Maelle's persona
// ahead of it.
func (s *session) requestBody() map[string]any {
	msgs := make([]any, 0, len(s.history)+1)
	if s.cfg.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": s.cfg.System})
	}
	msgs = append(msgs, s.history...)
	req := map[string]any{
		"model":    s.cfg.Model,
		"messages": msgs,
		"tools":    clientTools,
		"stream":   s.cfg.Stream,
	}
	if s.cfg.Stream {
		req["stream_options"] = map[string]any{"include_usage": true}
	}
	if s.cfg.Temp != nil {
		req["temperature"] = *s.cfg.Temp
	}
	if s.cfg.MaxTokens != nil {
		req["max_tokens"] = *s.cfg.MaxTokens
	}
	return req
}

// chatStream POSTs one streaming turn. onContent receives raw content deltas
// (markers included) for live rendering; the full content and the assembled
// client tool_calls are returned.
func (s *session) chatStream(onContent func(string)) (chatResult, error) {
	b, err := json.Marshal(s.requestBody())
	if err != nil {
		return chatResult{}, err
	}
	req, err := s.newRequest(http.MethodPost, s.baseURL+"/chat/completions", b)
	if err != nil {
		return chatResult{}, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := httpClient.Do(req)
	if err != nil {
		return chatResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return chatResult{}, apiError(resp)
	}

	var res chatResult
	type frag struct{ id, name, args string }
	frags := map[int]*frag{}
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
			if json.Unmarshal([]byte(payload), &data) != nil {
				continue
			}
			if e, ok := data["error"].(map[string]any); ok {
				if msg, _ := e["message"].(string); msg != "" {
					return res, fmt.Errorf("stream error: %s", msg)
				}
			}
			if u, ok := data["usage"].(map[string]any); ok {
				res.Usage = parseUsage(u)
			}
			choices, _ := data["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			choice, _ := choices[0].(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if c, ok := delta["content"].(string); ok && c != "" {
				res.Content += c
				if onContent != nil {
					onContent(c)
				}
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, raw := range tcs {
					tc, _ := raw.(map[string]any)
					idx := 0
					if f, ok := tc["index"].(float64); ok {
						idx = int(f)
					}
					fr := frags[idx]
					if fr == nil {
						fr = &frag{}
						frags[idx] = fr
					}
					if id, ok := tc["id"].(string); ok && id != "" {
						fr.id = id
					}
					if fn, ok := tc["function"].(map[string]any); ok {
						if n, ok := fn["name"].(string); ok && n != "" {
							fr.name = n
						}
						if a, ok := fn["arguments"].(string); ok {
							fr.args += a
						}
					}
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	idxs := make([]int, 0, len(frags))
	for i := range frags {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		fr := frags[i]
		res.Calls = append(res.Calls, toolCall{ID: fr.id, Name: fr.name, Arguments: fr.args})
	}
	return res, nil
}

// chatNonStream uses the proxy's non-streaming path and returns the whole
// message at once.
func (s *session) chatNonStream() (chatResult, error) {
	body := s.requestBody()
	body["stream"] = false
	delete(body, "stream_options")
	b, err := json.Marshal(body)
	if err != nil {
		return chatResult{}, err
	}
	req, err := s.newRequest(http.MethodPost, s.baseURL+"/chat/completions", b)
	if err != nil {
		return chatResult{}, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return chatResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return chatResult{}, apiError(resp)
	}
	var data struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return chatResult{}, fmt.Errorf("invalid JSON from proxy: %w", err)
	}
	if len(data.Choices) == 0 {
		return chatResult{}, fmt.Errorf("proxy returned no choices")
	}
	msg := data.Choices[0].Message
	res := chatResult{Content: msg.Content, Usage: parseUsage(data.Usage)}
	for _, c := range msg.ToolCalls {
		res.Calls = append(res.Calls, toolCall{ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	return res, nil
}

// simpleComplete is a one-shot non-streaming call without tools, used by
// /compact to summarise the conversation.
func (s *session) simpleComplete(messages []any) (string, error) {
	b, err := json.Marshal(map[string]any{
		"model": s.cfg.Model, "messages": messages, "stream": false,
	})
	if err != nil {
		return "", err
	}
	req, err := s.newRequest(http.MethodPost, s.baseURL+"/chat/completions", b)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp)
	}
	var data struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", fmt.Errorf("invalid JSON from proxy: %w", err)
	}
	if len(data.Choices) == 0 {
		return "", fmt.Errorf("proxy returned no choices")
	}
	return data.Choices[0].Message.Content, nil
}

// listModels calls GET /v1/models on the proxy.
func (s *session) listModels() ([]string, error) {
	req, err := s.newRequest(http.MethodGet, s.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("invalid JSON from /v1/models: %w", err)
	}
	ids := make([]string, 0, len(data.Data))
	for _, m := range data.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// rootURL strips a trailing /v1 so /healthz can be reached on the proxy root.
func (s *session) rootURL() string {
	return strings.TrimSuffix(strings.TrimSuffix(s.baseURL, "/v1"), "/")
}

// ping calls GET /healthz and returns the round-trip latency.
func (s *session) ping() (time.Duration, error) {
	req, err := s.newRequest(http.MethodGet, s.rootURL()+"/healthz", nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	d := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		return d, apiError(resp)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if strings.TrimSpace(string(b)) != "ok" {
		return d, fmt.Errorf("%s: unexpected body %q", resp.Status, strings.TrimSpace(string(b)))
	}
	return d, nil
}
