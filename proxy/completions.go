// POST /v1/chat/completions — persona injection, tool merging, and the
// interception loop. The loop mirrors how agent-style clients work (Claude
// Code, Vibe): forward to Mistral, inspect the tool_calls that come back,
// execute the maelle_* ones here, report the runs to the client as
// [maelle:tool {...}] content markers, feed the results to the model, and
// call again — up to maxRounds. Client tool calls are relayed untouched
// (streamed live as tool_call deltas) and the turn ends with
// finish_reason "tool_calls" so the client runs its own tools and calls
// back; rewriteHistory (tools.go) converts the markers back into real
// history then, so the proxy stays stateless.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const maxRounds = 8

func chatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if !authorised(r) {
		deny(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key",
			"Incorrect API key provided — the Bearer token does not match MAELLE_PROXY_KEY")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		deny(w, http.StatusBadRequest, "invalid_request_error", "",
			"failed to read request body: "+err.Error())
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		deny(w, http.StatusBadRequest, "invalid_request_error", "",
			"request body is not valid JSON: "+err.Error())
		return
	}

	persona, err := os.ReadFile(personaPath)
	if err != nil {
		deny(w, http.StatusInternalServerError, "api_error", "",
			"persona file unreadable: "+err.Error())
		return
	}
	req["messages"] = injectPersona(req["messages"], string(persona))

	maelleTools, err := loadTools(toolsPath)
	if err != nil {
		deny(w, http.StatusInternalServerError, "api_error", "",
			"tools file unreadable: "+err.Error())
		return
	}
	mergeTools(req, maelleTools)

	messages, _ := req["messages"].([]any)
	req["messages"] = rewriteHistory(messages)

	if stream, _ := req["stream"].(bool); stream {
		streamLoop(w, req)
	} else {
		nonstreamLoop(w, req)
	}
	log.Printf("POST /v1/chat/completions (%s)", time.Since(start).Round(time.Millisecond))
}

func postUpstream(req map[string]any) (*http.Response, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequest(http.MethodPost, upstream+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Authorization", "Bearer "+mistralKey)
	hr.Header.Set("Content-Type", "application/json")
	// Streaming is relayed line-by-line; gzip would only add latency here.
	hr.Header.Set("Accept-Encoding", "identity")
	return httpClient.Do(hr)
}

func injectPersona(raw any, persona string) []any {
	messages, _ := raw.([]any)
	messages = append([]any{map[string]any{"role": "system", "content": persona}}, messages...)
	return messages
}

// mergeTools adds the server-side maelle_* definitions to the client's own
// tool list, so the model sees both and tool descriptions tell it where
// each runs. The client's field is preserved if it is not a list.
func mergeTools(req map[string]any, maelle []any) {
	client, ok := req["tools"].([]any)
	if !ok {
		req["tools"] = maelle
		return
	}
	seen := map[string]bool{}
	for _, t := range maelle {
		if m, _ := t.(map[string]any); m != nil {
			if fn, _ := m["function"].(map[string]any); fn != nil {
				if n, _ := fn["name"].(string); n != "" {
					seen[n] = true
				}
			}
		}
	}
	for _, t := range client {
		if m, _ := t.(map[string]any); m != nil {
			if fn, _ := m["function"].(map[string]any); fn != nil {
				if n, _ := fn["name"].(string); seen[n] {
					return // client already uses a maelle_ name; trust the client
				}
			}
		}
	}
	req["tools"] = append(maelle, client...)
}

func loadTools(path string) ([]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tools []any
	if err := json.Unmarshal(b, &tools); err != nil {
		return nil, err
	}
	return tools, nil
}

type usageStats struct{ prompt, completion, total int }

func (u *usageStats) add(raw any) {
	m, ok := raw.(map[string]any)
	if !ok {
		return
	}
	if v, ok := m["prompt_tokens"].(float64); ok {
		u.prompt += int(v)
	}
	if v, ok := m["completion_tokens"].(float64); ok {
		u.completion += int(v)
	}
	if v, ok := m["total_tokens"].(float64); ok {
		u.total += int(v)
	}
}

func (u usageStats) mapOut() map[string]any {
	return map[string]any{"prompt_tokens": u.prompt,
		"completion_tokens": u.completion, "total_tokens": u.total}
}

func randHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func isMaelleCall(c map[string]any) bool {
	fn, _ := c["function"].(map[string]any)
	name, _ := fn["name"].(string)
	return strings.HasPrefix(name, "maelle_")
}

func normCall(c map[string]any) map[string]any {
	fn, _ := c["function"].(map[string]any)
	name, _ := fn["name"].(string)
	args, _ := fn["arguments"].(string)
	id, _ := c["id"].(string)
	return map[string]any{"id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": args}}
}

