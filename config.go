package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// placeholderPrefix marks the example values written by `drive-cleanup init`.
// A value still carrying this prefix means the template was not edited, so
// validation rejects it with a pointer at what to fix.
const placeholderPrefix = "REPLACE_WITH_"

// config is the on-disk config.json. Each subcommand gets its own top-level
// section so new commands can add settings without colliding.
type config struct {
	Crawl crawlConfig `json:"crawl"`
	// InternalDomains lists the org's own email domains (no leading "@"), e.g.
	// "example.com". Commands can use these to distinguish internal from
	// external owners.
	InternalDomains []string        `json:"internal-domains" doc:"your org's email domains"`
	Owners          ownersConfig    `json:"owners"`
	Migration       migrationConfig `json:"migration"`
}

type crawlConfig struct {
	Root rootConfig `json:"root" doc:"the folder to crawl (required for every command)"`
}

// ownersConfig holds settings for the owners command.
type ownersConfig struct {
	// IgnoreInternalDomains, when true, drops owners whose email is on one of
	// the configured InternalDomains from the owners report.
	IgnoreInternalDomains bool `json:"ignore-internal-domains" doc:"set true to hide internal owners from the owners report"`
}

// rootConfig is a folder spec: id and name kept together so the tool can
// guard against a stale/wrong id by verifying the live folder name matches.
type rootConfig struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// validate reports whether the folder spec is filled in. section is the dotted
// config path (e.g. "stash.folder") used in error messages. It rejects both an
// empty id/name and a value still carrying the init template's placeholder.
func (r rootConfig) validate(section string) error {
	if r.ID == "" || r.Name == "" {
		return fmt.Errorf("%s.id and %s.name must both be set (run `drive-cleanup init` to scaffold config.json)", section, section)
	}
	if strings.HasPrefix(r.ID, placeholderPrefix) || strings.HasPrefix(r.Name, placeholderPrefix) {
		return fmt.Errorf("%s still has placeholder values from `drive-cleanup init`; edit config with the real folder id and name", section)
	}
	return nil
}

// migrationConfig holds settings for the migration commands. Most are shared by
// pack and unpack; a few (e.g. SkipUnmovable) apply to only one of them.
type migrationConfig struct {
	// PackingFolder holds one folder per user being migrated, each containing
	// that user's Container and Stash. It MUST be a regular My-Drive folder,
	// never in a shared drive (a user's Stash parks third-party-owned files,
	// which a shared drive cannot hold), and MUST NOT be inside the crawl root
	// (a re-crawl would ingest mid-migration scaffolding).
	PackingFolder rootConfig `json:"packing-folder" doc:"a My-Drive folder outside the crawl root holding per-user packing folders"`
	// DropoffFolder is the shared-drive folder a user drags their Container
	// into; the drag flips ownership of the container's whole tree to the org.
	DropoffFolder rootConfig `json:"dropoff-folder" doc:"the shared-drive folder users drag their Container into"`
	// SkipUnmovable mirrors the pack command's --skip-unmovable flag: skip
	// crawled items the crawling account cannot edit (they cannot be moved) and
	// pack the rest instead of aborting. Either the flag or this setting enables
	// the behavior.
	SkipUnmovable bool `json:"skip-unmovable" doc:"set true to skip uneditable items and pack the rest instead of aborting (same as pack's --skip-unmovable)"`
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
	if err := cfg.Crawl.Root.validate("crawl.root"); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// configTemplate is the scaffold written by `drive-cleanup init`. It is built by
// marshaling a config value so the JSON keys come straight from the struct's
// json tags and never drift from what the subcommands read. Folder specs carry
// REPLACE_WITH_ placeholders (rejected by rootConfig.validate) so a half-edited
// template fails loudly instead of crawling the wrong folder. JSON has no
// comments, so the field guidance is printed to stderr by runInit instead.
func configTemplate() (string, error) {
	folder := func(name string) rootConfig {
		return rootConfig{
			ID:   placeholderPrefix + name + "_FOLDER_ID",
			Name: placeholderPrefix + name + "_FOLDER_NAME",
		}
	}
	cfg := config{
		Crawl:           crawlConfig{Root: folder("ROOT")},
		InternalDomains: []string{"example.com"},
		Owners:          ownersConfig{IgnoreInternalDomains: false},
		Migration: migrationConfig{
			PackingFolder: folder("PACKING"),
			DropoffFolder: folder("DROPOFF"),
			SkipUnmovable: false,
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("rendering config template: %w", err)
	}
	return string(b) + "\n", nil
}
