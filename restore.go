package main

// The restore command reverses one archival: it moves an archived item from
// the archive tree back under the original parent recorded when archive moved
// it, clears the archival bookkeeping, and marks the item keep so a later
// archive run does not immediately re-archive it. Permissions the archive pass
// removed are NOT restored — re-share the item by hand if needed.

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
)

var restoreCmd = &cobra.Command{
	Use:   "restore <drive_id>",
	Short: "Move an archived item back to its original parent and mark it keep",
	Long: `Move an archived file or folder (see the archive command) from the archive
tree back under the folder it originally lived in, clear its archived state,
and mark it keep — restoring an item is a decision to keep it, and leaving it
marked delete would just re-archive it on the next archive run.

The item is looked up by its Google Drive ID and must have been archived by
this tool (the database records the original parent). Permissions removed
during archiving are not restored; re-share the item by hand if needed. Neither
is ownership: a file whose ownership archive transferred to the org comes back
owned by the running account.

This command requires the full Drive scope. If the cached token.json only has
read-only access, the tool re-runs consent automatically to obtain it.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		return runRestore(dbPath, cfgPath, args[0], dryRun)
	},
}

func init() {
	restoreCmd.Flags().Bool("dry-run", false, "report what would be restored without changing anything (read-only scope)")
}

func runRestore(dbPath, cfgPath, driveID string, dryRun bool) error {
	// The config is loaded only to fail fast on a broken file; restore itself
	// needs nothing from it (the original parent is recorded in the database).
	if _, err := loadConfig(cfgPath); err != nil {
		return err
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var (
		name           string
		typ            string
		originalParent sql.NullString
	)
	err = db.QueryRow(
		`SELECT name, type, original_parent_drive_id FROM nodes WHERE drive_id = ?`, driveID).
		Scan(&name, &typ, &originalParent)
	if err == sql.ErrNoRows {
		return fmt.Errorf("drive id %s not found in the database", driveID)
	}
	if err != nil {
		return err
	}
	if !originalParent.Valid {
		return fmt.Errorf("%q (%s) is not archived (no original parent recorded); nothing to restore", name, driveID)
	}

	ctx, cancel := cancelOnSignal()
	defer cancel()

	scope := drive.DriveScope
	if dryRun {
		scope = drive.DriveReadonlyScope
	}
	svc, err := newDriveService(ctx, scope)
	if err != nil {
		return err
	}
	about, err := svc.About.Get().Fields("user").Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("fetching current user info: %w", err)
	}
	rec := &opLog{db: db, account: about.User.EmailAddress, command: "restore"}
	limiter := rate.NewLimiter(rate.Limit(20), 20)

	// The original parent must still exist to receive the item; fail loudly
	// otherwise and change nothing — the item stays findable in the archive.
	parent, err := getFileState(ctx, svc, limiter, originalParent.String)
	if isNotFound(err) {
		return fmt.Errorf("original parent %s of %q no longer exists on Drive; move the item out of the archive by hand instead", originalParent.String, name)
	}
	if err != nil {
		return fmt.Errorf("fetching original parent %s: %w", originalParent.String, err)
	}
	if parent.Trashed {
		return fmt.Errorf("original parent %q (%s) is in the trash; restore it first, or move the item by hand", parent.Name, parent.Id)
	}

	item, err := getFileState(ctx, svc, limiter, driveID)
	if isNotFound(err) {
		return fmt.Errorf("%q (%s) no longer exists on Drive; nothing to restore (run delete to clean up its database row)", name, driveID)
	}
	if err != nil {
		return fmt.Errorf("fetching %s: %w", driveID, err)
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "DRY RUN: no changes will be made.\n")
		log.Printf("WOULD move %s %q (%s) from %v back under %q (%s) and mark it keep",
			typ, name, driveID, item.Parents, parent.Name, parent.Id)
		return nil
	}

	if hasParent(item, originalParent.String) {
		log.Printf("%q (%s) is already under its original parent %q; fixing the database only", name, driveID, parent.Name)
	} else if err := rec.moveFileVerified(ctx, svc, limiter, driveID, originalParent.String, strings.Join(item.Parents, ",")); err != nil {
		return fmt.Errorf("moving %q (%s) back under %q (%s): %w", name, driveID, parent.Name, parent.Id, err)
	}

	if err := clearArchived(db, driveID, originalParent.String); err != nil {
		return fmt.Errorf("clearing the archived state of %s: %w", driveID, err)
	}

	// Restoring is a decision to keep: mark it so (with the review UI's own
	// propagation, so folder rollups stay consistent) instead of leaving the
	// item marked delete and due for re-archiving.
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := markInTx(tx, driveID, decisionKeep, "overwrite", map[int64]string{}); err != nil {
		return fmt.Errorf("marking %s keep: %w", driveID, err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	log.Printf("restored %s %q (%s) under %q (%s) and marked it keep", typ, name, driveID, parent.Name, parent.Id)
	return nil
}
