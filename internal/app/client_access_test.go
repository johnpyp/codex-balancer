package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestClientAccessDynamicPolicy(t *testing.T) {
	p := filepath.Join(t.TempDir(), "access.json")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"allowed_cidrs":["192.168.0.0/16","100.64.0.0/10","127.0.0.0/8","::1/128"]}`)
	h := clientAccessHandler(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	check := func(peer string, want int) {
		t.Helper()
		r := httptest.NewRequest("GET", "/backend-api/codex/models", nil)
		r.RemoteAddr = peer
		r.Header.Set("X-Forwarded-For", "192.168.1.1")
		r.Header.Set("Forwarded", "for=100.100.1.1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("peer %s: status %d, want %d", peer, w.Code, want)
		}
	}
	for _, peer := range []string{"192.168.0.1:1", "192.168.255.254:1", "100.64.0.1:1", "100.127.255.254:1", "127.0.0.1:1", "[::1]:1", "[::ffff:192.168.1.2]:1"} {
		check(peer, 204)
	}
	for _, peer := range []string{"192.169.0.1:1", "100.63.255.255:1", "100.128.0.1:1", "10.0.0.1:1", "8.8.8.8:1", "[fd00::1]:1", "invalid"} {
		check(peer, 403)
	}
	write(`{"allowed_cidrs":["10.0.0.0/8"]}`)
	check("10.0.0.1:1", 204)
	check("192.168.1.1:1", 403)
	write(`{"allowed_cidrs":[]}`)
	check("10.0.0.1:1", 403)
	for _, invalid := range []string{`{`, `{}`, `{"allowed_cidrs":["bogus"]}`} {
		write(invalid)
		check("10.0.0.1:1", 503)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	check("192.168.1.1:1", 503)
}
