package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestNativeLoginHTTPPreservesAffinityAndReplacesIdentity(t *testing.T) {
	s, p := testProviderHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token-a" || r.Header.Get("Chatgpt-Account-Id") != "a" {
			t.Error("native login was not replaced by route owner")
		}
		if r.Header.Get("X-Codex-Routing-Hint") != "model=gpt-6-astra" {
			t.Error("lost native routing header")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"output":[]}`)
	}), testAccount("a", 80), testAccount("client", 10))
	s.lookupAPIKey = nil // Explicit loopback-only -no-auth server mode.
	if err := s.pool.store.recordRoute(storedRoute{At: time.Now(), Session: "native-task", Account: "a"}); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"responses", "alpha/search", "guardian", "guardian-classifier"} {
		r := providerRequestForTest(t, "POST", p.URL+"/backend-api/codex/"+endpoint, []byte(`{"model":"gpt-6-astra","input":[],"session_id":"native-task"}`))
		r.Header.Set("Authorization", "Bearer native-secret")
		r.Header.Set("Chatgpt-Account-Id", "client")
		r.Header.Set("X-Codex-Routing-Hint", "model=gpt-6-astra")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s status %d", endpoint, resp.StatusCode)
		}
	}
	if owners, err := s.pool.store.routeOwners("", "native-task"); err != nil || len(owners) != 1 || owners[0] != "a" {
		t.Fatal("lost native task owner")
	}
	// Local access deliberately does not depend on any Codex login credential.
	for _, auth := range []string{"", "Bearer replaced-client-token"} {
		r, err := http.NewRequest("GET", p.URL+"/backend-api/codex/models", nil)
		if err != nil {
			t.Fatal(err)
		}
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("keyless models status %d", resp.StatusCode)
		}
	}
}

func TestNativeLoginWebSocketReplacesIdentity(t *testing.T) {
	s, p := testProviderHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token-a" || r.Header.Get("Chatgpt-Account-Id") != "a" {
			t.Error("incorrect upstream WebSocket identity")
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		if _, _, err = ws.Read(r.Context()); err != nil {
			return
		}
		ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created"}`))
		ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{}}`))
	}))
	s.lookupAPIKey = nil // Explicit loopback-only -no-auth server mode.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, strings.Replace(p.URL, "http:", "ws:", 1)+"/backend-api/codex/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer native-secret"}, "Chatgpt-Account-Id": {"client"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if err = ws.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-6-astra","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err = ws.Read(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
