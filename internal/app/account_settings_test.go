package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type accountSettingsRoundTrip func(*http.Request) (*http.Response, error)

func (roundTrip accountSettingsRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestConnectAccountEnsuresTrainingDisabled(t *testing.T) {
	for _, test := range []struct {
		name           string
		body           string
		readStatus     int
		patchStatus    int
		transportError bool
		wantMethods    []string
		wantError      string
	}{
		{name: "already disabled", body: `{"settings":{"training_allowed":false}}`, wantMethods: []string{"GET"}},
		{name: "enabled", body: `{"settings":{"training_allowed":true},"other":"ignored"}`, wantMethods: []string{"GET", "PATCH"}},
		{name: "read forbidden", readStatus: 403, wantMethods: []string{"GET"}, wantError: "read training setting: account settings returned 403 Forbidden"},
		{name: "read network error", transportError: true, wantMethods: []string{"GET"}, wantError: "read training setting:"},
		{name: "update forbidden", body: `{"settings":{"training_allowed":true}}`, patchStatus: 403, wantMethods: []string{"GET", "PATCH"}, wantError: "disable training: account settings returned 403 Forbidden"},
		{name: "missing setting", body: `{"settings":{}}`, wantMethods: []string{"GET"}, wantError: "missing or null"},
		{name: "missing settings", body: `{}`, wantMethods: []string{"GET"}, wantError: "missing or null"},
		{name: "null setting", body: `{"settings":{"training_allowed":null}}`, wantMethods: []string{"GET"}, wantError: "missing or null"},
		{name: "string false", body: `{"settings":{"training_allowed":"false"}}`, wantMethods: []string{"GET"}, wantError: "invalid settings response"},
		{name: "wrong scope", body: `{"beta_settings":{"training_allowed":false}}`, wantMethods: []string{"GET"}, wantError: "missing or null"},
		{name: "invalid JSON", body: `<html>error</html>`, wantMethods: []string{"GET"}, wantError: "invalid settings response"},
		{name: "trailing JSON", body: `{"settings":{"training_allowed":false}} {}`, wantMethods: []string{"GET"}, wantError: "invalid settings response"},
		{name: "oversized response", body: strings.Repeat(" ", accountSettingsMaxBytes+1), wantMethods: []string{"GET"}, wantError: "response too large"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := testAccount("account-a", 0).persisted()
			var methods []string
			client := &http.Client{Transport: accountSettingsRoundTrip(func(r *http.Request) (*http.Response, error) {
				methods = append(methods, r.Method)
				if r.Header.Get("Authorization") != "Bearer token-account-a" || r.Header.Get("chatgpt-account-id") != "account-a" {
					t.Error("missing account credentials")
				}
				if _, ok := r.Context().Deadline(); !ok {
					t.Error("missing settings timeout")
				}
				status := test.readStatus
				body := test.body
				switch r.Method {
				case http.MethodGet:
					if r.URL.String() != userSettingsEndpoint {
						t.Errorf("unexpected read endpoint: %s", r.URL)
					}
					if test.transportError {
						return nil, errors.New("network unavailable")
					}
					if status == 0 {
						status = 200
					}
				case http.MethodPatch:
					if r.URL.Scheme+"://"+r.URL.Host+r.URL.Path != accountSettingsEndpoint {
						t.Errorf("unexpected update endpoint: %s", r.URL)
					}
					if r.URL.Query().Get("feature") != "training_allowed" || r.URL.Query().Get("value") != "false" {
						t.Errorf("unexpected update query: %s", r.URL.RawQuery)
					}
					status = test.patchStatus
					if status == 0 {
						status = 204
					}
					body = ""
				default:
					t.Fatalf("unexpected method: %s", r.Method)
				}
				return &http.Response{StatusCode: status, Status: strconv.Itoa(status) + " " + http.StatusText(status), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			account, err := connectAccount(context.Background(), client, tokenResponse{IDToken: source.IDToken, AccessToken: source.AccessToken, RefreshToken: source.RefreshToken})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) || account != nil {
					t.Fatalf("account=%v, error=%v, want %q", account, err, test.wantError)
				}
			} else if err != nil || account == nil || account.id() != "account-a" {
				t.Fatalf("enrollment failed: %v", err)
			}
			if !reflect.DeepEqual(methods, test.wantMethods) {
				t.Errorf("methods=%v, want %v", methods, test.wantMethods)
			}
		})
	}
}

func TestConnectAccountSettingsCancellation(t *testing.T) {
	source := testAccount("account-a", 0).persisted()
	ctx, cancel := context.WithCancel(context.Background())
	client := &http.Client{Transport: accountSettingsRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("must not mutate after canceled read")
		}
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	defer cancel()
	account, err := connectAccount(ctx, client, tokenResponse{IDToken: source.IDToken, AccessToken: source.AccessToken})
	if account != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}
