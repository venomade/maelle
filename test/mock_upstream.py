"""Mock Mistral upstream for testing the maelle proxy contract.

Speaks OpenAI chat-completions shapes on purpose. Scenario routing is driven
by the last user message, so the same deterministic script exercises the
proxy's interception loop:

  serverfile  -> one maelle_read_file call, then narrates the real result
  double      -> two maelle calls in one batch (parallel tool calls)
  mixedwork   -> one maelle call + one client call in the same batch
  runcmd      -> one maelle_run_command call
  websearch   -> one maelle_web_search call (served by /search below)
  webfetch    -> one maelle_web_fetch call (served by /page below)
  webfetch404 -> one maelle_web_fetch call to a 404 URL
  clientonly  -> one client tool call (passthrough)
  (default)   -> echoes system prompt head + tool names (v1 assertions)

When history ends in tool messages, the "model" reports exactly what results
it received: RESULTS {"<call_id>": "<result>", ...} — machine-checkable
proof that the proxy executed / relayed / reconstructed everything.
"""
import json
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

KEY = "mock-mistral-key"


def error_body(msg, typ="invalid_request_error", code=None):
    return {"error": {"message": msg, "type": typ, "param": None, "code": code}}


def fn_call(cid, name, args):
    return {"id": cid, "type": "function",
            "function": {"name": name, "arguments": json.dumps(args)}}


