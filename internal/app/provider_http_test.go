package app

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/klauspost/compress/zstd"
)

func testProviderHTTP(t *testing.T, upstream http.Handler, accounts ...*Account) (*server, *httptest.Server) {
	t.Helper()
	if len(accounts) == 0 {
		accounts = []*Account{testAccount("a", 10)}
	}
	s := newTestServer(t, accounts)
	s.lookupAPIKey = func(key string) (string, bool, error) { return "test", key == "local-secret", nil }
	u := httptest.NewServer(upstream)
	t.Cleanup(u.Close)
	s.upstream = u.URL + "/backend-api/codex"
	p := httptest.NewServer(s.routes())
	t.Cleanup(p.Close)
	return s, p
}

func providerRequestForTest(t *testing.T, method, endpoint string, body []byte) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer local-secret")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestProviderHTTPPathsAndExactBodies(t *testing.T) {
	paths := []string{"responses", "responses/compact", "alpha/search", "images/generations", "images/edits", "memories/trace_summarize", "guardian", "guardian-classifier", "realtime/calls", "live", "future/tool"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			body := []byte(`{"model":"gpt-6-astra","unknown":{"a":[1,2]},"input":[],"session_id":"session-a"}`)
			_, p := testProviderHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/backend-api/codex/"+path || r.URL.RawQuery != "q=a%2Bb&q=two" {
					t.Errorf("unexpected URL %s", r.URL)
				}
				b, _ := io.ReadAll(r.Body)
				if !bytes.Equal(b, body) {
					t.Errorf("body changed: %s", b)
				}
				if r.Header.Get("Authorization") != "Bearer token-a" || r.Header.Get("Chatgpt-Account-Id") != "a" {
					t.Errorf("wrong upstream identity")
				}
				for _, h := range []string{"Cookie", "X-Api-Key", "X-Leak", "Proxy-Authorization"} {
					if r.Header.Get(h) != "" {
						t.Errorf("leaked %s", h)
					}
				}
				if r.Header.Get(codexTurnStateKey) != "opaque-state" || r.Header.Get("X-Future-Feature") != "preserve" {
					t.Error("lost provider headers")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Upstream-Test", "present")
				w.Header().Set("Set-Cookie", "secret-cookie")
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{"future":"unchanged"}`)
			}))
			r := providerRequestForTest(t, "POST", p.URL+"/v1/"+path+"?q=a%2Bb&q=two", body)
			r.Header.Set("Cookie", "secret")
			r.Header.Set("X-Api-Key", "local-secret")
			r.Header.Set("Proxy-Authorization", "secret")
			r.Header.Set("Connection", "X-Leak")
			r.Header.Set("X-Leak", "secret")
			r.Header.Set(codexTurnStateKey, "opaque-state")
			r.Header.Set("X-Future-Feature", "preserve")
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 201 || string(b) != `{"future":"unchanged"}` || resp.Header.Get("X-Upstream-Test") != "present" || resp.Header.Get("Set-Cookie") != "" {
				t.Fatalf("response changed: %d %s %v", resp.StatusCode, b, resp.Header)
			}
		})
	}
}

func TestProviderHTTPCompressedAffinity(t *testing.T) {
	for _, encoding := range []string{"gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			raw := []byte(`{"id":"search-session","model":"gpt-6-astra","commands":{"search_query":[{"q":"test"}]}}`)
			var body bytes.Buffer
			if encoding == "gzip" {
				z := gzip.NewWriter(&body)
				z.Write(raw)
				z.Close()
			} else {
				z, _ := zstd.NewWriter(&body)
				z.Write(raw)
				z.Close()
			}
			s, p := testProviderHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Chatgpt-Account-Id") != "a" {
					t.Error("search lost session owner")
				}
				b, _ := io.ReadAll(r.Body)
				if !bytes.Equal(b, body.Bytes()) || r.Header.Get("Content-Encoding") != encoding {
					t.Error("compressed bytes changed")
				}
				io.WriteString(w, `{"output":[]}`)
			}), testAccount("a", 90), testAccount("b", 5))
			if err := s.pool.store.recordRoute(storedRoute{At: time.Now(), Session: "search-session", Account: "a"}); err != nil {
				t.Fatal(err)
			}
			r := providerRequestForTest(t, "POST", p.URL+"/v1/alpha/search", body.Bytes())
			r.Header.Set("Content-Encoding", encoding)
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("%d %s", resp.StatusCode, b)
			}
		})
	}
}

func TestProviderHTTPRetainsOrRejectsOwner(t *testing.T) {
	for _, spent := range []bool{false, true} {
		t.Run(map[bool]string{false: "retain", true: "spent"}[spent], func(t *testing.T) {
			a := testAccount("a", 90)
			setTestAccountSpent(a, spent)
			var calls atomic.Int32
			s, p := testProviderHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Chatgpt-Account-Id") != "a" {
					t.Error("account switched")
				}
				io.WriteString(w, `{}`)
			}), a, testAccount("b", 5))
			s.pool.store.recordRoute(storedRoute{At: time.Now(), Thread: "old", Session: "old", Account: "a"})
			r := providerRequestForTest(t, "POST", p.URL+"/v1/responses/compact", []byte(`{"model":"gpt-6-astra","input":[{"type":"reasoning","encrypted_content":"bound"}]}`))
			r.Header.Set("Thread-Id", "old")
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if !spent && resp.StatusCode != 200 {
				t.Fatalf("retained owner rejected: %d", resp.StatusCode)
			}
			if spent && (resp.StatusCode != 503 || calls.Load() != 0) {
				t.Fatalf("spent owner replayed: %d %d", resp.StatusCode, calls.Load())
			}
			owners, _ := s.pool.store.routeOwners("old", "")
			if len(owners) != 1 || owners[0] != "a" {
				t.Fatalf("affinity changed %v", owners)
			}
		})
	}
}

func TestProviderHTTPStreamFlushTrailersAndUsage(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s, p := testProviderHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Trailer", "X-Final")
		io.WriteString(w, "event: future\ndata: {\"type\":\"future.event\",\"opaque\":true}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-6-astra\",\"usage\":{\"input_tokens\":12,\"output_tokens\":3}}}\n\n")
		w.Header().Set("X-Final", "done")
	}))
	r := providerRequestForTest(t, "POST", p.URL+"/v1/responses", []byte(`{"model":"gpt-6-astra"}`))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(r.WithContext(ctx))
	if err != nil {
		t.Fatal("stream was buffered", err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "event: future\n" {
		t.Fatalf("first delta: %q %v", first, err)
	}
	release <- struct{}{}
	b, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "future.event") || resp.Trailer.Get("X-Final") != "done" {
		t.Fatalf("stream/trailer changed %s %v", b, resp.Trailer)
	}
	s.stats.mu.Lock()
	usage := s.stats.monthlyUsage
	s.stats.mu.Unlock()
	if usage.InputTokens != 12 || usage.OutputTokens != 3 {
		t.Fatalf("usage %+v", usage)
	}
}

func TestProviderHTTPCancellationReachesUpstream(t *testing.T) {
	canceled := make(chan struct{})
	_, p := testProviderHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": ready\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := providerRequestForTest(t, "POST", p.URL+"/v1/responses", []byte(`{}`)).WithContext(ctx)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream was left running")
	}
}

func TestProviderHTTPFailuresAreNotReplayed(t *testing.T) {
	for _, status := range []int{302, 400, 404, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var count atomic.Int32
			_, p := testProviderHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				w.Header().Set("Location", "https://example.invalid/")
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				io.WriteString(w, "original error")
			}), testAccount("a", 10), testAccount("b", 20))
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := client.Do(providerRequestForTest(t, "POST", p.URL+"/v1/alpha/search", []byte(`{}`)))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != status || string(b) != "original error" || count.Load() != 1 || resp.Header.Get("Retry-After") != "7" {
				t.Fatalf("failure changed/replayed %d %s %d", resp.StatusCode, b, count.Load())
			}
		})
	}
}

func TestProviderHTTPRefreshRetriesSameAccountOnce(t *testing.T) {
	refreshes := useOAuthRefreshServer(t)
	var calls atomic.Int32
	_, p := testProviderHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Chatgpt-Account-Id") != "a" {
			t.Error("401 switched accounts")
		}
		if r.Header.Get("Authorization") == "Bearer token-a" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Authorization") != "Bearer refreshed-token" {
			t.Error("refresh not applied")
		}
		io.WriteString(w, `{}`)
	}), testAccount("a", 5), testAccount("b", 20))
	resp, err := http.DefaultClient.Do(providerRequestForTest(t, "POST", p.URL+"/v1/alpha/search", []byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || calls.Load() != 2 || refreshes() != 1 {
		t.Fatalf("refresh result %d %d %d", resp.StatusCode, calls.Load(), refreshes())
	}
}

func TestProviderHTTPGenericUpgrade(t *testing.T) {
	_, p := testProviderHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/future/socket" {
			t.Error(r.URL.Path)
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		kind, b, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		ws.Write(r.Context(), kind, b)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w, _, err := websocket.Dial(ctx, strings.Replace(p.URL, "http:", "ws:", 1)+"/v1/future/socket", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer local-secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.CloseNow()
	data := []byte{0, 1, 2, 255}
	if err := w.Write(ctx, websocket.MessageBinary, data); err != nil {
		t.Fatal(err)
	}
	kind, got, err := w.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || !bytes.Equal(got, data) {
		t.Fatalf("upgrade %v %v %v", kind, got, err)
	}
}

func TestProviderGuardianWebSocketPathAndQuery(t *testing.T) {
	for _, path := range []string{"guardian", "guardian-classifier"} {
		t.Run(path, func(t *testing.T) {
			_, p := testProviderHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/backend-api/codex/"+path || r.URL.RawQuery != "feature=1" {
					t.Errorf("wrong WS endpoint %s", r.URL)
				}
				ws, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer ws.CloseNow()
				_, b, err := ws.Read(r.Context())
				if err != nil {
					return
				}
				var event map[string]any
				json.Unmarshal(b, &event)
				if event["future"] != "retained" {
					t.Error("payload changed")
				}
				ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created"}`))
				ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{}}`))
				ws.Close(websocket.StatusNormalClosure, "")
			}))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			ws, _, err := websocket.Dial(ctx, strings.Replace(p.URL, "http:", "ws:", 1)+"/v1/"+path+"?feature=1", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer local-secret"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer ws.CloseNow()
			ws.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-6-astra","future":"retained"}`))
			for i := 0; i < 2; i++ {
				if _, _, err := ws.Read(ctx); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestProviderBackendAliasAndMultipart(t *testing.T) {
	body := []byte("--boundary\r\nContent-Disposition: form-data; name=\"image\"; filename=\"a.png\"\r\nContent-Type: image/png\r\n\r\n\x00\xff\x01\r\n--boundary--\r\n")
	_, p := testProviderHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/backend-api/codex/images/edits" || !bytes.Equal(got, body) || r.Header.Get("Content-Type") != "multipart/form-data; boundary=boundary" {
			t.Error("multipart forwarding changed")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte{0, 255, 1})
	}))
	r := providerRequestForTest(t, "POST", p.URL+"/backend-api/codex/images/edits", body)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=boundary")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Equal(got, []byte{0, 255, 1}) {
		t.Fatalf("binary response changed %d %v", resp.StatusCode, got)
	}
	models := providerRequestForTest(t, "GET", p.URL+"/backend-api/codex/models", nil)
	resp, err = http.DefaultClient.Do(models)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
}

