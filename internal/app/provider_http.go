package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const maxObservedResponse = 8 << 20

// This is a provider-relative reverse proxy, never a user-selected forward proxy.
// Unknown endpoint paths are intentionally forwarded so new Codex tool surfaces
// don't silently become local 404s as the CLI evolves.
func (s *server) providerRequest(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeAPIKey(r); !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	if isInferencePath(r.URL.Path) && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		s.responsesWebSocket(w, r)
		return
	}
	s.providerHTTP(w, r)
}

func isInferencePath(path string) bool {
	return path == "/v1/responses" || path == "/v1/guardian" || path == "/v1/guardian-classifier"
}

func providerURL(base string, request *http.Request) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil || u == nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return nil, errors.New("invalid provider upstream URL")
	}
	if !strings.HasPrefix(request.URL.Path, "/v1/") {
		return nil, errors.New("invalid provider path")
	}
	for _, part := range strings.Split(request.URL.Path, "/") {
		if part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
			return nil, errors.New("invalid provider path")
		}
	}
	u.RawPath = strings.TrimRight(u.EscapedPath(), "/") + strings.TrimPrefix(request.URL.EscapedPath(), "/v1")
	u.Path = strings.TrimRight(u.Path, "/") + strings.TrimPrefix(request.URL.Path, "/v1")
	if u.RawQuery != "" && request.URL.RawQuery != "" {
		u.RawQuery += "&"
	}
	u.RawQuery += request.URL.RawQuery
	u.Fragment = ""
	return u, nil
}

func providerWebSocketURL(base string, request *http.Request) (string, error) {
	// Some internal callers construct a request without its provider path.
	if request.URL == nil || !isInferencePath(request.URL.Path) {
		return responsesWebSocketURL(base)
	}
	u, err := providerURL(base, request)
	if err != nil {
		return "", err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	return u.String(), nil
}

type providerRequestInfo struct {
	websocketEnvelope
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	ThreadID  string `json:"thread_id"`
}

// Decode only an inspection copy. Upstream always receives the exact original
// compressed/multipart/JSON bytes, including fields this binary doesn't know.
func inspectProviderBody(body []byte, encoding string) (providerRequestInfo, error) {
	var info providerRequestInfo
	var reader io.Reader = bytes.NewReader(body)
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
	case "gzip":
		decoder, err := gzip.NewReader(reader)
		if err != nil {
			return info, err
		}
		defer decoder.Close()
		reader = decoder
	case "zstd":
		decoder, err := zstd.NewReader(reader, zstd.WithDecoderMaxMemory(maxWebSocketMessage), zstd.WithDecoderConcurrency(1))
		if err != nil {
			return info, err
		}
		defer decoder.Close()
		reader = decoder
	default:
		// Future encodings still forward transparently; header affinity applies.
		return info, nil
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, maxWebSocketMessage+1))
	if err != nil {
		return info, err
	}
	if len(decoded) > maxWebSocketMessage {
		return info, errors.New("decoded request too large")
	}
	// Non-JSON bodies (image multipart, audio, SDP) are valid provider requests.
	_ = json.Unmarshal(decoded, &info)
	return info, nil
}

func providerRoute(r *http.Request, info providerRequestInfo) websocketRoute {
	route := websocketRouteFrom(r.Header)
	metadata := requestTurnMetadata(r.Header.Get(codexTurnMetadataKey), info.ClientMetadata)
	if route.thread == "" {
		route.thread = info.ThreadID
	}
	if route.thread == "" {
		route.thread = metadata.ThreadID
	}
	if route.session == "" {
		route.session = info.SessionID
	}
	// Codex standalone web search identifies its session in the JSON `id`
	// field, not in the ordinary inference headers.
	if route.session == "" && r.URL.Path == "/v1/alpha/search" {
		route.session = info.ID
	}
	return route
}

