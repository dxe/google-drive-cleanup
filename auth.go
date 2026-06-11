package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

const (
	credentialsFile = "credentials.json"
	tokenFile       = "token.json"
)

// newDriveService builds an authenticated Drive client. The crawler only
// needs drive.readonly; the later move/transfer tooling will need the full
// drive scope (delete token.json and re-consent after widening it).
func newDriveService(ctx context.Context) (*drive.Service, error) {
	b, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s (see README for how to create an OAuth client): %w", credentialsFile, err)
	}
	config, err := google.ConfigFromJSON(b, drive.DriveReadonlyScope)
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
	return tok, nil
}

// tokenFromWeb runs the first-time consent flow: print the URL, have the user
// authorize in a browser, and read the auth code back from stdin.
func tokenFromWeb(ctx context.Context, config *oauth2.Config) (*oauth2.Token, error) {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Fprintf(os.Stderr,
		"Open the following URL in a browser, authorize, then paste the auth code here:\n\n%s\n\nAuth code: ", authURL)
	var code string
	if _, err := fmt.Scan(&code); err != nil {
		return nil, fmt.Errorf("reading auth code from stdin: %w", err)
	}
	tok, err := config.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchanging auth code: %w", err)
	}
	return tok, nil
}

func saveToken(path string, tok *oauth2.Token) error {
	b, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		return fmt.Errorf("caching oauth token: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Saved oauth token to %s\n", path)
	return nil
}
