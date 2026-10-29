package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
)

// notionOAuth signs in through a public (OAuth) connection. Its token acts
// as the signed-in user, limited to the pages shared when connecting, which
// suits a workspace where members cannot create internal connections.
type notionOAuth struct {
	base          string
	client        *http.Client
	clientID      string
	secretCommand []string
	redirectURI   string
	// tokenPath holds the access and refresh tokens, readable by me only.
	tokenPath string

	// mu serializes token grants: a refresh token is good for one use, so
	// two refreshes at once would lose the session.
	mu    sync.Mutex
	state string
}

type notionTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

var errNotionNotConnected = errors.New("not connected: use Connect Notion")

// authorizeURL starts a sign-in. Its state ties the callback to it, so a
// callback forged by another page is refused.
func (o *notionOAuth) authorizeURL() string {
	b := make([]byte, 16)
	rand.Read(b)
	state := hex.EncodeToString(b)
	o.mu.Lock()
	o.state = state
	o.mu.Unlock()
	q := url.Values{
		"client_id":     {o.clientID},
		"redirect_uri":  {o.redirectURI},
		"response_type": {"code"},
		"owner":         {"user"},
		"state":         {state},
	}
	return o.base + "/oauth/authorize?" + q.Encode()
}

func (o *notionOAuth) checkState(state string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	ok := o.state != "" && subtle.ConstantTimeCompare([]byte(o.state), []byte(state)) == 1
	o.state = ""
	return ok
}

func (o *notionOAuth) exchange(ctx context.Context, code string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.grant(ctx, map[string]string{"grant_type": "authorization_code", "code": code, "redirect_uri": o.redirectURI})
}

// refresh trades the refresh token for new tokens, unless another caller
// already did since stale was read.
func (o *notionOAuth) refresh(ctx context.Context, stale string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	t, err := o.load()
	if err != nil {
		return err
	}
	if t.AccessToken != stale {
		return nil
	}
	if t.RefreshToken == "" {
		return errNotionNotConnected
	}
	return o.grant(ctx, map[string]string{"grant_type": "refresh_token", "refresh_token": t.RefreshToken})
}

func (o *notionOAuth) grant(ctx context.Context, body map[string]string) error {
	secret, err := runCommand(ctx, o.secretCommand)
	if err != nil {
		return fmt.Errorf("client_secret_command: %w", err)
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/oauth/token", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.SetBasicAuth(o.clientID, secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Notion-Version", notionVersion)
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
			Message     string `json:"message"`
		}
		json.Unmarshal(data, &apiErr)
		if body["grant_type"] == "refresh_token" {
			// The session is gone: connecting again is the way out.
			return fmt.Errorf("%w (%s)", errNotionNotConnected, apiErr.Error)
		}
		return fmt.Errorf("token: %d %s %s%s", resp.StatusCode, apiErr.Error, apiErr.Description, apiErr.Message)
	}
	var t notionTokens
	if err := json.Unmarshal(data, &t); err != nil || t.AccessToken == "" {
		return errors.New("token: no access token in the answer")
	}
	return o.save(t)
}

func (o *notionOAuth) load() (notionTokens, error) {
	var t notionTokens
	data, err := os.ReadFile(o.tokenPath)
	if errors.Is(err, os.ErrNotExist) {
		return t, errNotionNotConnected
	}
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(data, &t); err != nil || t.AccessToken == "" {
		return t, errNotionNotConnected
	}
	return t, nil
}

func (o *notionOAuth) save(t notionTokens) error {
	data, _ := json.Marshal(t)
	tmp, err := os.CreateTemp(filepath.Dir(o.tokenPath), ".notion-oauth-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	// CreateTemp opens the file 0600.
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), o.tokenPath)
}

func (s *server) notionConnect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, s.notion.authorizeURL(), http.StatusFound)
}

func (s *server) notionCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !s.notion.checkState(q.Get("state")) {
		http.Error(w, "This sign-in was not started here, or it expired: use Connect Notion again.", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		http.Error(w, "Notion refused the connection: "+e, http.StatusBadRequest)
		return
	}
	if err := s.notion.exchange(r.Context(), q.Get("code")); err != nil {
		http.Error(w, "Notion connection failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}
