package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// fakeNotionOAuth issues tokens "access-N" / "refresh-N" and accepts only the
// latest access token on the API.
type fakeNotionOAuth struct {
	t       *testing.T
	issued  int
	refused bool   // refresh tokens no longer valid
	expired string // an access token the API refuses
	grants  []string
}

func (f *fakeNotionOAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/oauth/token":
		id, secret, _ := r.BasicAuth()
		if id != "client" || secret != "shh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.grants = append(f.grants, body["grant_type"]+":"+body["code"]+body["refresh_token"])
		wantRefresh := "refresh-" + string(rune('0'+f.issued))
		if body["grant_type"] == "refresh_token" && (f.refused || body["refresh_token"] != wantRefresh) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error": "invalid_grant"}`)
			return
		}
		f.issued++
		n := string(rune('0' + f.issued))
		json.NewEncoder(w).Encode(notionTokens{AccessToken: "access-" + n, RefreshToken: "refresh-" + n})
	case "/users/me":
		token := r.Header.Get("Authorization")
		if token != "Bearer access-"+string(rune('0'+f.issued)) || token == "Bearer "+f.expired {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"ok": true}`)
	default:
		f.t.Errorf("unexpected %s", r.URL)
	}
}

func newTestOAuthClient(t *testing.T, srv *httptest.Server) *notionClient {
	n := newNotionClient(NotionConfig{
		ClientID:            "client",
		ClientSecretCommand: []string{"echo", "shh"},
		redirectURI:         "http://127.0.0.1:7777/notion/callback",
		tokenPath:           filepath.Join(t.TempDir(), "notion-oauth.json"),
	})
	n.base, n.oauth.base = srv.URL, srv.URL
	return n
}

func TestNotionOAuth(t *testing.T) {
	fake := &fakeNotionOAuth{t: t}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	n := newTestOAuthClient(t, srv)
	ctx := context.Background()
	var out map[string]any

	if err := n.call(ctx, http.MethodGet, "/users/me", nil, &out); !errors.Is(err, errNotionNotConnected) {
		t.Fatalf("before connecting: %v, want not connected", err)
	}

	authorize, _ := url.Parse(n.oauth.authorizeURL())
	q := authorize.Query()
	if q.Get("client_id") != "client" || q.Get("owner") != "user" || q.Get("redirect_uri") != "http://127.0.0.1:7777/notion/callback" || q.Get("state") == "" {
		t.Errorf("authorize URL = %s", authorize)
	}
	if n.oauth.checkState("forged") {
		t.Error("a forged state is accepted")
	}
	n.oauth.authorizeURL()
	state := n.oauth.state
	if !n.oauth.checkState(state) || n.oauth.checkState(state) {
		t.Error("a state must be accepted once")
	}

	if err := n.oauth.exchange(ctx, "code-1"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(n.oauth.tokenPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("token file: %v %v, want mode 0600", info.Mode(), err)
	}
	if err := n.call(ctx, http.MethodGet, "/users/me", nil, &out); err != nil {
		t.Fatalf("connected: %v", err)
	}

	// The access token expires: the client refreshes it and retries.
	fake.expired = "access-1"
	if err := n.call(ctx, http.MethodGet, "/users/me", nil, &out); err != nil {
		t.Fatalf("after refresh: %v", err)
	}
	if got, _ := n.oauth.load(); got.AccessToken != "access-2" || got.RefreshToken != "refresh-2" {
		t.Errorf("stored tokens = %+v, want the refreshed ones", got)
	}

	// The refresh token is revoked: connecting again is the way out.
	fake.refused = true
	fake.expired = "access-2"
	if err := n.call(ctx, http.MethodGet, "/users/me", nil, &out); !errors.Is(err, errNotionNotConnected) {
		t.Errorf("revoked: %v, want not connected", err)
	}
}
