package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// config is the on-disk config.json. Each subcommand gets its own top-level
// section so new commands can add settings without colliding.
type config struct {
	Crawl crawlConfig `json:"crawl"`
	// InternalDomains lists the org's own email domains (no leading "@"), e.g.
	// "example.com". Commands can use these to distinguish internal from
	// external owners.
	InternalDomains  []string               `json:"internal-domains"`
	Owners           ownersConfig           `json:"owners"`
	RestoreLocations restoreLocationsConfig `json:"restore-locations"`
	Stash            stashConfig            `json:"stash"`
}

type crawlConfig struct {
	Root rootConfig `json:"root"`
}

// ownersConfig holds settings for the owners command.
type ownersConfig struct {
	// IgnoreInternalDomains, when true, drops owners whose email is on one of
	// the configured InternalDomains from the owners report.
	IgnoreInternalDomains bool `json:"ignore-internal-domains"`
}

// rootConfig is a folder spec: id and name kept together so the tool can
// guard against a stale/wrong id by verifying the live folder name matches.
type rootConfig struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// stashConfig holds settings for the stash push/pop commands.
type stashConfig struct {
	// Folder is the My-Drive parking area that stash push moves folder contents
	// into and stash pop drains. It MUST be a regular My-Drive folder inside the
	// crawl root, never a shared drive: the files parked here are owned by third
	// parties, which cannot be moved to a shared drive. Keeping it under the crawl root
	// also means a user's own parked files still surface in their
	// "owner:me" Drive search so they get migrated like any other loose file.
	Folder rootConfig `json:"folder"`
}

// restoreLocationsConfig holds settings for the restore-locations command.
type restoreLocationsConfig struct {
	// StagingFolder is the shared-drive folder owners drag their files into
	// before running restore-locations. The tool scans it (one level deep),
	// looks up each file's original parent in the database, and moves it back.
	StagingFolder rootConfig `json:"staging-folder"`
}

func loadConfig(path string) (config, error) {
	var cfg config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading config: %w", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", path, err)
	}
	if cfg.Crawl.Root.ID == "" || cfg.Crawl.Root.Name == "" {
		return cfg, fmt.Errorf(`%s must set both crawl.root.id and crawl.root.name`, path)
	}
	return cfg, nil
}
