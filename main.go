// Command drive-cleanup crawls a Google Drive folder tree into a
// SQLite database and reports on file ownership. It is the first half of an
// ownership-migration project: we snapshot every file's location and owner
// before moving files through shared drives for ownership transfer, so they
// can be moved back to their original parents afterwards. See README.md.
package main

import (
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
	Use:   "drive-cleanup",
	Short: "Crawl a Google Drive folder tree into SQLite and report on ownership",
	Long: `drive-cleanup snapshots every file's location and owner under a
configured root folder into a SQLite database, so files can be moved through
shared drives for ownership transfer and later restored to their original
parents.`,
	SilenceErrors: true,
	SilenceUsage:  true,
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

var checkEditAccessCmd = &cobra.Command{
	Use:   "check-edit-access",
	Short: "List files and folders the crawling account cannot edit",
	Long: `Print every node whose recorded edit capability (Drive's
capabilities.canEdit, captured during crawl) is false — i.e. the account that
ran the crawl lacks edit access. Use this before moving files to confirm you
can actually move them.

This reads only the database; re-run crawl first if it is stale.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		return runCheckEditAccess(dbPath)
	},
}

func init() {
	// --db and --config apply to every subcommand, so they live on the root as
	// persistent flags: defined once here, readable from any command's RunE via
	// cmd.Flags().GetString(...). Command-specific flags (e.g. crawl's --refresh,
	// explore's --out) are registered in each command's own file.
	rootCmd.PersistentFlags().String("db", "drive.db", "path to the SQLite database")
	rootCmd.PersistentFlags().String("config", "config.json", "path to the config JSON")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "log every item touched, not just progress summaries and errors")

	rootCmd.AddCommand(initCmd, crawlCmd, ownersCmd, pathCmd, checkEditAccessCmd, exploreCmd, packCmd, unpackCmd, reviewCmd, exportReviewCmd, keepRecentCmd)
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

func runCheckEditAccess(dbPath string) error {
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := nodesLackingEditAccess(db)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "No files or folders lacking edit access.")
		return nil
	}
	var folders, files int
	for _, r := range rows {
		if r.typ == typeFolder {
			folders++
		} else {
			files++
		}
		fmt.Printf("%-8s %s  [owner: %s]\n", r.typ, r.path, r.owner)
	}
	fmt.Fprintf(os.Stderr, "\n%d item(s) without edit access: %d folder(s), %d file(s).\n", len(rows), folders, files)
	return nil
}