def decide(msgs):
    """Return (content, tool_calls) for this round."""
    trailing = []
    for m in reversed(msgs):
        if m.get("role") == "tool":
            trailing.append(m)
        else:
            break
    trailing.reverse()
    if trailing:
        results = {m.get("tool_call_id"): m.get("content", "") for m in trailing}
        return "RESULTS " + json.dumps(results, separators=(",", ":")), None

    user = next((m.get("content", "") for m in reversed(msgs)
                 if m.get("role") == "user"), "")
    if "serverfile" in user:
        return "Let me read the server's hostname file.", [
            fn_call("call_srv", "maelle_read_file", {"path": "/etc/hostname"})]
    if "double" in user:
        return "Checking two things at once.", [
            fn_call("call_d1", "maelle_read_file", {"path": "/etc/hostname"}),
            fn_call("call_d2", "maelle_list_dir", {"path": "/etc"})]
    if "mixedwork" in user:
        return "I'll check the server file and your clock.", [
            fn_call("call_m1", "maelle_read_file", {"path": "/etc/hostname"}),
            fn_call("call_m2", "client_get_time", {})]
    if "runcmd" in user:
        return "Running the command now.", [
            fn_call("call_c1", "maelle_run_command",
                    {"command": "echo hello-from-maelle"})]
    if "webfetch404" in user:
        return "Fetching that page.", [
            fn_call("call_w4", "maelle_web_fetch",
                    {"url": "http://127.0.0.1:9901/missing"})]
    if "webfetch" in user:
        return "Fetching the page now.", [
            fn_call("call_wf", "maelle_web_fetch",
                    {"url": "http://127.0.0.1:9901/page"})]
    if "websearch" in user:
        return "Searching the web for that.", [
            fn_call("call_ws", "maelle_web_search", {"query": "maelle agent"})]
    if "clientonly" in user:
        return "What time is it on your machine?", [
            fn_call("call_co", "client_get_time", {})]

    # default: deterministic echo of what the proxy sent upstream
    sys_head = next((m.get("content", "") for m in msgs
                     if m.get("role") == "system"), "")[:22]
    tool_names = ",".join(
        t.get("function", {}).get("name", "?") for t in msgs[0].get("_tools", [])) \
        if False else None
    return None, None  # handled by caller


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def send_json(self, status, obj):
        data = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("X-Request-Id", "req_mock_123")
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/v1/models":
            self.send_json(200, {"object": "list", "data": [
                {"id": "mistral-small-latest", "object": "model",
                 "created": 1700000000, "owned_by": "mistral"},
            ]})
        elif self.path.startswith("/search"):
            # fake SearXNG JSON endpoint (MAELLE_SEARXNG_URL points here)
            q = urllib.parse.parse_qs(self.path.split("?", 1)[1]).get("q", [""])[0] \
                if "?" in self.path else ""
            self.send_json(200, {"query": q, "results": [
                {"title": "Maelle — the agent",
                 "url": "https://example.com/maelle",
                 "content": "Maelle is an agent that lives on Stefan's server."},
                {"title": "Maelle docs",
                 "url": "https://example.com/docs",
                 "content": "Maelle docs page with details."},
                {"title": "Agents overview",
                 "url": "https://example.org/agents",
                 "content": "An overview of agent architectures."},
            ]})
        elif self.path == "/page":
            body = (b"<!doctype html><html><head><title>Test Page</title>"
                    b"<script>var hidden-script = 1;</script>"
                    b"<style>.hidden-style { color: red }</style></head>"
                    b'<header><a href="#">site-chrome header-junk</a></header>'
                    b"<nav><ul><li>nav-chrome junk-link</li></ul></nav>"
                    b"<aside>sidebar-chrome junk-aside</aside>"
                    b"<body><!-- a comment -->"
                    b"<h1>Visible content</h1>"
                    b"<p>This is the real paragraph text.</p>"
                    b"<p>{{Multiple image|caption=junk-template|width=100}}</p>"
                    b"</body></html>")
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.path == "/missing":
            self.send_json(404, error_body("Not Found"))
        else:
            self.send_json(404, error_body(f"Invalid URL (GET {self.path})"))

    def do_POST(self):
        if self.path != "/v1/chat/completions":
            self.send_json(404, error_body(f"Invalid URL (POST {self.path})"))
            return
        if self.headers.get("Authorization") != f"Bearer {KEY}":
            self.send_json(401, error_body("Unauthorized"))
            return
        length = int(self.headers.get("Content-Length", 0))
        req = json.loads(self.rfile.read(length))
        msgs = req.get("messages", [])

        content, calls = decide(msgs)
        if content is None:
            # default echo: system head + tool names from the request body
            sys_head = next((m.get("content", "") for m in msgs
                             if m.get("role") == "system"), "")[:22]
            tool_names = ",".join(
                t.get("function", {}).get("name", "?") for t in req.get("tools", []))
            content = f"[mock reply] SYSTEM[{sys_head}...] TOOLS[{tool_names}]"

        cid, created = "chatcmpl-mock123", int(time.time())
        model = req.get("model", "unknown")
        if not req.get("stream"):
            msg = {"role": "assistant", "content": content}
            finish = "stop"
            if calls:
                msg["tool_calls"] = calls
                finish = "tool_calls"
            self.send_json(200, {
                "id": cid, "object": "chat.completion", "created": created,
                "model": model,
                "choices": [{"index": 0, "message": msg, "finish_reason": finish}],
                "usage": {"prompt_tokens": 10, "completion_tokens": 5,
                          "total_tokens": 15},
            })
            return

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("X-Request-Id", "req_mock_123")
        self.close_connection = True
        self.end_headers()

        def chunk(delta, finish=None, usage=None):
            obj = {"id": cid, "object": "chat.completion.chunk",
                   "created": created, "model": model,
                   "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
            if usage is not None:
                obj["usage"] = usage
                obj["choices"] = []
            self.wfile.write(b"data: " + json.dumps(obj).encode() + b"\n\n")

        chunk({"role": "assistant", "content": ""})
        chunk({"content": content})
        if calls:
            for i, tc in enumerate(calls):
                chunk({"tool_calls": [{
                    "index": i, "id": tc["id"], "type": "function",
                    "function": {"name": tc["function"]["name"],
                                 "arguments": tc["function"]["arguments"]}}]})
        chunk({}, finish="stop" if not calls else "tool_calls")
        if (req.get("stream_options") or {}).get("include_usage"):
            chunk({}, usage={"prompt_tokens": 10, "completion_tokens": 5,
                             "total_tokens": 15})
        self.wfile.write(b"data: [DONE]\n\n")


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 9901), Handler).serve_forever()