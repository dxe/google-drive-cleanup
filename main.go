// Command google-drive-cleanup crawls a Google Drive folder tree into a
// SQLite database and reports on file ownership. It is the first half of an
// ownership-migration project: we snapshot every file's location and owner
// before moving files through shared drives for ownership transfer, so they
// can be moved back to their original parents afterwards. See README.md.
package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

// rootCmd is the base command. SilenceErrors/SilenceUsage keep cobra from
// printing the error and a usage dump on runtime failures — main() reports the
// error via log.Fatal, and a full usage wall on, say, a Drive API error is
// just noise. Cobra still prints usage for flag/argument parsing errors.
var rootCmd = &cobra.Command{
	Use:   "google-drive-cleanup",
	Short: "Crawl a Google Drive folder tree into SQLite and report on ownership",
	Long: `google-drive-cleanup snapshots every file's location and owner under a
configured root folder into a SQLite database, so files can be moved through
shared drives for ownership transfer and later restored to their original
parents.`,
	SilenceErrors: true,
	SilenceUsage:  true,
}

var ownersCmd = &cobra.Command{
	Use:   "owners",
	Short: "Print each owner and how many files (non-folders) they own",
	Long: `Print each owner and how many files (non-folders) they own, sorted
descending by count — this drives outreach priority.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		return runOwners(dbPath, cfgPath)
	},
}

var pathCmd = &cobra.Command{
	Use:   "path <drive_id>",
	Short: "Print the full folder path of a node by Drive ID",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		return runPath(dbPath, args[0])
	},
}

func init() {
	ownersCmd.Flags().String("db", "drive.db", "path to the SQLite database")
	ownersCmd.Flags().String("config", "config.json", "path to the config JSON")
	pathCmd.Flags().String("db", "drive.db", "path to the SQLite database")
	exploreCmd.Flags().String("db", "drive.db", "path to the SQLite database")
	exploreCmd.Flags().String("out", "out/explore-owned-files", "output directory for the generated HTML")

	rootCmd.AddCommand(crawlCmd, ownersCmd, pathCmd, exploreCmd)
}

func runOwners(dbPath, cfgPath string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	counts, err := ownersReport(db)
	if err != nil {
		return err
	}
	fmt.Printf("%10s %10s %10s  %s\n", "FOLDERS", "FILES", "TOTAL", "OWNER")
	for _, oc := range counts {
		if cfg.Owners.IgnoreInternalDomains && isInternalEmail(oc.email, cfg.InternalDomains) {
			continue
		}
		fmt.Printf("%10d %10d %10d  %s\n", oc.folderCount, oc.fileCount, oc.total, ownerLabel(oc))
	}
	return nil
}

// isInternalEmail reports whether email is non-null and ends with "@" followed
// by one of the internal domains (case-insensitive).
func isInternalEmail(email sql.NullString, internalDomains []string) bool {
	if !email.Valid {
		return false
	}
	at := strings.LastIndex(email.String, "@")
	if at < 0 {
		return false
	}
	domain := strings.ToLower(email.String[at+1:])
	for _, d := range internalDomains {
		if domain == strings.ToLower(d) {
			return true
		}
	}
	return false
}

func ownerLabel(oc ownerCount) string {
	switch {
	case oc.email.Valid:
		if oc.displayName.Valid {
			return fmt.Sprintf("%s (%s)", oc.email.String, oc.displayName.String)
		}
		return oc.email.String
	case oc.ownerID.Valid:
		// No email on these rows; the stable Drive user id is all we have, so
		// include the display name to make it human-readable.
		if oc.displayName.Valid {
			return fmt.Sprintf("id:%s (%s)", oc.ownerID.String, oc.displayName.String)
		}
		return "id:" + oc.ownerID.String
	default:
		return "(unknown)"
	}
}

func runPath(dbPath, driveID string) error {
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	segments, err := nodePath(db, driveID)
	if err != nil {
		return err
	}
	fmt.Println(strings.Join(segments, " / "))
	return nil
}
