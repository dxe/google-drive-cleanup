package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
)

var unpackCmd = &cobra.Command{
	Use:   "unpack <user>",
	Short: "Restore a dragged Container's contents (and the Stash) to their original locations, then clean up",
	Long: `Finish <user>'s migration after they drag their Container into the shared-drive
dropoff folder (which flips ownership of everything inside to the org).

The Container is located live as the child named <user>-Container, not by the
ID recorded at pack time, in case it is manually re-created by the admin.
Normally it is found in the dropoff folder (the shared drive); with
--allow-not-moved it is found in the Pickup folder inside the per-user packing
folder instead (where it still sits, un-dragged).
If what is found differs from the recorded ID, unpack asks for confirmation
before proceeding. If no such folder is found in the expected place, unpack
errors out (the Container is in the wrong location or missing) rather than
falling back to the recorded ID.

Before restoring anything, unpack verifies that the drag's ownership transfer
— which Drive applies asynchronously, item by item — has reached everything
inside the Container, and waits for stragglers to flip. Restoring too early
would move an item back out of the shared drive before its ownership flipped,
silently leaving it owned by the migrating user in the middle of the org tree.

Each direct child of the Container is moved back to the original parent
recorded in the database (owned items nested deeper ride along with their
folders), then each Stash item is returned the same way. The Container must be
restored first: Stash items are owned by third parties, and a shared drive
cannot hold those, so their destination folders have to be back out of the
shared drive before they can follow.

Items that cannot be placed — not in the database, or their original parent is
gone — are quarantined under <packing-folder>/<user>/Errors/<original parent
id>/ for manual restore instead of blocking cleanup. Once the Stash and
Container are verified empty they are deleted, along with the Pickup folder
(which also removes the user's read access on it) and the per-user folder if
nothing (such as a non-empty Errors folder) remains in it. The Manager access
pack granted the user on the dropoff folder is then revoked.

This command requires the full Drive scope and, to move items out of the shared
drive, manager access on it.

If the user became unavailable and never dragged the Container into the shared
drive, pass --allow-not-moved to abort the migration: files are restored to
their original locations straight from the packing folder so they are usable
again, and pack can be re-run later to retry. Ownership never transferred in
that case, so the database owner columns are left unchanged. The user's dropoff
access is still revoked once the Stash is clear (a re-pack re-grants it), so they
cannot see the next user's files migrating through the same folder.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		maxErrors, _ := cmd.Flags().GetInt("max-errors")
		allowNotMoved, _ := cmd.Flags().GetBool("allow-not-moved")
		ignoreUnflipped, _ := cmd.Flags().GetBool("ignore-unflipped")
		concurrency, _ := cmd.Flags().GetInt("concurrency")
		return runUnpack(dbPath, cfgPath, args[0], dryRun, maxErrors, allowNotMoved, ignoreUnflipped, concurrency)
	},
}

func init() {
	unpackCmd.Flags().Bool("dry-run", false, "report what would move without changing anything (read-only scope)")
	unpackCmd.Flags().Int("max-errors", 5, "abort once more than this many items fail to move")
	unpackCmd.Flags().Bool("allow-not-moved", false, "abort the migration: restore files even if the Container was never dragged into the shared drive (ownership never flipped, so the database owner columns are left unchanged)")
	unpackCmd.Flags().Bool("ignore-unflipped", false, "unpack even if some items in the Container never had their ownership flipped to the org (restores them still owned by the migrating user; they are recorded as migrated anyway)")
	unpackCmd.Flags().Int("concurrency", defaultMoveConcurrency, "how many file moves to run in parallel (all still share the global rate limiter)")
}

// restoreStats holds an unpack run's running tallies. restoreChildren updates it
// from its worker pool, so the counters are guarded by mu; the shared error
// budget (embedded) carries the failure count and abort logic.
type restoreStats struct {
	*errorBudget
	mu          sync.Mutex
	restored    int
	quarantined int
}

func (s *restoreStats) incRestored()    { s.mu.Lock(); s.restored++; s.mu.Unlock() }
func (s *restoreStats) incQuarantined() { s.mu.Lock(); s.quarantined++; s.mu.Unlock() }

func (s *restoreStats) restoredCount() int    { s.mu.Lock(); defer s.mu.Unlock(); return s.restored }
func (s *restoreStats) quarantinedCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.quarantined }

// processed is a best-effort count for the progress heartbeat.
func (s *restoreStats) processed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restored + s.quarantined
}

// Dragging the Container into the shared drive flips ownership of everything
// inside to the org, but Drive applies the flip asynchronously, item by item —
// a tree dragged moments ago can be only partially transferred (seen live:
// five files deep in a dragged Container still owned by the migrating user
// minutes later, while their siblings had flipped). Unpack polls until the
// tree is clean before restoring; these bound the wait.
const (
	flipPollInterval = 15 * time.Second
	flipWaitTimeout  = 5 * time.Minute
)

// unflippedItems walks the Container tree in the shared drive and returns
// every item still owned by account, i.e. items the drag's asynchronous
// ownership transfer has not reached yet. Items the flip has processed report
// no owners at all (shared-drive items have none), so any item still naming
// the migrating user is pending. files.list can lag reality, but only ever
// toward the OLD state — it cannot report a flip that has not happened — so a
// clean walk is trustworthy, and a stale positive merely waits a little longer.
func unflippedItems(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, rootID, account string) ([]*drive.File, error) {
	var unflipped []*drive.File
	queue := []string{rootID}
	for len(queue) > 0 {
		folderID := queue[0]
		queue = queue[1:]
		children, err := listChildren(ctx, svc, limiter, folderID,
			"nextPageToken, files(id, name, mimeType, owners(emailAddress, permissionId))")
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", folderID, err)
		}
		for _, c := range children {
			if ownedByAccount(c, account) {
				unflipped = append(unflipped, c)
			}
			if c.MimeType == folderMimeType {
				queue = append(queue, c.Id)
			}
		}
	}
	return unflipped, nil
}

func runUnpack(dbPath, cfgPath, account string, dryRun bool, maxErrors int, allowNotMoved, ignoreUnflipped bool, concurrency int) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Migration.DropoffFolder.validate("migration.dropoff-folder"); err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// rec logs every Drive write below into drive_ops, tagged as this account's
	// unpack run (see opLog).
	rec := &opLog{db: db, account: account, command: "unpack"}

	m, err := getUserMigration(db, account)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("no pack recorded for %q; run pack first", account)
	}
	if !m.packedAt.Valid {
		log.Printf("WARN pack for %q never finished cleanly; some owned items may not be in the Container", account)
		if !dryRun && !promptYesNo("Continue with unpack anyway? [y/N] ") {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return nil
		}
	}

	ctx, cancel := cancelOnSignal()
	defer cancel()

	// A dry run only reads, so request the narrower scope — previewing never
	// forces a write-scope re-consent.
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

	dropoff, err := getConfiguredFolder(ctx, svc, cfg.Migration.DropoffFolder, "migration.dropoff-folder")
	if err != nil {
		return err
	}
	if dropoff.DriveId == "" {
		return fmt.Errorf("dropoff folder %s is not in a shared drive", dropoff.Id)
	}

	// The restores below run concurrently (see --concurrency), but every call
	// still passes through this one shared limiter, so it — not the worker count —
	// is the quota safety cap. See pack for the reasoning; backoff-on-429/403
	// self-throttles if a burst overshoots.
	limiter := rate.NewLimiter(rate.Limit(20), 20)

	// resolveContainer picks the Container to unpack from what was found live by
	// name, rather than trusting the ID recorded at pack time — the drag can
	// substitute a re-created folder, and the live tree is the source of truth
	// for what is about to be unpacked. When the found folder differs from the
	// recorded ID it confirms first (or just notes it, in a dry run). Returns
	// the chosen ID and whether to proceed.
	resolveContainer := func(found *drive.File, location string) (string, bool) {
		if found.Id == m.containerID {
			return found.Id, true
		}
		fmt.Fprintf(os.Stderr, "The %q folder in the %s (id %s) does not match the Container recorded at pack time (id %s).\n",
			containerFolderName(account), location, found.Id, m.containerID)
		if dryRun {
			fmt.Fprintf(os.Stderr, "DRY RUN: would unpack the folder found in the %s (%s).\n", location, found.Id)
			return found.Id, true
		}
		if !promptYesNo(fmt.Sprintf("Unpack the folder found in the %s (%s) instead? [y/N] ", location, found.Id)) {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return "", false
		}
		return found.Id, true
	}

	// After the user drags it in, the Container is a direct child of the dropoff
	// folder (the shared drive), so look it up there.
	containerID := m.containerID
	dropoffChild, err := findChildFolder(ctx, svc, limiter, dropoff.Id, containerFolderName(account))
	if err != nil {
		return fmt.Errorf("looking for the Container in the dropoff folder %s: %w", dropoff.Id, err)
	}
	switch {
	case dropoffChild != nil:
		id, ok := resolveContainer(dropoffChild, fmt.Sprintf("dropoff folder %q", dropoff.Name))
		if !ok {
			return nil
		}
		containerID = id
	case allowNotMoved:
		// Migration abort: the Container was never dragged, so it still lives in
		// the Pickup folder inside the per-user packing folder. Find it there
		// instead of trusting the recorded ID.
		pickupChild, err := findChildFolder(ctx, svc, limiter, m.pickupID, containerFolderName(account))
		if err != nil {
			return fmt.Errorf("looking for the Container in the Pickup folder %s: %w", m.pickupID, err)
		}
		if pickupChild == nil {
			return fmt.Errorf("no %q folder found in the dropoff folder %q or the Pickup folder %s; the Container recorded at pack time (%s) is missing from both", containerFolderName(account), dropoff.Name, m.pickupID, m.containerID)
		}
		id, ok := resolveContainer(pickupChild, "Pickup folder")
		if !ok {
			return nil
		}
		containerID = id
	default:
		// No Container by that name in the dropoff folder means the drag has not
		// happened (or it was dragged somewhere else). The recorded Container is
		// in the wrong location; refuse rather than silently unpacking from it.
		// --allow-not-moved is the deliberate exception handled above.
		return fmt.Errorf("no %q folder found in the dropoff folder %q — the Container recorded at pack time (%s) is not there; it is in the wrong location and must be dragged into the dropoff folder before unpacking (pass --allow-not-moved to abort the migration and restore from the packing folder instead)",
			containerFolderName(account), dropoff.Name, m.containerID)
	}

	// The Container must actually have been dragged into the shared drive, and
	// the running account must be able to move items back out of it.
	container, err := svc.Files.Get(containerID).
		Fields("id, name, driveId, trashed, capabilities(canMoveItemOutOfDrive)").
		SupportsAllDrives(true).
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("fetching Container %s: %w", containerID, err)
	}
	if container.Trashed {
		return fmt.Errorf("Container %s is trashed", containerID)
	}
	// containerInSharedDrive is the normal, post-drag state: ownership of
	// everything inside has flipped to the org. When the Container is still in
	// My Drive the drag never happened; --allow-not-moved lets the admin abort
	// the migration and restore files to their original locations anyway (e.g.
	// the user became unavailable), so they stay put until a retry. Ownership
	// never transferred, so those restores must NOT flip the database owner.
	containerInSharedDrive := container.DriveId != ""
	if !containerInSharedDrive {
		if !allowNotMoved {
			return fmt.Errorf("Container %s is still outside the shared drive — has %s dragged it into %q yet? (pass --allow-not-moved to abort the migration and restore files to their original locations instead)", containerID, account, dropoff.Name)
		}
		log.Printf("WARN Container %s was never dragged into the shared drive; --allow-not-moved set: aborting the migration and restoring files to their original locations. Ownership did not transfer to the org, so files keep their current owners and the database owner columns are left unchanged.", containerID)
	} else {
		if container.DriveId != dropoff.DriveId {
			return fmt.Errorf("Container %s is in shared drive %s, but not the dropoff folder's shared drive %s", containerID, container.DriveId, dropoff.DriveId)
		}
		if container.Capabilities != nil && !container.Capabilities.CanMoveItemOutOfDrive {
			return fmt.Errorf("%s cannot move items out of shared drive %s; it needs manager access", me.EmailAddress, container.DriveId)
		}
	}

	stashF, err := svc.Files.Get(m.stashID).
		Fields("id, name, mimeType, driveId").
		SupportsAllDrives(true).
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("fetching Stash %s: %w", m.stashID, err)
	}
	if stashF.MimeType != folderMimeType || stashF.DriveId != "" {
		return fmt.Errorf("Stash %s is not a My-Drive folder anymore; refusing to continue", m.stashID)
	}

	// After the drag the org owns everything, so restored items end up owned by
	// the running account. When aborting an un-dragged migration, ownership is
	// untouched and items return to their original owners.
	ownership := fmt.Sprintf("owned by: %s", me.EmailAddress)
	if !containerInSharedDrive {
		ownership = "left with their current owners (migration aborted; nothing was dragged)"
	}
	if dryRun {
		fmt.Fprintf(os.Stderr, "DRY RUN: no files will be moved. Restored files would be %s.\n", ownership)
	} else {
		fmt.Fprintf(os.Stderr, "Restored files will be %s.\n", ownership)
		if !promptYesNo("Continue? [y/N] ") {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return nil
		}
	}

	// Verify the drag's ownership transfer actually reached everything inside
	// the Container before moving anything back out (see unflippedItems).
	// Restoring an unflipped item would pull it out of the shared drive still
	// owned by the migrating user — and updateSubtreeOwner below would record
	// it as migrated anyway, hiding the miss until the next crawl. Runs after
	// the confirmation prompt so the poll can wait unattended. A dry run
	// reports the pending items but does not wait.
	if containerInSharedDrive {
		for start := time.Now(); ; {
			unflipped, err := unflippedItems(ctx, svc, limiter, containerID, account)
			if err != nil {
				return fmt.Errorf("verifying the drag's ownership transfer: %w", err)
			}
			if len(unflipped) == 0 {
				break
			}
			for _, f := range unflipped {
				log.Printf("WARN still owned by %s: %q (%s)", account, f.Name, f.Id)
			}
			if dryRun {
				log.Printf("WARN %d item(s) in the Container are still owned by %s; the drag's ownership transfer has not finished propagating", len(unflipped), account)
				break
			}
			if ignoreUnflipped {
				log.Printf("WARN %d item(s) in the Container are still owned by %s; --ignore-unflipped set: unpacking anyway. These items will be restored still owned by %s but recorded as migrated in the database.", len(unflipped), account, account)
				break
			}
			if time.Since(start) > flipWaitTimeout {
				return fmt.Errorf("%d item(s) in the Container are still owned by %s after %v; the drag's ownership transfer has not finished propagating — wait a few minutes and re-run unpack", len(unflipped), account, flipWaitTimeout)
			}
			log.Printf("%d item(s) in the Container are still owned by %s; the drag's ownership transfer is still propagating — rechecking in %v", len(unflipped), account, flipPollInterval)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(flipPollInterval):
			}
		}
	}

	// The restores run through a bounded worker pool sharing one context: when the
	// error budget is spent, stats.fail cancels it so in-flight workers stop and
	// the Stash restore short-circuits. A dry run only logs, so keep it
	// single-file for deterministic output.
	moveCtx, moveCancel := context.WithCancel(ctx)
	defer moveCancel()
	stats := &restoreStats{errorBudget: &errorBudget{cmd: "unpack", maxErrors: maxErrors, cancel: moveCancel}}
	workers := concurrency
	if dryRun {
		workers = 1
	}

	// ensureErrorsSub resolves (lazily creating) the <user folder>/Errors/<label>/
	// subfolder quarantine drops an unplaceable item into. Creation is serialized
	// by quarantineMu so concurrent workers can't race to make duplicate Errors
	// folders; the common already-resolved case is a fast map hit under the lock.
	var quarantineMu sync.Mutex
	var errorsFolder *drive.File
	errorsSubs := make(map[string]*drive.File)
	ensureErrorsSub := func(parentLabel string) (*drive.File, error) {
		quarantineMu.Lock()
		defer quarantineMu.Unlock()
		if errorsFolder == nil {
			ef, err := findChildFolder(moveCtx, svc, limiter, m.userFolderID, errorsFolderName)
			if err != nil {
				return nil, err
			}
			if ef == nil {
				if ef, err = rec.createFolder(moveCtx, svc, limiter, m.userFolderID, errorsFolderName); err != nil {
					return nil, err
				}
			}
			errorsFolder = ef
		}
		sub := errorsSubs[parentLabel]
		if sub == nil {
			s, err := findChildFolder(moveCtx, svc, limiter, errorsFolder.Id, parentLabel)
			if err != nil {
				return nil, err
			}
			if s == nil {
				if s, err = rec.createFolder(moveCtx, svc, limiter, errorsFolder.Id, parentLabel); err != nil {
					return nil, err
				}
			}
			errorsSubs[parentLabel] = s
			sub = s
		}
		return sub, nil
	}
	// quarantine moves an item that cannot be placed into
	// <user folder>/Errors/<parentLabel>/, where parentLabel is the Drive ID of
	// the folder it belongs in (or "unknown").
	quarantine := func(itemID, fromParent, parentLabel string) error {
		sub, err := ensureErrorsSub(parentLabel)
		if err != nil {
			return err
		}
		return rec.moveFileVerified(moveCtx, svc, limiter, itemID, sub.Id, fromParent)
	}

	// orphanLabel names the Errors subfolder for an item with no database row:
	// the live parent pack swept it from, if it was recorded, else "unknown".
	orphanLabel := func(itemID string) string {
		if p, err := packOrphanParent(db, account, itemID); err == nil {
			return p
		}
		return "unknown"
	}

	// restoreChildren drains one level of source (the Container or the Stash),
	// moving each child back to its original parent from the database. It first
	// walks the level (listing + DB lookups, on this goroutine, so the database
	// bookkeeping stays single-threaded), then runs the independent moves through
	// the worker pool. Moving a child out never changes another child's recorded
	// original parent, so enumerating before moving is safe.
	restoreChildren := func(source *drive.File, sourceLabel string, flipOwner bool) error {
		children, err := listChildren(ctx, svc, limiter, source.Id, "nextPageToken, files(id, name)")
		if err != nil {
			return fmt.Errorf("listing %s: %w", sourceLabel, err)
		}
		// task describes one child to restore: back to its recorded parent (dest),
		// or, when it has no usable database row, into the Errors subfolder label.
		type task struct {
			id, name    string
			dest, label string
			orphan      bool
		}
		var tasks []task
		for _, c := range children {
			if err := ctx.Err(); err != nil {
				log.Printf("interrupted: %d restored, %d quarantined, %d failed", stats.restoredCount(), stats.quarantinedCount(), stats.failedCount())
				return err
			}
			orig, derr := originalParentDriveID(db, c.Id)
			if derr == sql.ErrNoRows {
				tasks = append(tasks, task{id: c.Id, name: c.Name, label: orphanLabel(c.Id), orphan: true})
				continue
			}
			if derr != nil {
				return derr
			}
			tasks = append(tasks, task{id: c.Id, name: c.Name, dest: orig})
		}

		prog := newProgress()
		forEachConcurrent(moveCtx, workers, tasks, func(t task) {
			prog.tick("progress: restoring %s: %d/%d", sourceLabel, stats.processed(), len(tasks))
			if t.orphan {
				if dryRun {
					log.Printf("WOULD quarantine %q (%s): not in database -> %s/%s", t.name, t.id, errorsFolderName, t.label)
					stats.incQuarantined()
					return
				}
				if qerr := quarantine(t.id, source.Id, t.label); qerr != nil {
					if moveCtx.Err() != nil {
						return
					}
					stats.fail("ERROR quarantining %q (%s): %v", t.name, t.id, qerr)
					return
				}
				log.Printf("WARN %q (%s) is not in the database; quarantined under %s/%s for manual restore", t.name, t.id, errorsFolderName, t.label)
				stats.incQuarantined()
				return
			}
			if dryRun {
				log.Printf("WOULD move %q (%s) -> parent %s", t.name, t.id, t.dest)
				stats.incRestored()
				return
			}
			merr := rec.moveFileVerified(moveCtx, svc, limiter, t.id, t.dest, source.Id)
			if merr == nil {
				detailf("OK %q (%s) -> parent %s", t.name, t.id, t.dest)
				if flipOwner {
					// The drag flipped ownership of everything inside the Container
					// to the org — verified by the unflippedItems poll before the
					// restore started — so this child and every item that rode along
					// inside it (an owned subtree moves as one item) is now owned by
					// the running account. Record that for the whole subtree without
					// a per-file Drive lookup; the update only touches rows the DB
					// attributes to the migrating user, leaving nested third-party
					// (stashed) items alone.
					if n, err := updateSubtreeOwner(db, t.id, account, me.EmailAddress, me.PermissionId, me.DisplayName); err != nil {
						log.Printf("WARN could not update owner in DB for %q (%s) subtree: %v", t.name, t.id, err)
					} else {
						detailf("   updated owner for %d DB row(s) under %q (%s)", n, t.name, t.id)
					}
				}
				stats.incRestored()
				return
			}
			if moveCtx.Err() != nil {
				return
			}
			if isNotFound(merr) {
				// The destination is the problem (original parent deleted or
				// trashed): quarantine so cleanup is not blocked.
				if qerr := quarantine(t.id, source.Id, t.dest); qerr != nil {
					if moveCtx.Err() != nil {
						return
					}
					stats.fail("ERROR quarantining %q (%s) after missing parent %s: %v", t.name, t.id, t.dest, qerr)
					return
				}
				log.Printf("WARN original parent %s of %q (%s) is gone; quarantined under %s/%s for manual restore", t.dest, t.name, t.id, errorsFolderName, t.dest)
				stats.incQuarantined()
				return
			}
			stats.fail("ERROR moving %q (%s) to parent %s: %v (if the destination is still inside the shared drive, re-run unpack after the Container restore succeeds)", t.name, t.id, t.dest, merr)
		})
		if err := ctx.Err(); err != nil {
			log.Printf("interrupted: %d restored, %d quarantined, %d failed", stats.restoredCount(), stats.quarantinedCount(), stats.failedCount())
			return err
		}
		if stats.aborted {
			return stats.err
		}
		return nil
	}

	// The Container must be restored before the Stash: Stash items are owned by
	// third parties, and a shared drive cannot hold those, so their destination
	// folders have to be back in the regular tree first.
	if err := restoreChildren(container, "Container", containerInSharedDrive); err != nil {
		return err
	}

	// Unstash items that were moved to the stash.
	// Skip the Stash entirely when the Container did not fully clear; re-running unpack after the cause is fixed
	// converges.
	// A Stash item's destination is an owned folder that was inside the Container
	// (it rides back out only when its owned-root ancestor restores). If any
	// Container move failed — but stayed under --max-errors — those folders are
	// still in the shared drive, and every Stash item bound for them would fail
	// too (a shared drive cannot hold third-party items).
	if n := stats.failedCount(); n > 0 {
		log.Printf("WARN Container restore left %d item(s) in the shared drive; skipping the Stash restore because stashed items' destination folders may still be in the shared drive. Fix the cause and re-run unpack to finish.", n)
	} else if err := restoreChildren(stashF, "Stash", false); err != nil {
		return err
	}

	// Cleanup: delete the scaffolding only once live listings confirm it is
	// genuinely empty, so nothing that failed to move is silently lost.
	if dryRun {
		log.Printf("NOTE cleanup (deleting the emptied Stash, Container, and per-user folder, and revoking the user's dropoff access) is skipped in a dry run")
	} else {
		// isEmpty reports whether a live listing shows f has no remaining children.
		isEmpty := func(f *drive.File, label string) (bool, error) {
			remaining, err := listChildren(ctx, svc, limiter, f.Id, "nextPageToken, files(id)")
			if err != nil {
				return false, fmt.Errorf("re-checking %s contents: %w", label, err)
			}
			if len(remaining) > 0 {
				log.Printf("WARN %s still has %d item(s); leaving it in place", label, len(remaining))
				return false, nil
			}
			return true, nil
		}
		del := func(f *drive.File, label string) error {
			if err := rec.deleteFile(ctx, svc, limiter, f.Id); err != nil {
				return fmt.Errorf("deleting empty %s: %w", label, err)
			}
			log.Printf("deleted empty %s", label)
			return nil
		}
		deleteIfEmpty := func(f *drive.File, label string) error {
			ok, err := isEmpty(f, label)
			if err != nil || !ok {
				return err
			}
			return del(f, label)
		}
		// revokeDropoff removes the Manager access pack granted the user on the
		// dropoff folder (the shared drive): the round trip is done, so they no
		// longer need it, and leaving it would let them see the next user's files
		// migrating through the same folder. pack re-grants it if the migration is
		// retried. Needs their email; an owner-id-only account must be revoked
		// manually.
		revokeDropoff := func() error {
			if !strings.Contains(account, "@") {
				log.Printf("WARN %q is not an email address; cannot revoke dropoff access automatically — remove its access on %q manually", account, dropoff.Name)
				return nil
			}
			perm, err := findUserPermission(ctx, svc, limiter, dropoff.Id, account)
			if err != nil {
				return fmt.Errorf("checking dropoff access for %s: %w", account, err)
			}
			if perm != nil {
				if err := rec.revokePermission(ctx, svc, limiter, dropoff.Id, perm.Id); err != nil {
					return fmt.Errorf("revoking %s access on the dropoff folder: %w", account, err)
				}
				log.Printf("revoked %s access on the dropoff folder %q", account, dropoff.Name)
			}
			return nil
		}

		if !containerInSharedDrive {
			// Migration aborted (--allow-not-moved): the Container stays in the
			// Pickup folder for a later re-pack (along with the user's read access
			// on it — it only ever shows them their own Container); only the emptied
			// Stash is cleaned up here. Revoke the user's dropoff access anyway once
			// the Stash is clear — the round trip is over, and leaving it would
			// expose the next user's files; a re-pack re-grants it.
			stashEmpty, err := isEmpty(stashF, "Stash")
			if err != nil {
				return err
			}
			if !stashEmpty {
				log.Printf("WARN migration abort for %s is unfinished; leaving the Stash and the user's dropoff access in place — re-run unpack to finish", account)
			} else {
				if err := del(stashF, "Stash"); err != nil {
					return err
				}
				if err := revokeDropoff(); err != nil {
					return err
				}
			}
		} else {
			// Post-drag cleanup is all-or-nothing: delete the Stash and Container,
			// revoke the user's dropoff access, and delete the per-user folder only
			// once BOTH the Container and the Stash are confirmed empty. If either
			// still has items — a restore error left some behind — the migration is
			// unfinished and must be re-run, and that re-run needs the Container
			// and the Stash to still exist. So leave all the scaffolding, and the
			// user's access, in place until both are clear.
			containerEmpty, err := isEmpty(container, "Container")
			if err != nil {
				return err
			}
			stashEmpty, err := isEmpty(stashF, "Stash")
			if err != nil {
				return err
			}
			if !containerEmpty || !stashEmpty {
				log.Printf("WARN migration for %s is unfinished; leaving the Container, Stash, per-user folder, and the user's dropoff access in place — re-run unpack to finish", account)
			} else {
				if err := del(stashF, "Stash"); err != nil {
					return err
				}
				if err := del(container, "Container"); err != nil {
					return err
				}
				if err := revokeDropoff(); err != nil {
					return err
				}
				// The Pickup folder held only the Container, which the drag took
				// away; deleting it also removes the user's read access. It must go
				// before the per-user folder, which can only be deleted once empty.
				if err := deleteIfEmpty(&drive.File{Id: m.pickupID}, "Pickup folder"); err != nil {
					return err
				}
				// The per-user folder goes too, unless something remains in it — e.g.
				// a non-empty Errors folder.
				if err := deleteIfEmpty(&drive.File{Id: m.userFolderID}, fmt.Sprintf("per-user folder for %s", account)); err != nil {
					return err
				}
			}
		}
	}

	verb := "restored"
	if dryRun {
		verb = "would restore"
	}
	log.Printf("done: %d item(s) %s, %d quarantined, %d failed", stats.restoredCount(), verb, stats.quarantinedCount(), stats.failedCount())
	if stats.quarantinedCount() > 0 {
		log.Printf("NOTE quarantined items are under the %s folder; each subfolder is named after the Drive ID of the folder the item belongs in (\"unknown\" if never crawled)", errorsFolderName)
	}
	if n := stats.failedCount(); n > 0 {
		return fmt.Errorf("%d item(s) failed; re-run unpack to retry", n)
	}
	if !dryRun {
		if err := markUnpacked(db, account); err != nil {
			return fmt.Errorf("recording unpack completion: %w", err)
		}
	}
	return nil
}
