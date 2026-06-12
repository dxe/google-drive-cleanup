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
	InternalDomains []string     `json:"internal-domains"`
	Owners          ownersConfig `json:"owners"`
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

// rootConfig is the crawl root spec. id and name are kept together so the
// crawler can guard against a stale/wrong id by checking the live folder name
// against name (see crawler.validateAndInsertRoot).
type rootConfig struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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