// nonstreamLoop runs the interception rounds and returns one final
// chat.completion: content joined across rounds (markers included), only
// the client tool_calls in the final message, and usage summed over rounds.
func nonstreamLoop(w http.ResponseWriter, req map[string]any) {
	turnID := "chatcmpl-maelle-" + randHex()
	created, model := time.Now().Unix(), "unknown"
	var u usageStats
	var parts []string
	var clientCalls []any
	finish := "stop"
	completed := false

	for round := 0; round < maxRounds; round++ {
		resp, err := postUpstream(req)
		if err != nil {
			deny(w, http.StatusBadGateway, "api_error", "", "upstream unreachable")
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			if ct := resp.Header.Get("Content-Type"); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(resp.StatusCode)
			w.Write(b)
			return
		}
		var data map[string]any
		if err := json.Unmarshal(b, &data); err != nil {
			deny(w, http.StatusBadGateway, "api_error", "",
				"upstream returned invalid JSON")
			return
		}
		if m, _ := data["model"].(string); m != "" {
			model = m
		}
		if c, ok := data["created"].(float64); ok {
			created = int64(c)
		}
		u.add(data["usage"])
		choices, _ := data["choices"].([]any)
		if len(choices) == 0 {
			deny(w, http.StatusBadGateway, "api_error", "",
				"upstream returned no choices")
			return
		}
		choice, _ := choices[0].(map[string]any)
		msg, _ := choice["message"].(map[string]any)
		content, _ := msg["content"].(string)
		parts = append(parts, content)
		calls, _ := msg["tool_calls"].([]any)
		var maelle, client []any
		for _, c := range calls {
			if cm, ok := c.(map[string]any); ok {
				if isMaelleCall(cm) {
					maelle = append(maelle, cm)
				} else {
					client = append(client, cm)
				}
			}
		}

		if len(maelle) > 0 {
			// History gains the full assistant message (all its calls) plus
			// one tool result per executed maelle call. Client calls in a
			// mixed batch are collected for the final message; the round
			// still ends so the client can run them.
			var toolMsgs []any
			for _, c := range maelle {
				cm, _ := c.(map[string]any)
				fn, _ := cm["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				id, _ := cm["id"].(string)
				status, result := executeTool(name, args)
				parts = append(parts, encodeMarker(id, name, args, status, result))
				toolMsgs = append(toolMsgs, map[string]any{
					"role": "tool", "tool_call_id": id, "content": result,
				})
			}
			var normCalls []any
			for _, c := range calls {
				if cm, ok := c.(map[string]any); ok {
					normCalls = append(normCalls, normCall(cm))
				}
			}
			messages, _ := req["messages"].([]any)
			messages = append(messages, map[string]any{
				"role": "assistant", "content": content, "tool_calls": normCalls,
			})
			messages = append(messages, toolMsgs...)
			req["messages"] = messages
			if len(client) > 0 {
				for _, c := range client {
					if cm, ok := c.(map[string]any); ok {
						clientCalls = append(clientCalls, normCall(cm))
					}
				}
				finish = "tool_calls"
				completed = true
				break
			}
			continue
		}
		if len(client) > 0 {
			for _, c := range client {
				if cm, ok := c.(map[string]any); ok {
					clientCalls = append(clientCalls, normCall(cm))
				}
			}
			finish = "tool_calls"
			completed = true
			break
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			finish = fr
		}
		completed = true
		break
	}
	if !completed {
		parts = append(parts, "[maelle: note] round cap reached; giving my best answer so far")
	}

	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	message := map[string]any{"role": "assistant",
		"content": strings.Join(nonEmpty, "\n")}
	if len(clientCalls) > 0 {
		message["tool_calls"] = clientCalls
	}
	out := map[string]any{
		"id": turnID, "object": "chat.completion", "created": created, "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": finish,
		}},
		"usage": u.mapOut(),
	}
	b, _ := json.Marshal(out)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