func TestProviderHTTPCompressedResponseUnchanged(t *testing.T) {
	var encoded bytes.Buffer
	z := gzip.NewWriter(&encoded)
	z.Write([]byte(`{"output":"compressed"}`))
	z.Close()
	_, p := testProviderHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(encoded.Bytes())
	}))
	r := providerRequestForTest(t, "POST", p.URL+"/v1/future", []byte(`{}`))
	r.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(b, encoded.Bytes()) {
		t.Fatal("upstream compression changed")
	}
}

func TestProviderHTTPMultiLineSSEUsageAndQuotaErrors(t *testing.T) {
	for _, quota := range []bool{false, true} {
		t.Run(map[bool]string{false: "usage", true: "quota"}[quota], func(t *testing.T) {
			var calls atomic.Int32
			s, p := testProviderHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if quota {
					io.WriteString(w, "data: {\"type\":\"error\",\"error\":{\"code\":\"usage_limit_reached\"}}\r\n\r\n")
					return
				}
				io.WriteString(w, "data: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"usage\":{\"input_tokens\":13,\"output_tokens\":2}}}\r\n\r\n")
			}))
			resp, err := http.DefaultClient.Do(providerRequestForTest(t, "POST", p.URL+"/v1/responses", []byte(`{}`)))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if calls.Load() != 1 {
				t.Fatal("SSE replayed")
			}
			if quota {
				if !strings.Contains(string(b), "usage_limit_reached") || !s.pool.find("a").routingCandidate().spent {
					t.Fatal("quota error not preserved/observed")
				}
			} else {
				s.stats.mu.Lock()
				usage := s.stats.monthlyUsage
				s.stats.mu.Unlock()
				if usage.InputTokens != 13 {
					t.Fatalf("multiline usage %+v", usage)
				}
			}
		})
	}
}

