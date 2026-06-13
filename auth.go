package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

const (
	credentialsFile = "credentials.json"
	tokenFile       = "token.json"

	// callbackPort is the loopback port the consent flow listens on for the
	// OAuth redirect. Desktop-app OAuth clients accept http://localhost on any
	// port, so this need not be registered in the Google console — but it must
	// match the forwarded port in .devcontainer/devcontainer.json so the host
	// browser's redirect reaches the server running inside the container.
	callbackPort = 8765

	oauthState = "state-token"
)

// newDriveService builds an authenticated Drive client for the given scopes.
// Read-only commands pass drive.DriveReadonlyScope; the write commands pass
// drive.DriveScope (full).
//
// The scopes a cached token was granted are recorded next to it in token.json.
// If the cached token does not cover the scopes this command needs (e.g. you
// ran a read-only command first and now run a write command, or token.json
// predates scope tracking), consent re-runs automatically and the new token
// replaces the old one — no manual `rm token.json` required.
func newDriveService(ctx context.Context, scopes ...string) (*drive.Service, error) {
	b, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s (see README for how to create an OAuth client): %w", credentialsFile, err)
	}
	config, err := google.ConfigFromJSON(b, scopes...)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", credentialsFile, err)
	}

	tok, granted, err := tokenFromFile(tokenFile)
	if err != nil || !scopesCover(granted, scopes) {
		if err == nil {
			fmt.Fprintf(os.Stderr,
				"Cached token does not cover the scope this command needs (have %v, need %v); re-authorizing.\n",
				granted, scopes)
		}
		tok, err = tokenFromWeb(ctx, config)
		if err != nil {
			return nil, err
		}
		if err := saveToken(tokenFile, tok, grantedScopes(tok, scopes)); err != nil {
			return nil, err
		}
	}
	return drive.NewService(ctx, option.WithHTTPClient(config.Client(ctx, tok)))
}

// storedToken is the on-disk shape of token.json: the OAuth token plus the
// scopes it was granted, so a later run can tell whether the cached token is
// broad enough for the command being run. The embedded *oauth2.Token keeps the
// token's own JSON fields at the top level, so a legacy token.json written
// before scope tracking still unmarshals cleanly (with an empty Scopes).
type storedToken struct {
	*oauth2.Token
	Scopes []string `json:"scopes,omitempty"`
}

// scopesCover reports whether the granted scopes are sufficient for every
// required scope. The full Drive scope implies the read-only scope, so a token
// granted full access satisfies a read-only command without re-consent.
func scopesCover(granted, required []string) bool {
	have := make(map[string]bool, len(granted))
	for _, s := range granted {
		have[s] = true
	}
	for _, r := range required {
		if have[r] {
			continue
		}
		if r == drive.DriveReadonlyScope && have[drive.DriveScope] {
			continue // full Drive access covers read-only operations
		}
		return false
	}
	return true
}

// grantedScopes returns the scopes the exchanged token was actually granted.
// Google echoes them back in the token response's "scope" field (space-
// separated); when that is absent we fall back to the scopes we requested.
func grantedScopes(tok *oauth2.Token, requested []string) []string {
	if s, ok := tok.Extra("scope").(string); ok && strings.TrimSpace(s) != "" {
		return strings.Fields(s)
	}
	return requested
}

func tokenFromFile(path string) (*oauth2.Token, []string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	st := storedToken{Token: &oauth2.Token{}}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	// An incomplete token (e.g. a leftover empty file from an aborted consent)
	// parses fine but is useless — treat it as a cache miss so consent re-runs
	// instead of silently failing later at API-call time.
	if st.AccessToken == "" && st.RefreshToken == "" {
		return nil, nil, fmt.Errorf("%s has no access or refresh token", path)
	}
	return st.Token, st.Scopes, nil
}

// tokenFromWeb runs the first-time consent flow. It starts a loopback HTTP
// server on callbackPort, points the OAuth redirect there, and prints the
// consent URL. The user authorizes in a (host) browser; Google redirects to
// http://localhost:callbackPort/?code=..., the devcontainer forwards that port
// into the container, and the server captures the code automatically. If the
// browser can't reach the callback, the user can paste the code (or the whole
// redirect URL) on stdin as a fallback.
func tokenFromWeb(ctx context.Context, config *oauth2.Config) (*oauth2.Token, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", callbackPort))
	if err != nil {
		return nil, fmt.Errorf("starting local OAuth callback server on port %d: %w", callbackPort, err)
	}
	defer listener.Close()

	config.RedirectURL = fmt.Sprintf("http://localhost:%d", callbackPort)
	authURL := config.AuthCodeURL(oauthState, oauth2.AccessTypeOffline)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			http.Error(w, "Authorization failed: "+e+". You can close this tab.", http.StatusBadRequest)
			errCh <- fmt.Errorf("authorization denied: %s", e)
			return
		}
		if q.Get("state") != oauthState {
			http.Error(w, "State mismatch — possible CSRF. You can close this tab.", http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth state mismatch")
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "No auth code in redirect.", http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "Authorization complete. You can close this tab and return to the terminal.")
		codeCh <- code
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	defer srv.Shutdown(context.Background())

	fmt.Fprintf(os.Stderr,
		"Open this URL in a browser and authorize:\n\n%s\n\n"+
			"Waiting for the redirect to be captured automatically...\n"+
			"(If the browser can't reach the callback within %v, you will be\n"+
			"prompted to paste the auth code or full redirect URL here.)\n", authURL, browserWaitTimeout)

	// Phase 1: wait for the browser redirect — no stdin reading, no goroutine leak.
	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(browserWaitTimeout):
		// fall through to stdin fallback
	}

	if code == "" {
		// Phase 2: browser redirect didn't arrive; ask the user to paste.
		// The goroutine below exits as soon as the user provides input, so it
		// does not outlive this call and cannot race with later stdin reads.
		fmt.Fprintf(os.Stderr, "No redirect received. Paste the auth code or the full redirect URL and press Enter: ")
		stdinCh := make(chan string, 1)
		go func() {
			var in string
			if _, err := fmt.Scan(&in); err == nil {
				stdinCh <- extractCode(in)
			}
		}()
		select {
		case code = <-codeCh: // late browser redirect
		case code = <-stdinCh:
		case err := <-errCh:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	tok, err := config.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchanging auth code: %w", err)
	}
	return tok, nil
}

// browserWaitTimeout is how long tokenFromWeb waits for the OAuth browser
// redirect before falling back to a manual paste prompt. The redirect normally
// arrives within seconds; this timeout only fires when port forwarding is
// broken or the browser never completes the flow.
const browserWaitTimeout = 2 * time.Minute

// extractCode accepts either a bare auth code or a full redirect URL pasted on
// stdin and returns the code.
func extractCode(in string) string {
	in = strings.TrimSpace(in)
	if u, err := url.Parse(in); err == nil && u.Query().Get("code") != "" {
		return u.Query().Get("code")
	}
	return in
}

func saveToken(path string, tok *oauth2.Token, scopes []string) error {
	b, err := json.Marshal(storedToken{Token: tok, Scopes: scopes})
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		return fmt.Errorf("caching oauth token: %w", err)
	}
	fmt.Fprintf(os.Stderr, "\nSaved oauth token to %s\nAuthentication completed successfully.\n", path)
	return nil
}
