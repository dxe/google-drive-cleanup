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
// Read-only commands pass drive.DriveReadonlyScope; restore-locations passes
// drive.DriveScope (full). If you widen the scope, delete token.json and
// re-run to re-consent — the cached refresh token only covers the scope it was
// issued for.
func newDriveService(ctx context.Context, scopes ...string) (*drive.Service, error) {
	b, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s (see README for how to create an OAuth client): %w", credentialsFile, err)
	}
	config, err := google.ConfigFromJSON(b, scopes...)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", credentialsFile, err)
	}

	tok, err := tokenFromFile(tokenFile)
	if err != nil {
		tok, err = tokenFromWeb(ctx, config)
		if err != nil {
			return nil, err
		}
		if err := saveToken(tokenFile, tok); err != nil {
			return nil, err
		}
	}
	return drive.NewService(ctx, option.WithHTTPClient(config.Client(ctx, tok)))
}

func tokenFromFile(path string) (*oauth2.Token, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	tok := &oauth2.Token{}
	if err := json.Unmarshal(b, tok); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	// An incomplete token (e.g. a leftover empty file from an aborted consent)
	// parses fine but is useless — treat it as a cache miss so consent re-runs
	// instead of silently failing later at API-call time.
	if tok.AccessToken == "" && tok.RefreshToken == "" {
		return nil, fmt.Errorf("%s has no access or refresh token", path)
	}
	return tok, nil
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
			"(If your browser can't reach the callback, paste the auth code — or the\n"+
			"full localhost redirect URL — here and press enter): ", authURL)

	// stdin fallback: runs concurrently with the callback server.
	go func() {
		var in string
		if _, err := fmt.Scan(&in); err == nil {
			codeCh <- extractCode(in)
		}
	}()

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tok, err := config.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchanging auth code: %w", err)
	}
	return tok, nil
}

// extractCode accepts either a bare auth code or a full redirect URL pasted on
// stdin and returns the code.
func extractCode(in string) string {
	in = strings.TrimSpace(in)
	if u, err := url.Parse(in); err == nil && u.Query().Get("code") != "" {
		return u.Query().Get("code")
	}
	return in
}

func saveToken(path string, tok *oauth2.Token) error {
	b, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		return fmt.Errorf("caching oauth token: %w", err)
	}
	fmt.Fprintf(os.Stderr, "\nSaved oauth token to %s\nAuthentication completed successfully.\n", path)
	return nil
}
