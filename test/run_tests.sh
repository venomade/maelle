#!/usr/bin/env bash
# Maelle proxy contract suite — OpenAI-compatible surface + tool interception.
#
# Terminal 1:  python3 test/mock_upstream.py
# Terminal 2:  cd proxy && MAELLE_UPSTREAM=http://127.0.0.1:9901 \
#                MISTRAL_API_KEY=mock-mistral-key MAELLE_PROXY_KEY=dev-secret \
#                MAELLE_TOOL_SUDO=0 go run .
# Then:        test/run_tests.sh
#
# Scenario keywords (mock): serverfile / double / mixedwork / runcmd /
# clientonly. When history ends in tool messages, the mock reports exactly
# what results it received (RESULTS {...}) — machine-checkable proof that
# the proxy executed, relayed and reconstructed everything correctly.
set -u
BASE="${BASE:-http://localhost:8080}"
KEY="${KEY:-dev-secret}"
pass=0; fail=0

check() {
  if [[ "$2" == "$3" ]]; then echo "PASS  $1"; pass=$((pass+1));
  else echo "FAIL  $1"; echo "      got:  $2"; echo "      want: $3"; fail=$((fail+1)); fi
}

chat() { # chat <key> <json> -> sets body / code
  local out
  out=$(curl -s -w $'\n%{http_code}' "$BASE/v1/chat/completions" \
    -H "Authorization: Bearer $1" -H "Content-Type: application/json" -d "$2")
  code=${out##*$'\n'}
  body=${out%$'\n'*}
}

chat_stream() { # chat_stream <key> <json> -> sets stream
  stream=$(curl -s -N "$BASE/v1/chat/completions" \
    -H "Authorization: Bearer $1" -H "Content-Type: application/json" -d "$2")
}

stream_content() { # content accumulated from SSE data frames
  grep '^data: {' <<<"$stream" | sed 's/^data: //' \
    | jq -sr 'map(.choices[0].delta.content // "") | join("")'
}

stream_client_calls() { # assembled client tool_calls (maelle_ ones never appear)
  grep '^data: {' <<<"$stream" | sed 's/^data: //' | jq -s '
    [ .[].choices[0].delta.tool_calls[]? ]
    | group_by(.index)
    | map({id: (map(.id // empty) | first),
           function: {name: (map(.function.name // empty) | first),
                      arguments: (map(.function.arguments // "") | join(""))}})
    | map(select((.function.name | startswith("maelle_")) | not))'
}

stream_finish() { # last non-null finish_reason in the stream
  grep '^data: {' <<<"$stream" | sed 's/^data: //' \
    | jq -r '.choices[0].finish_reason // empty' | tail -1
}

# ---------- 1. v1 surface (unchanged by interception) ----------

check "healthz responds" "$(curl -s "$BASE/healthz")" "ok"

chat wrong '{"model":"mistral-small-latest","messages":[{"role":"user","content":"hi"}]}'
check "bad key: status" "$code" "401"
check "bad key: error.type" "$(jq -r .error.type <<<"$body")" "authentication_error"
check "bad key: error.code" "$(jq -r .error.code <<<"$body")" "invalid_api_key"

chat "$KEY" '{"model":'
check "invalid JSON: status" "$code" "400"
check "invalid JSON: error.type" "$(jq -r .error.type <<<"$body")" "invalid_request_error"

out=$(curl -s -w $'\n%{http_code}' "$BASE/v1/nonexistent")
code=${out##*$'\n'}; body=${out%$'\n'*}
check "unknown route: status" "$code" "404"
check "unknown route: message" "$(jq -r .error.message <<<"$body")" "Invalid URL (GET /v1/nonexistent)"

hdr=$(curl -s -D - -o /dev/null -X GET "$BASE/v1/chat/completions" -H "Authorization: Bearer $KEY")
check "wrong method: status" "$(head -1 <<<"$hdr" | awk '{print $2}')" "405"
check "wrong method: Allow header" "$(grep -i '^allow:' <<<"$hdr" | tr -d '\r' | cut -d' ' -f2)" "POST"

chat "$KEY" '{"model":"mistral-small-latest","messages":[{"role":"user","content":"who are you?"}]}'
check "chat: status" "$code" "200"
check "chat: object" "$(jq -r .object <<<"$body")" "chat.completion"
check "chat: finish_reason" "$(jq -r '.choices[0].finish_reason' <<<"$body")" "stop"
check "chat: has usage" "$(jq '.usage | has("total_tokens")' <<<"$body")" "true"
check "chat: persona injected" "$(jq -r '.choices[0].message.content' <<<"$body" | grep -c 'SYSTEM\[You are Maelle, the ag')" "1"
check "chat: maelle tools merged" "$(jq -r '.choices[0].message.content' <<<"$body" | grep -c 'TOOLS\[maelle_read_file,maelle_list_dir,maelle_run_command\]')" "1"

hdr=$(curl -s -D - -o /dev/null "$BASE/v1/chat/completions" -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"mistral-small-latest","stream":true,"messages":[{"role":"user","content":"hi"}]}')
check "stream: content-type" "$(grep -i '^content-type:' <<<"$hdr" | tr -d '\r' | cut -d' ' -f2)" "text/event-stream"
chat_stream "$KEY" '{"model":"mistral-small-latest","stream":true,"messages":[{"role":"user","content":"hi"}]}'
check "stream: object chunk" "$(grep '^data: {' <<<"$stream" | head -1 | sed 's/^data: //' | jq -r .object)" "chat.completion.chunk"
check "stream: role delta first" "$(grep '^data: {' <<<"$stream" | head -1 | sed 's/^data: //' | jq -r '.choices[0].delta.role')" "assistant"
check "stream: [DONE] terminator" "$(tail -1 <<<"$stream")" "data: [DONE]"
check "stream: no usage by default" "$(grep -c '"usage"' <<<"$stream")" "0"

chat_stream "$KEY" '{"model":"mistral-small-latest","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}'
last=$(grep '^data: {' <<<"$stream" | tail -1 | sed 's/^data: //')
check "usage: total_tokens" "$(jq -r '.usage.total_tokens' <<<"$last")" "15"
check "usage: choices empty" "$(jq -c '.choices' <<<"$last")" "[]"

out=$(curl -s -w $'\n%{http_code}' "$BASE/v1/models" -H "Authorization: Bearer $KEY")
code=${out##*$'\n'}; body=${out%$'\n'*}
check "models: status" "$code" "200"
check "models: object list" "$(jq -r .object <<<"$body")" "list"
check "models: auth required" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/models")" "401"

# ---------- 2. interception: pure maelle_ tool call ----------

HN=$(cat /etc/hostname)
has() { grep -q "$2" <<<"$1" && echo found; }
chat "$KEY" '{"model":"mistral-small-latest","messages":[{"role":"user","content":"serverfile"}]}'
C=$(jq -r '.choices[0].message.content' <<<"$body")
check "srv/ns: status" "$code" "200"
check "srv/ns: marker emitted" "$(has "$C" 'maelle:tool ')" "found"
check "srv/ns: tool was maelle_read_file" "$(has "$C" '"name":"maelle_read_file"')" "found"
check "srv/ns: real file executed" "$(has "$C" "$HN")" "found"
check "srv/ns: result narrated by model" "$(has "$C" "RESULTS.*$HN")" "found"
check "srv/ns: no tool_calls to client" "$(jq '.choices[0].message | has("tool_calls")' <<<"$body")" "false"

chat_stream "$KEY" '{"model":"mistral-small-latest","stream":true,"messages":[{"role":"user","content":"serverfile"}]}'
content=$(stream_content)
check "srv/st: marker in stream" "$(echo "$content" | grep -c 'maelle:tool ')" "1"
check "srv/st: hostname result in stream" "$(echo "$content" | grep -c "RESULTS.*$HN")" "1"
check "srv/st: finish stop" "$(stream_finish)" "stop"

# ---------- 3. interception: multiple maelle calls in one batch ----------

chat "$KEY" '{"model":"mistral-small-latest","messages":[{"role":"user","content":"double"}]}'
C=$(jq -r '.choices[0].message.content' <<<"$body")
check "dbl/ns: two markers" "$(grep -c 'maelle:tool ' <<<"$C")" "2"
check "dbl/ns: both results" "$(grep -c "RESULTS" <<<"$C")" "1"
check "dbl/ns: hostname present" "$(has "$C" "$HN")" "found"
check "dbl/ns: /etc listing present" "$(has "$C" '"call_d2"')" "found"

# ---------- 4. interception: run_command ----------

chat "$KEY" '{"model":"mistral-small-latest","messages":[{"role":"user","content":"runcmd"}]}'
check "cmd/ns: exit 0" "$(has "$body" 'exit=0')" "found"
check "cmd/ns: command output" "$(has "$body" 'hello-from-maelle')" "found"

# ---------- 5. client tool calls pass through untouched ----------

chat "$KEY" '{"model":"mistral-small-latest","messages":[{"role":"user","content":"clientonly"}]}'
check "cli/ns: tool_calls relayed" "$(jq -r '.choices[0].message.tool_calls[0].function.name' <<<"$body")" "client_get_time"
check "cli/ns: finish_reason tool_calls" "$(jq -r '.choices[0].finish_reason' <<<"$body")" "tool_calls"
check "cli/ns: no markers" "$(jq -r '.choices[0].message.content' <<<"$body" | grep -c 'maelle:tool ')" "0"

# ---------- 6. mixed batch: execute + relay + round-trip ----------

chat "$KEY" '{"model":"mistral-small-latest","messages":[{"role":"user","content":"mixedwork"}]}'
check "mix/ns: marker for maelle part" "$(jq -r '.choices[0].message.content' <<<"$body" | grep -c '"name":"maelle_read_file"')" "1"
check "mix/ns: client call relayed" "$(jq -r '[.choices[0].message.tool_calls[].function.name] | join(",")' <<<"$body")" "client_get_time"
check "mix/ns: finish tool_calls" "$(jq -r '.choices[0].finish_reason' <<<"$body")" "tool_calls"

# simulate the client's round trip: it kept the assistant message (with
# markers in content, only client tool_calls), ran its tool, sends back
MIXREQ=$(jq -n --arg hn "$HN" '{model:"mistral-small-latest",messages:[
  {role:"user",content:"mixedwork"},
  {role:"assistant",content:("I'"'"'ll check the server file and your clock.\n[maelle:tool {\"id\":\"call_m1\",\"name\":\"maelle_read_file\",\"arguments\":\"{\\\"path\\\":\\\"/etc/hostname\\\"}\",\"status\":\"ok\",\"result\":\"'"$HN"'\"}]"),
   tool_calls:[{id:"call_m2",type:"function",function:{name:"client_get_time",arguments:"{}"}}]},
  {role:"tool",tool_call_id:"call_m2",content:"12:34"}]}')
chat "$KEY" "$MIXREQ"
check "mix/rt: both results returned to server" "$(jq -r '.choices[0].message.content' <<<"$body" | grep -c "RESULTS.*$HN.*12:34")" "1"
check "mix/rt: maelle call reconstructed" "$(jq -r '.choices[0].message.content' <<<"$body" | grep -c '"call_m1"')" "1"

# same flow, streaming: markers visible, client deltas standard, then replay
chat_stream "$KEY" '{"model":"mistral-small-latest","stream":true,"messages":[{"role":"user","content":"mixedwork"}]}'
content=$(stream_content)
check "mix/st: marker streamed" "$(echo "$content" | grep -c '"name":"maelle_read_file"')" "1"
check "mix/st: client deltas present" "$(stream_client_calls | jq -r '.[0].function.name')" "client_get_time"
check "mix/st: finish tool_calls" "$(stream_finish)" "tool_calls"

# client assembles the assistant message from the stream and round-trips
SA=$(jq -n --arg c "$content" \
  --argjson calls "$(stream_client_calls)" \
  '{role:"assistant",content:$c,tool_calls:$calls}')
MIXREQ2=$(jq -n --arg hn "$HN" --argjson sa "$SA" \
  '{model:"mistral-small-latest",messages:[
    {role:"user",content:"mixedwork"},
    $sa,
    {role:"tool",tool_call_id:"call_m2",content:"12:34"}]}')
chat "$KEY" "$MIXREQ2"
check "mix/str: reconstruction from stream round-trip" "$(jq -r '.choices[0].message.content' <<<"$body" | grep -c "RESULTS.*$HN.*12:34")" "1"

echo
echo "passed: $pass, failed: $fail"