// streamLoop is the streaming face of the same loop. Turn identity is
// rewritten to one stable id (each upstream round has its own), the role
// delta is forwarded only on round 0, and the finish chunk is emitted only
// when the whole agent turn ends. maelle_* tool_call deltas are eaten
// (SSE is additive — once sent, a delta cannot be unsent) and replaced by
// a content delta holding the marker; client tool_call deltas stream
// through live, buffered only until the function name is known.
func streamLoop(w http.ResponseWriter, req map[string]any) {
	includeUsage := false
	if so, ok := req["stream_options"].(map[string]any); ok {
		includeUsage, _ = so["include_usage"].(bool)
	}
	turnID := "chatcmpl-maelle-" + randHex()
	created, model := time.Now().Unix(), "unknown"
	var u usageStats
	headersSent := false
	roleSent := false

	flusher, _ := w.(http.Flusher)
	beginHeaders := func() {
		if headersSent {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		headersSent = true
	}
	emit := func(obj map[string]any) {
		b, err := json.Marshal(obj)
		if err != nil {
			return
		}
		w.Write(append(append([]byte("data: "), b...), '\n', '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk := func(delta map[string]any, finish any) {
		emit(map[string]any{
			"id": turnID, "object": "chat.completion.chunk",
			"created": created, "model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": delta, "finish_reason": finish,
			}},
		})
	}
	finishStream := func(reason string) {
		chunk(map[string]any{}, reason)
		if includeUsage {
			emit(map[string]any{
				"id": turnID, "object": "chat.completion.chunk",
				"created": created, "model": model,
				"choices": []any{}, "usage": u.mapOut(),
			})
		}
		w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	done := false
	for round := 0; round < maxRounds; round++ {
		resp, err := postUpstream(req)
		if err != nil {
			if headersSent {
				emit(map[string]any{"error": map[string]any{
					"message": "upstream unreachable", "type": "api_error",
					"param": nil, "code": nil,
				}})
				w.Write([]byte("data: [DONE]\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			} else {
				deny(w, http.StatusBadGateway, "api_error", "", "upstream unreachable")
			}
			return
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if headersSent {
				w.Write(append(append([]byte("data: "), b...), '\n', '\n'))
				w.Write([]byte("data: [DONE]\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			} else {
				if ct := resp.Header.Get("Content-Type"); ct != "" {
					w.Header().Set("Content-Type", ct)
				}
				w.WriteHeader(resp.StatusCode)
				w.Write(b)
			}
			return
		}
		beginHeaders()

		type tstate struct {
			id, name string
			args     strings.Builder
			client   bool
			classed  bool
			pending  []any
			flushed  bool
		}
		buffers := map[int]*tstate{}
		var finish string
		var contentParts []string

		reader := bufio.NewReaderSize(resp.Body, 64*1024)
		for {
			line, rerr := reader.ReadString('\n')
			line = strings.TrimRight(line, "\r\n")
			if line != "" && strings.HasPrefix(line, "data: ") {
				payload := strings.TrimSpace(line[len("data: "):])
				if payload == "[DONE]" {
					break
				}
				var data map[string]any
				if json.Unmarshal([]byte(payload), &data) == nil {
					choices, _ := data["choices"].([]any)
					if len(choices) == 0 {
						if data["usage"] != nil {
							u.add(data["usage"])
						}
					} else {
						if m, _ := data["model"].(string); m != "" {
							model = m
						}
						if c, ok := data["created"].(float64); ok {
							created = int64(c)
						}
						choice, _ := choices[0].(map[string]any)
						delta, _ := choice["delta"].(map[string]any)
						if role, ok := delta["role"].(string); ok && !roleSent && round == 0 {
							chunk(map[string]any{"role": role}, nil)
							roleSent = true
						}
						if c, ok := delta["content"].(string); ok && c != "" {
							contentParts = append(contentParts, c)
							chunk(map[string]any{"content": c}, nil)
						}
						if tcs, ok := delta["tool_calls"].([]any); ok {
							for _, raw := range tcs {
								tc, _ := raw.(map[string]any)
								idx := 0
								if f, ok := tc["index"].(float64); ok {
									idx = int(f)
								}
								st := buffers[idx]
								if st == nil {
									st = &tstate{}
									buffers[idx] = st
								}
								if id, ok := tc["id"].(string); ok && id != "" {
									st.id = id
								}
								if fn, ok := tc["function"].(map[string]any); ok {
									if n, ok := fn["name"].(string); ok && n != "" {
										st.name = n
										st.classed = true
										st.client = !strings.HasPrefix(n, "maelle_")
									}
									if a, ok := fn["arguments"].(string); ok {
										st.args.WriteString(a)
									}
								}
								if st.classed && st.client {
									// Flush name-less fragments that arrived
									// before classification, then relay live.
									if !st.flushed {
										for _, p := range st.pending {
											chunk(map[string]any{"tool_calls": []any{p}}, nil)
										}
										st.pending = nil
										st.flushed = true
									}
									chunk(map[string]any{"tool_calls": []any{tc}}, nil)
								} else {
									st.pending = append(st.pending, tc)
								}
							}
						}
						if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
							finish = fr
						}
					}
				}
			}
			if rerr != nil {
				break
			}
		}
		resp.Body.Close()

		idxs := make([]int, 0, len(buffers))
		for i := range buffers {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		var maelleIdx, clientIdx []int
		for _, i := range idxs {
			if buffers[i].client {
				clientIdx = append(clientIdx, i)
			} else {
				maelleIdx = append(maelleIdx, i)
			}
		}

		if len(maelleIdx) > 0 {
			contentText := strings.Join(contentParts, "")
			var normCalls []any
			for _, i := range idxs {
				st := buffers[i]
				id := st.id
				if id == "" {
					id = fmt.Sprintf("call_%d", i)
				}
				normCalls = append(normCalls, map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{
						"name": st.name, "arguments": st.args.String(),
					},
				})
			}
			var toolMsgs []any
			for _, i := range maelleIdx {
				st := buffers[i]
				id := st.id
				if id == "" {
					id = fmt.Sprintf("call_%d", i)
				}
				status, result := executeTool(st.name, st.args.String())
				chunk(map[string]any{"content": "\n" +
					encodeMarker(id, st.name, st.args.String(), status, result) + "\n"}, nil)
				toolMsgs = append(toolMsgs, map[string]any{
					"role": "tool", "tool_call_id": id, "content": result,
				})
			}
			messages, _ := req["messages"].([]any)
			messages = append(messages, map[string]any{
				"role": "assistant", "content": contentText, "tool_calls": normCalls,
			})
			messages = append(messages, toolMsgs...)
			req["messages"] = messages
		}

		// End the turn when the model stopped calling maelle tools: either
		// it wants the client to run something (finish "tool_calls"), or it
		// produced a plain answer (forward the upstream finish reason).
		if len(clientIdx) > 0 || len(maelleIdx) == 0 {
			reason := finish
			if len(clientIdx) > 0 {
				reason = "tool_calls"
			}
			if reason == "" {
				reason = "stop"
			}
			finishStream(reason)
			done = true
			break
		}
	}
	if !done {
		finishStream("stop")
	}
}