func TestProviderURLCannotEscapeBase(t *testing.T) {
	for _, path := range []string{"/v1/../admin", "/v1/%2e%2e/admin", "/v1/foo%2f..%2fadmin", "/v1/foo%5c..%5cadmin"} {
		r := httptest.NewRequest("POST", path, nil)
		if _, err := providerURL("https://example.test/backend-api/codex", r); err == nil {
			t.Errorf("accepted traversal %s", path)
		}
	}
}

func TestWebSocketEncryptedInputRetainedWithoutModification(t *testing.T) {
	input := json.RawMessage(`[{"type":"reasoning","encrypted_content":"opaque-original"},{"type":"compaction","encrypted_content":"opaque-compaction"}]`)
	u := newWebSocketUpstream(t, func(account string, conn *websocket.Conn, event websocketEnvelope) {
		if account != "a" || !bytes.Equal(event.Input, input) {
			t.Errorf("encrypted input changed or moved: %s %s", account, event.Input)
		}
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.created"})
		writeWebSocketEvent(t, conn, map[string]any{"type": "response.completed"})
	})
	defer u.Close()
	s, p := newWebSocketProxy(t, u.URL, []*Account{testAccount("a", 90), testAccount("b", 5)})
	s.pool.store.recordRoute(storedRoute{At: time.Now(), Thread: "old", Session: "session", Account: "a"})
	ws, _ := dialWebSocket(t, p.URL, codexWebSocketHeaders("session", "old"))
	defer ws.CloseNow()
	completeWebSocketTurn(t, ws, map[string]any{"type": "response.create", "input": input})
}
