#!/usr/bin/env bash
# Maelle CLI REPL integration suite.
#
# Boots the mock Mistral upstream + the real proxy, builds the CLI, then
# drives the REPL with scripted stdin (piped, so the plain non-TTY path is
# exercised) and asserts on the output. No jq — grep only.
#
#   test/run_cli_tests.sh            # uses ports 19901 (mock) / 18080 (proxy)
#   MOCK_PORT=29901 PROXY_PORT=28080 test/run_cli_tests.sh
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
MOCK_PORT="${MOCK_PORT:-19901}"
PROXY_PORT="${PROXY_PORT:-18080}"
MOCK_PID="" PROXY_PID=""
cleanup() {
  [[ -n "$MOCK_PID" ]] && kill "$MOCK_PID" 2>/dev/null
  [[ -n "$PROXY_PID" ]] && kill "$PROXY_PID" 2>/dev/null
  rm -rf "$TMP"
}
trap cleanup EXIT

pass=0; fail=0
check() { # check <name> <haystack> <fixed-string>
  if grep -qF -- "$3" <<<"$2"; then echo "PASS  $1"; pass=$((pass+1));
  else echo "FAIL  $1"; echo "      wanted: $3"; echo "      output:"; sed 's/^/      | /' <<<"$2" | head -50; fail=$((fail+1)); fi
}
check_not() {
  if grep -qF -- "$3" <<<"$2"; then echo "FAIL  $1 (found '$3')"; sed 's/^/      | /' <<<"$2" | head -50; fail=$((fail+1));
  else echo "PASS  $1"; pass=$((pass+1)); fi
}
check_rc() {
  if [[ "$2" == "$3" ]]; then echo "PASS  $1"; pass=$((pass+1));
  else echo "FAIL  $1 (got $2, want $3)"; fail=$((fail+1)); fi
}

echo "== gofmt / vet / unit tests =="
if [[ -z "$(gofmt -l "$ROOT/cli")" ]]; then echo "PASS  gofmt clean"; pass=$((pass+1));
else echo "FAIL  gofmt: $(gofmt -l "$ROOT/cli")"; fail=$((fail+1)); fi
if (cd "$ROOT/cli" && go vet ./... >/dev/null 2>&1); then echo "PASS  go vet"; pass=$((pass+1));
else echo "FAIL  go vet"; (cd "$ROOT/cli" && go vet ./...); fail=$((fail+1)); fi
if (cd "$ROOT/cli" && go test ./... >"$TMP/gotest.log" 2>&1); then echo "PASS  go test ./cli"; pass=$((pass+1));
else echo "FAIL  go test ./cli"; cat "$TMP/gotest.log"; fail=$((fail+1)); fi

echo "== build =="
(cd "$ROOT/cli" && go build -o "$TMP/maelle" .) || { echo "FAIL  cli build"; exit 1; }
(cd "$ROOT/proxy" && go build -o "$TMP/proxy" .) || { echo "FAIL  proxy build"; exit 1; }
echo "PASS  builds"

echo "== boot mock upstream + proxy =="
# the mock hardcodes port 9901 — run a copy with the port substituted
sed "s/9901/$MOCK_PORT/" "$ROOT/test/mock_upstream.py" > "$TMP/mock_upstream.py"
python3 "$TMP/mock_upstream.py" & MOCK_PID=$!
(cd "$ROOT/proxy" && MAELLE_UPSTREAM="http://127.0.0.1:$MOCK_PORT" \
  MISTRAL_API_KEY=mock-mistral-key MAELLE_PROXY_KEY=dev-secret \
  MAELLE_TOOL_SUDO=0 MAELLE_LISTEN="127.0.0.1:$PROXY_PORT" \
  "$TMP/proxy" >"$TMP/proxy.log" 2>&1) & PROXY_PID=$!

up=0
for _ in $(seq 1 50); do
  curl -sf "http://127.0.0.1:$PROXY_PORT/healthz" >/dev/null 2>&1 && { up=1; break; }
  sleep 0.2
