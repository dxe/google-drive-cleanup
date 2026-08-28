package main

// The delete command permanently deletes items that archive already
// soft-deleted (only rows with a recorded original parent are ever touched).
// Ownership decides the mechanics:
//
//   - items the running account owns are deleted directly;
//   - items owned by other INTERNAL accounts (config internal-domains) cannot
//     be deleted directly, but moving them into a shared drive flips ownership
//     to the org — they go through a "Deletion pending" folder inside the
//     dropoff shared drive and are deleted there;
//   - EXTERNALLY-owned items are skipped (and flagged delete_skipped) unless
//     --remove-unowned is given, which removes them from their folder — Drive
//     relocates them to their owner's My Drive — and then drops every direct
//     permission, the running account's own last.
//
// Empty replica folders are pruned afterwards. A database row is removed only
// once Drive confirms the item is gone — deleted, removed from our tree, or
// reported absent by a live read; every other result (an API error, an
// interrupted run, a skip) keeps the row so the next run retries the item. Each
// handler says which happened by returning an itemOutcome, and dropRowIfGone is
// the one place that acts on it. Run pack/unpack before delete to transfer
// internal ownership to the org and shrink the dropoff bucket.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
)

// deletionPendingFolderName is the working folder created inside the dropoff
// shared drive for internally-owned items: moving them there flips their
// ownership to the org so they become deletable. Removed again when empty.
const deletionPendingFolderName = "Deletion pending"

var deleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Permanently delete archived items (see archive)",
	Long: `Permanently delete items the archive command already moved into the archive
tree. Only archived items (with their original parent recorded) that are still
marked delete are touched, and a confirmation with counts is asked first.

Items owned by the running account are deleted directly. Items owned by other
internal accounts (config internal-domains) are moved into a "Deletion pending"
folder inside the dropoff shared drive — the move flips their ownership to the
org — and deleted there; this requires the Google Workspace privilege "Move any
file or folder into shared drives" (see the README). Externally-owned items are
skipped and counted unless --remove-unowned is given, which removes them from
their archive folder (Drive relocates them to their owner's My Drive, sharing
intact) and then drops every direct permission, the running account's own last.

Empty "ARCH " replica folders are pruned afterwards. Drive listings are
eventually consistent, so a folder that just emptied may be skipped once —
re-run delete to pick it up. Pass --folder <id> (a folder inside the archive
tree, or a crawled folder whose replica exists) to delete only that subtree.

This command requires the full Drive scope. If the cached token.json only has
read-only access, the tool re-runs consent automatically to obtain it.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		removeUnowned, _ := cmd.Flags().GetBool("remove-unowned")
		subfolder, _ := cmd.Flags().GetString("folder")
		maxErrors, _ := cmd.Flags().GetInt("max-errors")
		concurrency, _ := cmd.Flags().GetInt("concurrency")
		return runDelete(dbPath, cfgPath, subfolder, dryRun, removeUnowned, maxErrors, concurrency)
	},
}

func init() {
	deleteCmd.Flags().Bool("dry-run", false, "report what would be deleted without changing anything (read-only scope)")
	deleteCmd.Flags().Bool("remove-unowned", false, "remove externally-owned archived items from their folders (they land in their owner's My Drive) and drop their direct permissions, instead of skipping them")
	deleteCmd.Flags().String("folder", "", "Google Drive folder ID to delete only that subtree of the archive (an archive-tree folder, or a crawled folder whose replica exists)")
	deleteCmd.Flags().Int("max-errors", 5, "abort once more than this many items fail")
	deleteCmd.Flags().Int("concurrency", defaultMoveConcurrency, "how many deletions to run in parallel (all still share the global rate limiter)")
}

// ownerClass buckets an archived item by what delete can do with it.
type ownerClass int

const (
	ownerMine     ownerClass = iota // running account owns it: delete directly
	ownerInternal                   // another internal account: via the dropoff shared drive
	ownerExternal                   // outside the org: skip, or --remove-unowned
)

func classifyOwner(t archiveTarget, me *drive.User, internalDomains []string) ownerClass {
	if t.ownerEmail.Valid && strings.EqualFold(t.ownerEmail.String, me.EmailAddress) {
		return ownerMine
	}
	if t.ownerID.Valid && me.PermissionId != "" && t.ownerID.String == me.PermissionId {
		return ownerMine
	}
	if isInternalEmail(t.ownerEmail, internalDomains) {
		return ownerInternal
	}
	return ownerExternal
}

// itemOutcome is what one item's handler managed to do with it on the Drive
// side. It is the sole gate on removing the item's database row: only
// outcomeGone — the Drive object was deleted, or removed from our tree, or a
// live read proved it no longer exists — may drop a row. Anything else keeps
// the row so a later run retries the item. Keeping the decision in one type,
// returned by every handler and acted on in one place (see dropRowIfGone),
// is what stops a future branch from quietly dropping a row for an item Drive
// still holds.
type itemOutcome int

const (
	outcomeFailed  itemOutcome = iota // Drive still holds it, or we could not tell
	outcomeGone                       // confirmed removed from Drive, or confirmed already absent
	outcomeSkipped                    // deliberately left alone (dry run, externally owned, re-queued)
)

// deleteStats holds a delete run's tallies, updated from the worker pool.
type deleteStats struct {
	*errorBudget
	mu         sync.Mutex
	deleted    int
	viaDropoff int
	removed    int // externally-owned items removed from their folders (--remove-unowned)
	skipped    int // externally-owned items skipped (no --remove-unowned)
	notEmpty   int
	pruned     int
}

func (s *deleteStats) del()          { s.mu.Lock(); s.deleted++; s.mu.Unlock() }
func (s *deleteStats) dropoff()      { s.mu.Lock(); s.viaDropoff++; s.mu.Unlock() }
func (s *deleteStats) remove()       { s.mu.Lock(); s.removed++; s.mu.Unlock() }
func (s *deleteStats) skip()         { s.mu.Lock(); s.skipped++; s.mu.Unlock() }
func (s *deleteStats) skipFull()     { s.mu.Lock(); s.notEmpty++; s.mu.Unlock() }
func (s *deleteStats) prune()        { s.mu.Lock(); s.pruned++; s.mu.Unlock() }
func (s *deleteStats) delCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.deleted }
func (s *deleteStats) externalCounts() (removed, skipped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removed, s.skipped
}

func runDelete(dbPath, cfgPath, subfolder string, dryRun, removeUnowned bool, maxErrors, concurrency int) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Archive.Root.validate("archive.root"); err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	// The dropoff folder is validated eagerly even though it is only used for
	// internally-owned items, so a broken config fails before anything deletes.
	if err := cfg.Migration.DropoffFolder.validate("migration.dropoff-folder"); err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	crawlRoot, err := crawlRootDriveID(db)
	if err == sql.ErrNoRows {
		return fmt.Errorf("database is empty; run crawl first")
	}
	if err != nil {
		return err
	}
	if crawlRoot != cfg.Crawl.Root.ID {
		return fmt.Errorf("crawl root in config (%s, %q) does not match the root in the database (%s); re-run `drive-cleanup crawl` before deleting",
			cfg.Crawl.Root.ID, cfg.Crawl.Root.Name, crawlRoot)
	}

	// --folder scopes the run to one subtree of the archive. A folder ID from
	// the original tree is what the operator naturally has (the same ID used
	// for `archive --folder`), so when the folder has a replica the scope is
	// the replica's subtree — that is where its archived contents live — plus
	// the folder's own row, which (once archived) sits beside its replica in
	// the parent's replica rather than inside it. An archive-tree folder ID
	// (a replica, or an archived folder without one) scopes as-is.
	selfFolder := ""
	if subfolder != "" {
		var replica sql.NullString
		err := db.QueryRow(`SELECT archive_folder_drive_id FROM nodes WHERE drive_id = ?`, subfolder).Scan(&replica)
		if err == sql.ErrNoRows {
			return fmt.Errorf("folder %s not found in the database", subfolder)
		}
		if err != nil {
			return err
		}
		inArchive, err := nodeInSubtree(db, cfg.Archive.Root.ID, subfolder)
		if err != nil {
			return err
		}
		switch {
		case replica.Valid:
			log.Printf("scoping to the archive replica %s of folder %s", replica.String, subfolder)
			selfFolder = subfolder
			subfolder = replica.String
		case inArchive:
			// Already an archive-tree folder; scope to its subtree directly.
		default:
			return fmt.Errorf("folder %s is not inside the archive tree and has no archive replica; delete only acts on archived items", subfolder)
		}
	}

	files, folders, err := archivedForDeletion(db, subfolder)
	if err != nil {
		return err
	}
	if selfFolder != "" {
		// The scoped folder itself, when archived and still marked delete, is
		// deleted too — appended last, since everything in its replica must go
		// before it can be empty.
		_, selfFolders, err := archivedForDeletion(db, selfFolder)
		if err != nil {
			return err
		}
		for _, t := range selfFolders {
			if t.driveID == selfFolder {
				folders = append(folders, t)
			}
		}
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
	me := about.User
	rec := &opLog{db: db, account: me.EmailAddress, command: "delete"}

	dropoff, err := getConfiguredFolder(ctx, svc, cfg.Migration.DropoffFolder, "migration.dropoff-folder")
	if err != nil {
		return err
	}
	if dropoff.DriveId == "" {
		return fmt.Errorf("dropoff folder %s is not in a shared drive; moving internally-owned items there would not flip their ownership to the org", dropoff.Id)
	}

	// Split the work by owner class up front, for the confirmation counts and
	// because each class takes a different path.
	var mineF, internalF, externalF []archiveTarget
	for _, t := range files {
		switch classifyOwner(t, me, cfg.InternalDomains) {
		case ownerMine:
			mineF = append(mineF, t)
		case ownerInternal:
			internalF = append(internalF, t)
		default:
			externalF = append(externalF, t)
		}
	}
	var externalFolders int
	for _, t := range folders {
		if classifyOwner(t, me, cfg.InternalDomains) == ownerExternal {
			externalFolders++
		}
	}

	if len(files)+len(folders) == 0 {
		fmt.Fprintln(os.Stderr, "Nothing archived and marked delete; nothing to do. (Run archive first.)")
		return nil
	}

	externalNote := "skipped; re-run with --remove-unowned to remove them from their folders"
	if removeUnowned {
		externalNote = "removed from their folders and unshared (--remove-unowned)"
	}
	summaryLine := fmt.Sprintf(
		"%d archived file(s) and %d archived folder(s): %d owned by %s, %d owned by internal accounts (via the %q shared drive), %d externally owned (%s).",
		len(files), len(folders), len(mineF), me.EmailAddress, len(internalF), dropoff.Name, len(externalF)+externalFolders, externalNote)
	if dryRun {
		fmt.Fprintf(os.Stderr, "DRY RUN: no changes will be made. Would permanently delete %s\n", summaryLine)
	} else {
		fmt.Fprintf(os.Stderr, "About to PERMANENTLY delete %s\nThis cannot be undone.\n", summaryLine)
		if !promptYesNo("Continue? [y/N] ") {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return nil
		}
	}

	limiter := rate.NewLimiter(rate.Limit(20), 20)

	deleteCtx, deleteCancel := context.WithCancel(ctx)
	defer deleteCancel()
	stats := &deleteStats{errorBudget: &errorBudget{cmd: "delete", maxErrors: maxErrors, cancel: deleteCancel}}
	workers := concurrency
	if dryRun {
		workers = 1
	}
	prog := newProgress()

	// deletionPending lazily finds or creates the working folder in the dropoff
	// shared drive; created only if an internally-owned item actually needs it.
	var deletionPending *drive.File
	ensureDeletionPending := func(ctx context.Context) (*drive.File, error) {
		if deletionPending != nil {
			return deletionPending, nil
		}
		f, err := findChildFolder(ctx, svc, limiter, dropoff.Id, deletionPendingFolderName)
		if err != nil {
			return nil, fmt.Errorf("looking up %q in %q: %w", deletionPendingFolderName, dropoff.Name, err)
		}
		if f == nil {
			if f, err = rec.createFolder(ctx, svc, limiter, dropoff.Id, deletionPendingFolderName); err != nil {
				return nil, fmt.Errorf("creating %q in %q: %w", deletionPendingFolderName, dropoff.Name, err)
			}
		}
		deletionPending = f
		return f, nil
	}

	// dropRowIfGone removes an item's database row, and only when its handler
	// confirmed Drive no longer holds it. Every node row this command removes
	// for a deleted item goes through here, so a Drive failure can never leave
	// the database claiming the item was dealt with: the row survives and the
	// next run retries it. A failure of the delete itself is per-item, not fatal
	// (the row lingers and the next run's 404 handling cleans it up).
	dropRowIfGone := func(t archiveTarget, outcome itemOutcome) {
		if outcome != outcomeGone {
			return
		}
		if err := deleteNodeRow(db, t.driveID); err != nil {
			log.Printf("WARN removing database row of deleted %q (%s): %v", t.name, t.driveID, err)
		}
	}

	// deleteOwned deletes an item the running account owns.
	deleteOwned := func(ctx context.Context, t archiveTarget) itemOutcome {
		if dryRun {
			log.Printf("WOULD delete %s %q (%s), owned by this account", t.typ, t.name, t.driveID)
			stats.del()
			return outcomeSkipped
		}
		err := rec.deleteFile(ctx, svc, limiter, t.driveID)
		if err == nil || isNotFound(err) {
			// A 404 from files.delete means the object is not there — the end
			// state we wanted — so the row goes either way.
			detailf("OK deleted %q (%s)", t.name, t.driveID)
			stats.del()
			return outcomeGone
		}
		if ctx.Err() == nil {
			stats.fail("ERROR deleting %q (%s): %v", t.name, t.driveID, err)
		}
		return outcomeFailed
	}

	// deleteViaDropoff handles an internally-owned item: move it into the
	// Deletion pending folder (the move into the shared drive flips ownership
	// to the org; requires the Workspace "Move any file or folder into shared
	// drives" privilege), then delete it there. Runs sequentially.
	deleteViaDropoff := func(ctx context.Context, t archiveTarget) itemOutcome {
		if dryRun {
			log.Printf("WOULD move %s %q (%s), owned by %s, into %q in the dropoff shared drive and delete it there",
				t.typ, t.name, t.driveID, t.ownerEmail.String, deletionPendingFolderName)
			stats.dropoff()
			return outcomeSkipped
		}
		dp, err := ensureDeletionPending(ctx)
		if err != nil {
			stats.fail("ERROR %q (%s): %v", t.name, t.driveID, err)
			return outcomeFailed
		}
		// Optimistic move from the recorded parent (the archive replica);
		// diagnose from live state on failure, pack-style.
		err = rec.moveFileVerified(ctx, svc, limiter, t.driveID, dp.Id, t.parentDriveID.String)
		if err != nil && ctx.Err() == nil {
			f, gerr := getFileState(ctx, svc, limiter, t.driveID)
			switch {
			case isNotFound(gerr):
				detailf("OK %q (%s): no longer exists", t.name, t.driveID)
				stats.del()
				return outcomeGone
			case gerr != nil:
				stats.fail("ERROR %q (%s): move to %q failed (%v) and live lookup failed (%v)", t.name, t.driveID, deletionPendingFolderName, err, gerr)
				return outcomeFailed
			case hasParent(f, dp.Id):
				// Already moved by an interrupted run; carry on to the delete.
			default:
				if err = rec.moveFileVerified(ctx, svc, limiter, t.driveID, dp.Id, strings.Join(f.Parents, ",")); err != nil {
					if ctx.Err() == nil {
						stats.fail("ERROR moving %q (%s) into %q: %v (this needs the Workspace privilege \"Move any file or folder into shared drives\")",
							t.name, t.driveID, deletionPendingFolderName, err)
					}
					return outcomeFailed
				}
			}
		}
		if ctx.Err() != nil {
			return outcomeFailed
		}
		if err := rec.deleteFile(ctx, svc, limiter, t.driveID); err != nil && !isNotFound(err) {
			stats.fail("ERROR deleting %q (%s) from %q (it is stranded there): %v", t.name, t.driveID, deletionPendingFolderName, err)
			return outcomeFailed
		}
		detailf("OK deleted %q (%s) via %q", t.name, t.driveID, deletionPendingFolderName)
		stats.dropoff()
		return outcomeGone
	}

	// handleExternal skips (and flags) an externally-owned item, or with
	// --remove-unowned removes it from its folder — Drive relocates it to its
	// owner's My Drive, sharing intact — and then drops every direct
	// permission it can, the running account's own last.
	handleExternal := func(ctx context.Context, t archiveTarget) itemOutcome {
		owner := "(unknown)"
		if t.ownerEmail.Valid {
			owner = t.ownerEmail.String
		}
		if !removeUnowned {
			if dryRun {
				log.Printf("WOULD skip %s %q (%s): externally owned by %s", t.typ, t.name, t.driveID, owner)
			} else {
				detailf("SKIP %q (%s): externally owned by %s", t.name, t.driveID, owner)
				if err := markDeleteSkipped(db, t.driveID); err != nil {
					log.Printf("WARN flagging skipped %q (%s): %v", t.name, t.driveID, err)
				}
			}
			stats.skip()
			return outcomeSkipped
		}
		if dryRun {
			log.Printf("WOULD remove %s %q (%s), externally owned by %s, from its folder (it lands in the owner's My Drive) and drop its direct permissions",
				t.typ, t.name, t.driveID, owner)
			stats.remove()
			return outcomeSkipped
		}
		f, err := getFileState(ctx, svc, limiter, t.driveID)
		if isNotFound(err) {
			detailf("OK %q (%s): no longer exists", t.name, t.driveID)
			stats.del()
			return outcomeGone
		}
		if err != nil {
			if ctx.Err() == nil {
				stats.fail("ERROR fetching external %q (%s): %v", t.name, t.driveID, err)
			}
			return outcomeFailed
		}
		// Nothing to detach when Drive reports no parent we can see: the item is
		// already out of our tree, which is the end state either way. Remember
		// which it was so the closing line does not claim a removal that never
		// happened.
		detached := len(f.Parents) > 0
		if detached {
			// A 404 means the item (or the parent we were detaching it from) is
			// already gone between the read above and now — the end state we
			// wanted, so take it as done.
			if err := rec.removeFromParent(ctx, svc, limiter, t.driveID, strings.Join(f.Parents, ",")); err != nil && !isNotFound(err) {
				if ctx.Err() == nil {
					stats.fail("ERROR removing external %q (%s) from its folder: %v", t.name, t.driveID, err)
				}
				return outcomeFailed
			}
		}
		// Best-effort permission cleanup; the item is out of our tree either way.
		// Our own permission goes last — it is what lets us revoke the others.
		//
		// A 404 here is the success case, not a failure: detaching the item
		// commonly takes our own access with it (our only grant was inherited
		// from the folder it sat in), and the owner may also have deleted it
		// meanwhile. Either way it is out of our reach with nothing left to
		// revoke, so say so plainly instead of warning.
		unreachable := false
		perms, err := listPermissions(ctx, svc, limiter, t.driveID)
		switch {
		case isNotFound(err):
			unreachable, perms = true, nil
		case err != nil:
			log.Printf("WARN listing permissions on removed %q (%s): %v", t.name, t.driveID, err)
			perms = nil
		}
		var selfID string
		for _, p := range perms {
			if p.Deleted || p.Role == "owner" {
				continue
			}
			if p.Type == "user" && (p.Id == me.PermissionId || strings.EqualFold(p.EmailAddress, me.EmailAddress)) {
				selfID = p.Id
				continue
			}
			// A 404 means that grant (or the item) is already gone — the end
			// state we were after.
			if err := rec.revokePermission(ctx, svc, limiter, t.driveID, p.Id); err != nil && !isNotFound(err) {
				log.Printf("WARN revoking permission of %s on removed %q (%s): %v", p.EmailAddress, t.name, t.driveID, err)
			}
		}
		if selfID != "" {
			if err := rec.revokePermission(ctx, svc, limiter, t.driveID, selfID); err != nil && !isNotFound(err) {
				log.Printf("WARN revoking own permission on removed %q (%s): %v", t.name, t.driveID, err)
			}
		}
		switch {
		case unreachable && detached:
			detailf("OK removed external %q (%s) from its folder; this account can no longer see it", t.name, t.driveID)
		case unreachable:
			detailf("OK external %q (%s) is no longer visible to this account", t.name, t.driveID)
		case detached:
			detailf("OK removed external %q (%s) from its folder", t.name, t.driveID)
		default:
			detailf("OK external %q (%s) was already outside every folder we can see", t.name, t.driveID)
		}
		stats.remove()
		return outcomeGone
	}

	interrupted := func() bool {
		if ctx.Err() == nil {
			return false
		}
		log.Printf("interrupted: %d deleted, %d via %q, %d removed, %d skipped, %d failed",
			stats.deleted, stats.viaDropoff, deletionPendingFolderName, stats.removed, stats.skipped, stats.failed)
		return true
	}
	checkpoint := func() error {
		if interrupted() {
			return ctx.Err()
		}
		if stats.aborted {
			return stats.err
		}
		return nil
	}

	// Phase 1: files owned by this account, concurrently. Items whose live
	// owner turns out to have changed are re-routed to the internal queue,
	// which many workers may hit at once — hence the mutex; phase 3 reads the
	// queue only after this pool has drained.
	var internalMu sync.Mutex
	deleteMine := func(t archiveTarget) itemOutcome {
		if dryRun {
			return deleteOwned(deleteCtx, t)
		}
		err := rec.deleteFile(deleteCtx, svc, limiter, t.driveID)
		if err == nil || isNotFound(err) {
			detailf("OK deleted %q (%s)", t.name, t.driveID)
			stats.del()
			return outcomeGone
		}
		if deleteCtx.Err() != nil {
			return outcomeFailed
		}
		// The database says we own it, Drive disagrees (stale snapshot). If the
		// live owner is internal the dropoff path still works; queue it there.
		f, gerr := getFileState(deleteCtx, svc, limiter, t.driveID)
		switch {
		case deleteCtx.Err() != nil:
			return outcomeFailed
		case isNotFound(gerr):
			stats.del()
			return outcomeGone
		case gerr == nil && len(f.Owners) > 0 && !ownedByAccount(f, me.EmailAddress) &&
			isInternalEmail(nullString(f.Owners[0].EmailAddress), cfg.InternalDomains):
			log.Printf("NOTE %q (%s) is now owned by %s (snapshot was stale); routing it via %q", t.name, t.driveID, f.Owners[0].EmailAddress, deletionPendingFolderName)
			t.ownerEmail = nullString(f.Owners[0].EmailAddress)
			internalMu.Lock()
			internalF = append(internalF, t)
			internalMu.Unlock()
			// Re-queued, not dealt with: phase 3 decides its row's fate.
			return outcomeSkipped
		default:
			stats.fail("ERROR deleting %q (%s): %v", t.name, t.driveID, err)
			return outcomeFailed
		}
	}
	forEachConcurrent(deleteCtx, workers, mineF, func(t archiveTarget) {
		prog.tick("progress: %d/%d owned file(s) deleted", stats.delCount(), len(mineF))
		dropRowIfGone(t, deleteMine(t))
	})
	if err := checkpoint(); err != nil {
		return err
	}

	// Phase 2: externally-owned files, concurrently.
	forEachConcurrent(deleteCtx, workers, externalF, func(t archiveTarget) {
		removed, skipped := stats.externalCounts()
		prog.tick("progress: external file(s): %d removed, %d skipped", removed, skipped)
		dropRowIfGone(t, handleExternal(deleteCtx, t))
	})
	if err := checkpoint(); err != nil {
		return err
	}

	// Phase 3: internally-owned files, sequentially through Deletion pending.
	for _, t := range internalF {
		if deleteCtx.Err() != nil {
			break
		}
		prog.tick("progress: %d internal file(s) via %q", stats.viaDropoff, deletionPendingFolderName)
		dropRowIfGone(t, deleteViaDropoff(deleteCtx, t))
	}
	if err := checkpoint(); err != nil {
		return err
	}

	// Phase 4: archived folders, sequentially and deepest-first, each gated on
	// a live emptiness check (its archived contents were deleted above).
	for _, t := range folders {
		if deleteCtx.Err() != nil {
			break
		}
		if dryRun {
			log.Printf("WOULD delete archived folder %q (%s) once empty", t.name, t.driveID)
			stats.del()
			continue
		}
		empty, err := folderIsEmpty(deleteCtx, svc, limiter, t.driveID)
		if isNotFound(err) {
			dropRowIfGone(t, outcomeGone)
			stats.del()
			continue
		}
		if err != nil {
			if deleteCtx.Err() == nil {
				stats.fail("ERROR checking whether folder %q (%s) is empty: %v", t.name, t.driveID, err)
			}
			continue
		}
		if !empty {
			// Name the contents that are holding it up, and say whether a plain
			// re-run can clear them — see explainNotEmptyForDelete.
			blockers, berr := folderBlockers(deleteCtx, svc, limiter, db, t.driveID)
			if berr != nil {
				if deleteCtx.Err() != nil {
					break
				}
				log.Printf("SKIP folder %q (%s): not empty yet, and listing its contents to say why failed: %v", t.name, t.driveID, berr)
			} else {
				log.Printf("SKIP folder %q (%s): %s", t.name, t.driveID, explainNotEmptyForDelete(blockers))
			}
			stats.skipFull()
			continue
		}
		var outcome itemOutcome
		switch classifyOwner(t, me, cfg.InternalDomains) {
		case ownerMine:
			outcome = deleteOwned(deleteCtx, t)
		case ownerInternal:
			outcome = deleteViaDropoff(deleteCtx, t)
		default:
			outcome = handleExternal(deleteCtx, t)
		}
		dropRowIfGone(t, outcome)
	}
	if err := checkpoint(); err != nil {
		return err
	}

	// Phase 5: prune replica folders that emptied out, deepest-first (a child
	// folder's replica nests inside its parent's), clearing the originals'
	// caches so a later archive re-creates replicas as needed.
	replicas, err := foldersWithReplicas(db)
	if err != nil {
		return err
	}
	var inScope map[string]bool
	if subfolder != "" {
		if inScope, err = subtreeDriveIDs(db, subfolder); err != nil {
			return err
		}
	}
	for _, t := range replicas {
		if deleteCtx.Err() != nil {
			break
		}
		replicaID := t.archiveFolder.String
		if inScope != nil && !inScope[replicaID] {
			continue
		}
		if dryRun {
			log.Printf("WOULD prune replica %q of %q (%s) once empty", replicaName(t.name), t.name, replicaID)
			continue
		}
		// Same rule as the item phases: the replica's row and the original's
		// cached pointer to it are only cleared once Drive confirms the replica
		// folder is gone.
		outcome := outcomeFailed
		empty, err := folderIsEmpty(deleteCtx, svc, limiter, replicaID)
		switch {
		case isNotFound(err):
			// Replica already gone; just clear the bookkeeping.
			outcome = outcomeGone
		case err != nil:
			if deleteCtx.Err() == nil {
				log.Printf("WARN checking replica %s of %q: %v", replicaID, t.name, err)
			}
		case !empty:
			detailf("replica of %q (%s) not empty yet; leaving it", t.name, replicaID)
		default:
			if derr := rec.deleteFile(deleteCtx, svc, limiter, replicaID); derr != nil && !isNotFound(derr) {
				if deleteCtx.Err() == nil {
					log.Printf("WARN pruning empty replica %s of %q: %v", replicaID, t.name, derr)
				}
			} else {
				outcome = outcomeGone
			}
		}
		if outcome != outcomeGone {
			continue
		}
		if err := clearArchiveFolder(db, t.driveID); err != nil {
			log.Printf("WARN clearing replica cache of %q (%s): %v", t.name, t.driveID, err)
		}
		if err := deleteNodeRow(db, replicaID); err != nil {
			log.Printf("WARN removing database row of pruned replica %s: %v", replicaID, err)
		}
		detailf("OK pruned empty replica of %q (%s)", t.name, replicaID)
		stats.prune()
	}

	// Phase 6: remove Deletion pending itself when it was used and emptied out.
	if deletionPending != nil && deleteCtx.Err() == nil {
		empty, err := folderIsEmpty(deleteCtx, svc, limiter, deletionPending.Id)
		switch {
		case isNotFound(err):
			// Already gone (a concurrent run, or someone tidying up): nothing to do.
			detailf("OK %q folder no longer exists", deletionPendingFolderName)
		case err != nil:
			log.Printf("WARN checking %q: %v", deletionPendingFolderName, err)
		case empty:
			if err := rec.deleteFile(deleteCtx, svc, limiter, deletionPending.Id); err != nil && !isNotFound(err) {
				log.Printf("WARN removing empty %q folder: %v", deletionPendingFolderName, err)
			} else {
				detailf("OK removed empty %q folder", deletionPendingFolderName)
			}
		default:
			log.Printf("%q is not empty (stranded items, or Drive's listing lags recent deletions); leaving it", deletionPendingFolderName)
		}
	}
	if interrupted() {
		return ctx.Err()
	}

	verb := "deleted"
	if dryRun {
		verb = "would be deleted"
	}
	log.Printf("done: %d item(s) %s directly, %d via %q, %d externally-owned removed from their folders, %d folder(s) not yet empty, %d replica folder(s) pruned, %d failed",
		stats.deleted, verb, stats.viaDropoff, deletionPendingFolderName, stats.removed, stats.notEmpty, stats.pruned, stats.failed)
	if stats.skipped > 0 {
		fmt.Fprintf(os.Stderr, "%d externally-owned item(s) skipped; re-run with --remove-unowned to remove them from their folders.\n", stats.skipped)
	}
	if stats.failed > 0 {
		return fmt.Errorf("%d item(s) failed; re-run delete to retry", stats.failed)
	}
	return nil
}