func (s *server) providerHTTP(w http.ResponseWriter, r *http.Request) {
	target, err := providerURL(s.upstream, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebSocketMessage))
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, "cannot read provider request")
		return
	}
	info, err := inspectProviderBody(body, r.Header.Get("Content-Encoding"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot inspect encoded provider request")
		return
	}
	route := providerRoute(r, info)
	model, tier := "", ""
	if isInferencePath(r.URL.Path) {
		model, tier = info.Model, info.ServiceTier
	}
	// Reuse the same durable-owner lookup and provisional claim registry as WS.
	d, err := newResponsesWebSocketDialer(s, r, route, model, tier)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "cannot resolve provider account")
		return
	}
	selection := s.claimAccount(route, d.durable, model, tier, nil, 0)
	account := selection.account
	if account == nil || selection.blocked != "" {
		selection.claim.release()
		writeError(w, http.StatusServiceUnavailable, "session account unavailable; retry")
		return
	}
	defer selection.claim.release()
	// Auxiliary search references, encrypted history, and response IDs can be
	// account-bound. Never silently move an established HTTP session, even when
	// its quota is exhausted. Return the upstream rejection to the caller.
	if selection.moved() || (len(d.owners) > 0 && account.id() != d.owners[0]) {
		writeError(w, http.StatusServiceUnavailable, "session account unavailable; cannot move account-bound request")
		return
	}
	if account.refreshDue(time.Now()) && !s.refreshed(account, account.id()) {
		writeError(w, http.StatusServiceUnavailable, "session account refresh failed")
		return
	}
	if !s.accountRoutable(account.id()) || !selection.claim.active() {
		writeError(w, http.StatusServiceUnavailable, "session account unavailable; retry")
		return
	}
	requestContext, cancel := context.WithCancel(r.Context())
	defer cancel()
	activeID := s.activeWebSockets.add(account.id(), func(_, _ string) { cancel() })
	defer s.activeWebSockets.remove(activeID, account.id())
	if !s.accountRoutable(account.id()) || !selection.claim.active() {
		writeError(w, http.StatusServiceUnavailable, "session account unavailable; retry")
		return
	}
	r = r.WithContext(requestContext)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	apiKey, _ := s.authorizeAPIKey(r)
	metadata := requestTurnMetadata(r.Header.Get(codexTurnMetadataKey), info.ClientMetadata)
	if metadata.RequestKind == "" && !isInferencePath(r.URL.Path) {
		metadata.RequestKind = strings.TrimPrefix(r.URL.Path, "/v1/")
	}
	started := time.Now()
	transport := s.client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1, // Flush deltas immediately; don't wait for a complete body.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = target
			pr.Out.Host = target.Host
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Del("X-Api-Key")
			pr.Out.Header.Del("Api-Key")
			// Explicit identity encoding avoids transport-level decompression; any
			// encoding returned anyway is forwarded with its matching header.
			pr.Out.Header.Set("Accept-Encoding", "identity")
			setProviderAuthorization(pr.Out.Header, account)
		},
		Transport: roundTripperFunc(func(out *http.Request) (*http.Response, error) {
			response, err := transport.RoundTrip(out)
			if err != nil || response.StatusCode != http.StatusUnauthorized || out.Context().Err() != nil {
				return response, err
			}
			// Only a definitive 401 is replayed, once, on the SAME account.
			// Never retry ambiguous network failures, 429s, 5xx, or partial streams.
			if !s.refreshed(account, account.id()) {
				return response, nil
			}
			response.Body.Close()
			retry := out.Clone(out.Context())
			retry.Body = io.NopCloser(bytes.NewReader(body))
			setProviderAuthorization(retry.Header, account)
			return transport.RoundTrip(retry)
		}),
		ModifyResponse: func(response *http.Response) error {
			account.observe(response.Header)
			response.Header.Del("Set-Cookie")
			response.Header.Del("Authorization")
			response.Header.Del("Proxy-Authenticate")
			s.log.Debug("provider HTTP response", "method", r.Method, "path", r.URL.Path, "thread", route.key(), "account", account.id(), "status", response.StatusCode, "latency", time.Since(started))
			if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusForbidden {
				if responseUsageLimitReached(response) {
					account.markSpent()
					s.stats.rateLimited(account.id())
				} else if response.StatusCode == http.StatusTooManyRequests {
					account.rateLimited(response.Header, 0)
					s.stats.rateLimited(account.id())
				}
			}
			if response.StatusCode >= 200 && response.StatusCode < 300 || response.StatusCode == http.StatusSwitchingProtocols {
				accepted := s.acceptWebSocketRoute(&websocketDial{account: account, claim: selection.claim}, storedRoute{At: time.Now(), Session: route.session, Thread: route.thread, Account: account.id()})
				if !accepted.allowed {
					return errRouteOwnerUnavailable
				}
				if isInferencePath(r.URL.Path) {
					s.stats.recordAccepted(time.Now(), statsThreadKey(route.key(), metadata), requestIP(r), apiKey.suffix, account.id(), info.Model, info.Reasoning.Effort, info.ServiceTier, metadata, true)
				}
				if response.StatusCode != http.StatusSwitchingProtocols && (response.Header.Get("Content-Encoding") == "" || response.Header.Get("Content-Encoding") == "identity") {
					observer := &providerResponseObserver{server: s, account: account, apiKey: apiKey, info: info, metadata: metadata, thread: statsThreadKey(route.key(), metadata), started: started,
						sse: strings.Contains(response.Header.Get("Content-Type"), "text/event-stream")}
					response.Body = &observedProviderBody{ReadCloser: response.Body, observer: observer}
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			if req.Context().Err() != nil {
				return
			}
			s.log.Warn("provider HTTP failure", "path", r.URL.Path, "thread", route.key(), "account", account.id(), "error", err)
			writeError(w, http.StatusBadGateway, "upstream provider unavailable")
		},
	}
	proxy.ServeHTTP(w, r)
}

