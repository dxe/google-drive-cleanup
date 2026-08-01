package main

// The keep-recent subcommand: bulk-mark every recently-modified file as keep.
// It reuses the review server's exact bulk-file logic (markManyInTx) so the
// resulting decisions honour the same no-kept-item-inside-a-delete-subtree
// invariant the web UI relies on — only files are marked directly; their folder
// ancestors are re-decided by the rollup, never marked keep on their own.

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

var keepRecentCmd = &cobra.Command{
	Use:   "keep-recent",
	Short: "Mark every file modified within the last N months as keep",
	Long: `Mark every file whose recorded last_modified is within the last N months
(default 6) as keep, exactly as if you had opened each containing folder in the
review UI and clicked "keep" on those files.

Only files are marked. Folder decisions are derived, not set directly: marking a
recent file keep clears any delete decision on its ancestor folders (a kept file
may not live inside a deleted subtree) and rolls fully-decided folders back up,
so the web UI's keep/delete invariants stay intact.

Files with no recorded last_modified (never seen a crawl that recorded it, or an
unparseable value) are left untouched. This reads and writes only the database;
re-run crawl first if it is stale.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		months, _ := cmd.Flags().GetInt("months")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		return runKeepRecent(dbPath, cfgPath, months, dryRun)
	},
}

func init() {
	keepRecentCmd.Flags().Int("months", 6, "treat files modified within this many months as recent")
	keepRecentCmd.Flags().Bool("dry-run", false, "report what would change without writing")
}

func runKeepRecent(dbPath, cfgPath string, months int, dryRun bool) error {
	if months <= 0 {
		return fmt.Errorf("--months must be a positive number of months")
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// Archived items must keep their 'delete' decision (the delete command
	// consumes it), so the archive tree is excluded from bulk keep-marking just
	// as it is hidden from the review UI.
	archiveRootID, err := optionalArchiveRootID(cfgPath)
	if err != nil {
		return err
	}
	inArchive := map[string]bool{}
	if archiveRootID != "" {
		if inArchive, err = subtreeDriveIDs(db, archiveRootID); err != nil {
			return err
		}
	}

	cutoff := time.Now().AddDate(0, -months, 0)

	// Collect the recent files. Filter by parsed time in Go (rather than a SQL
	// string comparison) to match review_export's modTime handling exactly.
	rows, err := db.Query(
		`SELECT drive_id, last_modified FROM nodes
		 WHERE type <> ? AND last_modified IS NOT NULL AND last_modified <> ''`, typeFolder)
	if err != nil {
		return err
	}
	defer rows.Close()
	var recent []string
	for rows.Next() {
		var driveID, lastMod string
		if err := rows.Scan(&driveID, &lastMod); err != nil {
			return err
		}
		if inArchive[driveID] {
			continue
		}
		t, err := time.Parse(time.RFC3339, lastMod)
		if err != nil {
			continue // unparseable time: treat as unknown, leave untouched
		}
		if t.After(cutoff) {
			recent = append(recent, driveID)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if len(recent) == 0 {
		fmt.Fprintf(os.Stderr, "No files modified in the last %s.\n", monthsPhrase(months))
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rec := make(map[int64]string)
	res, matched, err := markManyInTx(tx, recent, decisionKeep, rec)
	if err != nil {
		return err
	}

	if dryRun {
		fmt.Fprintf(os.Stderr,
			"[dry run] %d file(s) modified in the last %s; would change %d node(s), "+
				"clearing %d delete ancestor(s). No changes written.\n",
			matched, monthsPhrase(months), len(rec), res.ClearedAncestors)
		return nil // deferred Rollback discards the transaction
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr,
		"Marked keep: %d file(s) modified in the last %s; %d node(s) changed, "+
			"%d delete ancestor(s) cleared.\n",
		matched, monthsPhrase(months), len(rec), res.ClearedAncestors)
	return nil
}
