// Maelle proxy — an OpenAI-compatible API gateway.
//
// Incoming requests are modified before forwarding to the Mistral API:
// Maelle's persona is prepended as the system prompt, and her server-side
// tools (maelle_*) are merged into the request's tool list. maelle_* tool
// calls that come back are intercepted and executed on this machine by
// completions.go; client tool calls pass through untouched. Stdlib only.
//
// Wire contract: OpenAI chat-completions shapes, both directions. Every
// failure is an OpenAI-shaped JSON error ({"error":{message,type,param,
// code}}) — including unknown routes and wrong methods — so an OpenAI SDK
// pointed at this proxy never sees a plain-text error page. Streaming replies
// are relayed as text/event-stream with a flush per chunk.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	listenAddr  = getenv("MAELLE_LISTEN", "127.0.0.1:8080")
	upstream    = getenv("MAELLE_UPSTREAM", "https://api.mistral.ai")
	personaPath = getenv("MAELLE_PERSONA", "persona.md")
	toolsPath   = getenv("MAELLE_TOOLS", "tools.json")
	mistralKey  = os.Getenv("MISTRAL_API_KEY")
	proxyKey    = os.Getenv("MAELLE_PROXY_KEY")
)

// No overall timeout: streams may run for minutes. The phases that should
// fail fast — dial, TLS, waiting for the first response byte — do.
var httpClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	},
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	if mistralKey == "" {
		log.Fatal("MISTRAL_API_KEY is not set (get one at https://console.mistral.ai)")
	}
	if proxyKey == "" {
		log.Fatal("MAELLE_PROXY_KEY is not set — pick a secret that clients must present")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	// Method-specific patterns handle the real calls; the method-less
	// duplicates turn wrong-method requests into OpenAI-shaped 405s instead
	// of Go's plain-text ones, and "/" turns every other path into a JSON
	// 404 with OpenAI's "Invalid URL" wording.
	mux.HandleFunc("POST /v1/chat/completions", chatCompletions)
	mux.HandleFunc("/v1/chat/completions", methodNotAllowed("POST"))
	mux.HandleFunc("GET /v1/models", models)
	mux.HandleFunc("/v1/models", methodNotAllowed("GET"))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		deny(w, http.StatusNotFound, "invalid_request_error", "",
			"Invalid URL ("+r.Method+" "+r.URL.Path+")")
	})

	log.Printf("maelle proxy listening on %s (upstream %s)", listenAddr, upstream)
	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

// authorised checks the Bearer token against MAELLE_PROXY_KEY in constant time.
func authorised(r *http.Request) bool {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	got := []byte(strings.TrimSpace(auth[len(prefix):]))
	want := []byte(proxyKey)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// deny writes an OpenAI-shaped error. An empty code marshals as null, like
// OpenAI's errors that have no specific code.
func deny(w http.ResponseWriter, status int, typ, code, msg string) {
	var codeField any
	if code != "" {
		codeField = code
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    typ,
			"param":   nil,
			"code":    codeField,
		},
	})
}

func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		deny(w, http.StatusMethodNotAllowed, "invalid_request_error", "",
			r.Method+" is not supported on "+r.URL.Path+" (use "+allow+")")
	}
}

func models(w http.ResponseWriter, r *http.Request) {
	if !authorised(r) {
		deny(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key",
			"Incorrect API key provided — the Bearer token does not match MAELLE_PROXY_KEY")
		return
	}
	upReq, err := http.NewRequest(http.MethodGet, upstream+"/v1/models", nil)
	if err != nil {
		deny(w, http.StatusInternalServerError, "api_error", "",
			"failed to build upstream request")
		return
	}
	upReq.Header.Set("Authorization", "Bearer "+mistralKey)

	resp, err := httpClient.Do(upReq)
	if err != nil {
		deny(w, http.StatusBadGateway, "api_error", "", "upstream unreachable")
		return
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