done
[[ $up == 1 ]] || { echo "FAIL  proxy did not start"; exit 1; }
mockup=0
for _ in $(seq 1 50); do
  curl -sf "http://127.0.0.1:$MOCK_PORT/v1/models" -H "Authorization: Bearer mock-mistral-key" >/dev/null 2>&1 && { mockup=1; break; }
  sleep 0.2
done
[[ $mockup == 1 ]] || { echo "FAIL  mock did not start"; exit 1; }
echo "PASS  mock + proxy up"

BASE="http://127.0.0.1:$PROXY_PORT/v1"
run() { # run <home-name> <line...> — one REPL session, lines on stdin
  local name="$1"; shift
  printf '%s\n' "$@" | env -i HOME="$TMP/home-$name" PATH="$PATH" TERM=dumb \
    MAELLE_PROXY_KEY=dev-secret MAELLE_URL="$BASE" "$TMP/maelle" 2>&1
}

echo "== banner, help, exit codes =="
out=$(run basic /help /exit)
check "banner shows endpoint" "$out" "$BASE"
check "banner shows model" "$out" "mistral-small-latest"
check "help lists /models" "$out" "/models"
check "help lists /compact" "$out" "/compact"
check "help lists /retry" "$out" "/retry"
check "help lists ! shell" "$out" "!<cmd>"
check "help lists multiline" "$out" 'multi-line'
check "bye on /exit" "$out" "bye"

printf '/exit\n' | env -i HOME="$TMP/h1" PATH="$PATH" MAELLE_PROXY_KEY=dev-secret MAELLE_URL="$BASE" "$TMP/maelle" >/dev/null 2>&1
check_rc "exit code 0 on /exit" "$?" "0"
printf '' | env -i HOME="$TMP/h2" PATH="$PATH" MAELLE_PROXY_KEY=dev-secret MAELLE_URL="$BASE" "$TMP/maelle" >/dev/null 2>&1
check_rc "exit code 0 on EOF" "$?" "0"
out=$(env -i PATH="$PATH" "$TMP/maelle" </dev/null 2>&1); rc=$?
check "missing key message" "$out" "MAELLE_PROXY_KEY"
check_rc "exit code 1 without key" "$rc" "1"

echo "== api commands: /ping /models =="
out=$(run api /ping /models /exit)
check "ping ok" "$out" "healthz"
check "models count" "$out" "1 models"
check "models lists id" "$out" "mistral-small-latest"
check "models marks current" "$out" "(current)"

echo "== streaming chat with server-side tool (marker rendering) =="
HN=$(cat /etc/hostname)
out=$(run chat serverfile /exit)
check "model narrates" "$out" "Let me read the server"
check "marker box rendered" "$out" "┌─ maelle_read_file"
check "marker status ok" "$out" "└─ ok"
check "real file content" "$out" "$HN"
check "second round RESULTS" "$out" "RESULTS"
check "usage footer" "$out" "tok ("
check "footer shows model" "$out" "mistral-small-latest"

echo "== client tool passthrough (CLI executes locally, round-trips) =="
out=$(run cli2 clientonly /exit)
check "client tool announced" "$out" "client_get_time"
check "client tool result line" "$out" "└─ "
check "RESULTS round trip" "$out" "RESULTS"

echo "== non-streaming mode =="
out=$(run ns '/stream off' serverfile /exit)
check "stream toggled" "$out" "stream off"
check "non-stream marker" "$out" "maelle_read_file"
check "non-stream RESULTS" "$out" "RESULTS"

echo "== model switch + config persistence =="
out=$(run mdl '/model other-model' /model /exit)
check "model set note" "$out" "model set to other-model"
check "model shown" "$out" "model: other-model"
out=$(run mdl /exit)
check "model persisted across runs" "$out" "other-model"

