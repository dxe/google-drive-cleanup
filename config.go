package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	Archive         archiveConfig   `json:"archive"`
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

// configured reports whether the folder spec was actually filled in (non-empty
// and not the init template's placeholder). Unlike validate it never errors:
// it is for optional sections consulted by commands that also work without
// them (e.g. the crawl and review commands consult archive.root only when set).
func (r rootConfig) configured() bool {
	return r.ID != "" && r.Name != "" &&
		!strings.HasPrefix(r.ID, placeholderPrefix) && !strings.HasPrefix(r.Name, placeholderPrefix)
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
	PackingFolder rootConfig `json:"packing-folder" doc:"a My-Drive folder outside the crawl root holding users' Packing folders"`
	// DropoffFolder is the shared-drive folder a user drags their Container
	// into; the drag flips ownership of the container's whole tree to the org.
	DropoffFolder rootConfig `json:"dropoff-folder" doc:"the shared-drive folder users drag their Container into"`
	// SkipUnmovable mirrors the pack command's --skip-unmovable flag: skip
	// crawled items the crawling account cannot edit (they cannot be moved) and
	// pack the rest instead of aborting. Either the flag or this setting enables
	// the behavior.
	SkipUnmovable bool `json:"skip-unmovable" doc:"set true to skip uneditable items and pack the rest instead of aborting (same as pack's --skip-unmovable)"`
	// OwnershipTransferAccounts lists the email addresses that files may be
	// transferred to during the migration. The crawl keeps a node's
	// original_owner_* pointed at the most recent owner that is NOT one of these,
	// so once a file is handed to a transfer account the original owner stays
	// frozen at the last real owner.
	OwnershipTransferAccounts []string `json:"ownership-transfer-accounts" doc:"emails files may be transferred to; crawl freezes original_owner at the last owner that is not one of these"`
	// ManualOwnershipTransferAccounts is the subset of OwnershipTransferAccounts
	// whose transfers are performed by hand. A manual transfer permanently bumps
	// the file's modifiedTime, so a file crawled while owned by one of these is
	// flagged manual_transfer_performed, and `crawl` reads its last real edit from
	// the most recent revision rather than modifiedTime (an ownership transfer
	// does not create a revision).
	ManualOwnershipTransferAccounts []string `json:"manual-ownership-transfer-accounts" doc:"subset of ownership-transfer-accounts whose transfers bump modifiedTime; crawl uses the most recent revision for their files"`
}

// archiveConfig holds settings for the archive/delete/restore commands.
type archiveConfig struct {
	// Root is the folder the archive command moves soft-deleted files into,
	// recreating the crawl root's folder structure beneath it as "ARCH "-prefixed
	// replicas. It MUST be outside the crawl root (inside it, the archive would
	// inherit the crawl root's sharing and the archive command refuses to run)
	// and must be a regular My-Drive folder, never in a shared drive (archived
	// files may still be owned by third parties, which a shared drive cannot
	// hold). When configured, the crawl command also crawls this tree — after
	// the crawl root — so archived files stay packable; the review UI and
	// keep-recent exclude it from decision marking.
	Root rootConfig `json:"root" doc:"a My-Drive folder outside the crawl root that archived (soft-deleted) files are moved into"`
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

// optionalArchiveRootID returns archive.root.id when the config file exists
// and the section is filled in, and "" when the file is absent or the section
// is not configured. Database-only commands (review, export-review,
// keep-recent) use it to exclude the archive tree from decision marking
// without making config.json mandatory for them.
func optionalArchiveRootID(path string) (string, error) {
	cfg, err := loadConfig(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !cfg.Archive.Root.configured() {
		return "", nil
	}
	return cfg.Archive.Root.ID, nil
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
			PackingFolder:                   folder("PACKING"),
			DropoffFolder:                   folder("DROPOFF"),
			SkipUnmovable:                   false,
			OwnershipTransferAccounts:       []string{},
			ManualOwnershipTransferAccounts: []string{},
		},
		Archive: archiveConfig{Root: folder("ARCHIVE")},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("rendering config template: %w", err)
	}
	return string(b) + "\n", nil
}