func setProviderAuthorization(headers http.Header, account *Account) {
	account.mu.Lock()
	defer account.mu.Unlock()
	headers.Set("Authorization", "Bearer "+account.AccessToken)
	headers.Set("Chatgpt-Account-Id", claimsFromToken(account.IDToken).Auth.AccountID)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type observedProviderBody struct {
	io.ReadCloser
	observer *providerResponseObserver
}

func (b *observedProviderBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.observer.write(p[:n])
	if err == io.EOF {
		b.observer.finish()
	}
	return n, err
}

// Observation is bounded and never rewrites or delays response bytes. Unknown
// events and oversized events still reach Codex unchanged.
type providerResponseObserver struct {
	server    *server
	account   *Account
	apiKey    apiKeyIdentity
	info      providerRequestInfo
	metadata  turnMetadata
	thread    string
	started   time.Time
	sse       bool
	buffer    []byte
	data      []byte
	dropping  bool
	dropEvent bool
	recorded  bool
}

func (o *providerResponseObserver) write(p []byte) {
	if o.recorded {
		return
	}
	if !o.sse {
		if len(o.buffer)+len(p) > maxObservedResponse {
			o.dropping = true
			o.buffer = nil
		}
		if !o.dropping {
			o.buffer = append(o.buffer, p...)
		}
		return
	}
	for _, b := range p {
		if b == '\n' {
			o.line()
			o.buffer = nil
			o.dropping = false
		} else if !o.dropping {
			if len(o.buffer) >= maxObservedResponse {
				o.buffer = nil
				o.dropping = true
			} else {
				o.buffer = append(o.buffer, b)
			}
		}
	}
}

func (o *providerResponseObserver) line() {
	line := bytes.TrimSuffix(o.buffer, []byte{'\r'})
	if o.dropping {
		o.dropEvent = true
		o.data = nil
		return
	}
	if len(line) == 0 {
		if !o.dropEvent {
			var event websocketEnvelope
			if json.Unmarshal(o.data, &event) == nil {
				headers := websocketEventHeaders(event.Headers)
				o.account.observe(headers)
				if rejection := websocketRejection(event); rejection != websocketRejectionNone {
					o.server.handleWebSocketRejection(o.account, rejection, headers, o.thread)
				}
				if event.Type == "response.completed" || event.Type == "response.incomplete" || event.Type == "response.failed" {
					o.record(event.Response.responsePayload, event.Type == "response.completed")
				}
			}
		}
		o.data = nil
		o.dropEvent = false
		return
	}
	if !o.dropEvent && bytes.HasPrefix(line, []byte("data:")) {
		value := bytes.TrimPrefix(line[5:], []byte{' '})
		if len(o.data)+len(value)+1 > maxObservedResponse {
			o.dropEvent = true
			o.data = nil
			return
		}
		o.data = append(o.data, value...)
		o.data = append(o.data, '\n')
	}
}

func (o *providerResponseObserver) finish() {
	if o.sse || o.dropping || o.recorded {
		return
	}
	var payload responsePayload
	if json.Unmarshal(o.buffer, &payload) == nil {
		o.record(payload, true)
	}
}

func (o *providerResponseObserver) record(payload responsePayload, completed bool) {
	if o.recorded {
		return
	}
	o.recorded = true
	model, tier := payload.Model, payload.ServiceTier
	if model == "" {
		model = o.info.Model
	}
	if tier == "" {
		tier = o.info.ServiceTier
	}
	if !payload.Usage.empty() {
		logResponseUsage(o.server.log, o.thread, o.account.id(), model, tier, o.metadata, time.Since(o.started), payload.Usage)
		o.server.stats.recordAPIKeyUsage(o.apiKey.name, o.thread, o.account.id(), model, o.info.Reasoning.Effort, tier, payload.Usage)
	}
	if completed {
		o.server.stats.completed(o.thread, o.account.id(), o.metadata, time.Since(o.started))
	}
}