echo "== settings commands =="
out=$(run st /stream '/temp 0.5' /temp '/max-tokens 100' /max-tokens '/system hello-there' /exit)
check "stream state shown" "$out" "stream on"
check "temp set" "$out" "temperature 0.5"
check "temp shown" "$out" "temperature 0.5"
check "max-tokens set" "$out" "max_tokens 100"
check "system set" "$out" "client system prompt set"
out=$(run tp '/temp banana' /exit)
check "bad temp errors" "$out" "error"

echo "== history / undo / retry / save / load =="
out=$(run hist zebra-uniq-1 /history /exit)
check "history shows user msg" "$out" "you ❯"
check "history shows zebra" "$out" "zebra-uniq-1"
out=$(run und zebra-uniq-2 /undo /history /exit)
check "undo note" "$out" "removed 2 message(s)"
check_not "undone msg gone from /history" "$out" "you ❯"
out=$(run ret serverfile /retry /exit)
n=$(grep -c "RESULTS" <<<"$out")
check_rc "retry re-ran the turn (2 RESULT rounds)" "$n" "2"
f="$TMP/conv.json"
out=$(run sl serverfile "/save $f" /reset "/load $f" /history /exit)
check "save note" "$out" "saved 2 messages"
check "reset note" "$out" "conversation cleared"
check "load note" "$out" "loaded 2 messages"
check "history restored" "$out" "you ❯"
[[ -f "$f" ]] && { echo "PASS  save file exists"; pass=$((pass+1)); } || { echo "FAIL  save file missing"; fail=$((fail+1)); }

echo "== local shell (!cmd) =="
out=$(run sh '!echo hello-local-42' /history /exit)
check "shell output shown" "$out" "hello-local-42"
check "context note" "$out" "added to the conversation context"
check "cmd in history" "$out" "echo hello-local-42"

echo "== multi-line input (trailing backslash) =="
out=$(run ml 'first-line\' 'second-line' /history /exit)
check "continuation joined" "$out" "second-line"

echo "== /raw marker toggle =="
out=$(run raw1 /raw serverfile /exit)
check "raw toggle note" "$out" "raw markers on"
check "raw marker verbatim" "$out" "[maelle:tool "

echo "== /compact =="
out=$(run comp zebra-compact-9 /compact /history /exit)
check "compact note" "$out" "compacted 2 messages"
check "summary wrapper" "$out" "summary of the earlier conversation"

echo "== /usage =="
out=$(run usg serverfile /usage /exit)
check "usage command" "$out" "last turn:"

echo "== /tools =="
out=$(run tl /tools /exit)
check "tools lists client_run_local" "$out" "client_run_local"
check "tools lists client_search_local" "$out" "client_search_local"
check "tools mentions maelle_*" "$out" "maelle_*"

echo "== auth failure surfaces, REPL survives =="
out=$(printf 'hi\n/exit\n' | env -i HOME="$TMP/hbad" PATH="$PATH" TERM=dumb \
  MAELLE_PROXY_KEY=wrong-key MAELLE_URL="$BASE" "$TMP/maelle" 2>&1)
check "401 surfaced" "$out" "401"
check "REPL still exits cleanly" "$out" "bye"

echo "== color control =="
out=$(printf '/exit\n' | env -i HOME="$TMP/hcol" PATH="$PATH" MAELLE_COLOR=always \
  MAELLE_PROXY_KEY=dev-secret MAELLE_URL="$BASE" "$TMP/maelle" 2>&1)
check "ANSI when MAELLE_COLOR=always" "$out" "$(printf '\033')"
out=$(printf '/exit\n' | env -i HOME="$TMP/hnocol" PATH="$PATH" MAELLE_COLOR=never \
  MAELLE_PROXY_KEY=dev-secret MAELLE_URL="$BASE" "$TMP/maelle" 2>&1)
check_not "no ANSI when MAELLE_COLOR=never" "$out" "$(printf '\033')"

echo
echo "passed: $pass, failed: $fail"
[[ $fail == 0 ]]
