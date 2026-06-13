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
	InternalDomains  []string               `json:"internal-domains" doc:"your org's email domains"`
	Owners           ownersConfig           `json:"owners"`
	RestoreLocations restoreLocationsConfig `json:"restore-locations"`
	Stash            stashConfig            `json:"stash"`
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

// stashConfig holds settings for the stash push/pop commands.
type stashConfig struct {
	// Folder is the My-Drive parking area that stash push moves folder contents
	// into and stash pop drains. It MUST be a regular My-Drive folder inside the
	// crawl root, never a shared drive: the files parked here are owned by third
	// parties, which cannot be moved to a shared drive. Keeping it under the crawl root
	// also means a user's own parked files still surface in their
	// "owner:me" Drive search so they get migrated like any other loose file.
	Folder rootConfig `json:"folder" doc:"a My-Drive folder inside the crawl root for parking"`
}

// restoreLocationsConfig holds settings for the restore-locations command.
type restoreLocationsConfig struct {
	// StagingFolder is the shared-drive folder owners drag their files into
	// before running restore-locations. The tool scans it (one level deep),
	// looks up each file's original parent in the database, and moves it back.
	StagingFolder rootConfig `json:"staging-folder" doc:"the shared-drive folder owners drag files into"`
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
		Crawl:            crawlConfig{Root: folder("ROOT")},
		InternalDomains:  []string{"example.com"},
		Owners:           ownersConfig{IgnoreInternalDomains: false},
		RestoreLocations: restoreLocationsConfig{StagingFolder: folder("STAGING")},
		Stash:            stashConfig{Folder: folder("STASH")},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("rendering config template: %w", err)
	}
	return string(b) + "\n", nil
}
