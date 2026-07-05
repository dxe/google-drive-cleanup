package main

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

// User's Packing folder layout inside the packing folder:
//
//	<packing>/<account>/           org-owned; the user has NO access
//	  pickup-<account>/            user is granted read access (see pickupFolderName)
//	    <account>-Container/       created manually by the admin's personal Gmail
//	  Stash/                       third-party items, hidden from the user
//
// The Container must be created manually by the admin's PERSONAL Gmail account
// — only personal accounts can transfer ownership to other personal accounts,
// and the user must own the Container to drag it into the shared drive. The
// CLI creates everything else.
const (
	stashFolderName  = "Stash"
	errorsFolderName = "Errors"
	// dropoffRole is the shared-drive role granted to the migrating user on the
	// dropoff folder. "organizer" is "Manager" in the Drive UI — enough to move
	// (drag) their Container into the shared drive. unpack revokes it afterward.
	dropoffRole = "organizer"
)

// containerFolderName is the required name of a user's Container folder. It is
// scoped by account so multiple users' Containers are distinguishable once
// dragged into the same shared drive.
func containerFolderName(account string) string {
	return account + "-Container"
}

// pickupFolderName is the name of a user's Pickup folder — the Container's
// parent, and the only part of the packing scaffolding the user can see (pack
// grants them read access). Without it, accepting Container ownership would
// relocate the Container to the user's My Drive root: Drive does that when the
// new owner cannot see the item's parent, and the Container would then be
// missing from where a pack re-run or unpack looks for it. The user's Packing folder
// itself cannot be shared instead — the Stash inside it holds third-party
// files the user must not access. Scoped by account so the folder is
// recognizable in the user's "Shared with me".
func pickupFolderName(account string) string {
	return "pickup-" + account
}

var packCmd = &cobra.Command{
	Use:   "pack <user>",
	Short: "Move everything <user> owns into their Container folder, ready for a single drag into the shared drive",
	Long: `Gather everything <user> (email or owner id) owns into one Container folder
under <packing-folder>/<user>/pickup-<user>/, so the user can transfer it all to
the org with a single drag of the Container into the shared-drive dropoff
folder. The user is granted read access to the Pickup folder (and nothing else
in the scaffolding) so the Container stays in place when they accept ownership
of it — without that, Drive would relocate it to their My Drive root.

Pass --folder <id> (a Drive folder ID that was crawled into the database) to
pack only the user's items within that subfolder of the crawl root, instead of
everything they own. The rest of the user's files stay put; the confirmation
message shows the subfolder's path relative to the crawl root.

Owned subtrees move intact: only "owned roots" (owned items whose parent is not
owned by the user) are moved into the Container; owned items nested below them
ride along. A recursive sweep then moves every item inside the Container tree
that the user does NOT own into a flat Stash folder next to the Container —
a shared drive cannot hold third-party-owned items, so leaving them in place
would block the drag. unpack later returns everything to the original parents
recorded in the database.

The Container itself must already exist: create it inside the Pickup folder
with the admin's PERSONAL Gmail account (only personal accounts can transfer
ownership to other personal accounts), so its ownership can be transferred to
the user before they drag it. On a first run pack creates the user's Packing folder,
Stash, and Pickup folder, then stops with instructions to create the Container.

This command requires the full Drive scope. If the cached token.json only has
read-only access, the tool re-runs consent automatically to obtain it.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		maxErrors, _ := cmd.Flags().GetInt("max-errors")
		subfolder, _ := cmd.Flags().GetString("folder")
		skipUnmovable, _ := cmd.Flags().GetBool("skip-unmovable")
		concurrency, _ := cmd.Flags().GetInt("concurrency")
		return runPack(dbPath, cfgPath, args[0], subfolder, dryRun, maxErrors, skipUnmovable, concurrency)
	},
}

// defaultMoveConcurrency is how many moves pack and unpack run in flight by
// default. Each files.update has hundreds of ms of latency, so a single worker
// only reaches a few per second regardless of the rate limit; a handful of
// workers keeps enough requests in flight to reach the shared limiter's ceiling.
// The limiter (not the worker count) is the quota safety cap, so this stays
// modest.
const defaultMoveConcurrency = 8

func init() {
	packCmd.Flags().Bool("dry-run", false, "report what would move without changing anything (read-only scope)")
	packCmd.Flags().Int("max-errors", 5, "abort once more than this many items fail to move")
	packCmd.Flags().String("folder", "", "Google Drive folder ID to scope the pack to (must be crawled into the database); packs only the user's items within that subfolder of the crawl root")
	packCmd.Flags().Bool("skip-unmovable", false, "skip crawled items the crawling account cannot edit (they cannot be moved) and pack the rest, instead of aborting (equivalent to migration.skip-unmovable in config.json)")
	packCmd.Flags().Int("concurrency", defaultMoveConcurrency, "how many file moves to run in parallel (all still share the global rate limiter)")
}

// subtreeRelativePath returns the path of driveID relative to the crawl root,
// e.g. "Projects/2025" for a folder two levels below the root. nodePath yields
// the segments from the crawl root down (root name first), so dropping the
// first segment gives the path relative to it; it is empty for the root itself.
func subtreeRelativePath(db *sql.DB, driveID string) (string, error) {
	segments, err := nodePath(db, driveID)
	if err != nil {
		return "", err
	}
	if len(segments) > 1 {
		return strings.Join(segments[1:], "/"), nil
	}
	return "", nil
}

// moveStats holds a pack run's running tallies. The move phases update it from
// their worker pool, so the counters are guarded by mu; the shared error budget
// (embedded) carries the failure count and abort logic.
type moveStats struct {
	*errorBudget
	mu               sync.Mutex
	movedToContainer int
	sweptToStash     int
	alreadyPacked    int
	skipped          int
}

func (s *moveStats) moved()   { s.mu.Lock(); s.movedToContainer++; s.mu.Unlock() }
func (s *moveStats) swept()   { s.mu.Lock(); s.sweptToStash++; s.mu.Unlock() }
func (s *moveStats) already() { s.mu.Lock(); s.alreadyPacked++; s.mu.Unlock() }
func (s *moveStats) skip()    { s.mu.Lock(); s.skipped++; s.mu.Unlock() }

func (s *moveStats) movedCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.movedToContainer }
func (s *moveStats) sweptCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.sweptToStash }

func runPack(dbPath, cfgPath, account, subfolder string, dryRun bool, maxErrors int, skipUnmovable bool, concurrency int) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	// The config setting and the flag are equivalent; either one enables skipping.
	skipUnmovable = skipUnmovable || cfg.Migration.SkipUnmovable
	if err := cfg.Migration.PackingFolder.validate("migration.packing-folder"); err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
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
	// pack run (see opLog).
	rec := &opLog{db: db, account: account, command: "pack"}

	crawlRoot, err := crawlRootDriveID(db)
	if err == sql.ErrNoRows {
		return fmt.Errorf("database is empty; run crawl first")
	}
	if err != nil {
		return err
	}
	// The snapshot must describe the tree the config now points at: pack moves
	// live files based on it, so a config root that no longer matches what was
	// crawled means every original-parent decision could be wrong. Refuse and
	// make the operator re-crawl. (unpack deliberately skips this check — it
	// finishes an in-flight migration from recorded per-user state, not the root.)
	if crawlRoot != cfg.Crawl.Root.ID {
		return fmt.Errorf("crawl root in config (%s, %q) does not match the root in the database (%s); crawl.root.id changed since the last crawl — re-run `drive-cleanup crawl` to rebuild the snapshot before packing",
			cfg.Crawl.Root.ID, cfg.Crawl.Root.Name, crawlRoot)
	}

	// An optional subfolder scopes the pack to the user's items within one
	// crawled folder of the tree. It must be a folder in the snapshot (anything
	// crawled is by construction under the crawl root). subfolderPath is its
	// path relative to the crawl root, shown in the confirmation message.
	var subfolderPath string
	if subfolder != "" {
		typ, err := nodeTypeByDriveID(db, subfolder)
		if err == sql.ErrNoRows {
			return fmt.Errorf("subfolder %s not found in the database; it must be a folder crawled under the crawl root", subfolder)
		}
		if err != nil {
			return err
		}
		if typ != typeFolder {
			return fmt.Errorf("subfolder %s is a %s, not a folder", subfolder, typ)
		}
		subfolderPath, err = subtreeRelativePath(db, subfolder)
		if err != nil {
			return err
		}
	}

	owned, err := nodesOwnedBy(db, account, subfolder)
	if err != nil {
		return err
	}
	if len(owned) == 0 {
		if subfolder != "" {
			fmt.Fprintf(os.Stderr, "Nothing owned by %q under subfolder %q in the database; nothing to pack.\n", account, subfolderPath)
		} else {
			fmt.Fprintf(os.Stderr, "Nothing owned by %q in the database; nothing to pack.\n", account)
		}
		return nil
	}
	roots, err := ownedRoots(db, account, subfolder)
	if err != nil {
		return err
	}
	for _, n := range owned {
		if n.driveID == crawlRoot {
			log.Printf("WARN the crawl root itself is owned by %q; it is never moved", account)
		}
	}
	extraParents, err := extraParentNodeIDs(db)
	if err != nil {
		return err
	}

	if pending, err := countPendingFolders(db, ""); err != nil {
		return err
	} else if pending > 0 {
		return fmt.Errorf("the crawl is incomplete (%d folder(s) not fully listed); the database may be missing items. Re-run crawl first for a complete pack", pending)
	}

	// Edit-access pre-check: the same scan check-edit-access reports. If any
	// crawled node is not editable, the run may fail to move some items, so
	// confirm before proceeding. Scope it to the subfolder when one is given, so
	// uneditable items elsewhere in the tree don't trigger a spurious prompt.
	uneditable, err := nodesLackingEditAccess(db)
	if err != nil {
		return err
	}
	if subfolder != "" {
		inSubtree, err := subtreeDriveIDs(db, subfolder)
		if err != nil {
			return err
		}
		filtered := uneditable[:0]
		for _, r := range uneditable {
			if inSubtree[r.driveID] {
				filtered = append(filtered, r)
			}
		}
		uneditable = filtered
	}
	if len(uneditable) > 0 {
		var folderCount, fileCount int
		for _, r := range uneditable {
			if r.typ == typeFolder {
				folderCount++
			} else {
				fileCount++
			}
		}
		if !skipUnmovable {
			return fmt.Errorf("%d crawled item(s) are not editable by the crawling account: %d folder(s), %d file(s); any of these that need to move will fail. Run check-edit-access for the full list, or pass --skip-unmovable (or set migration.skip-unmovable in config) to skip those items and pack the rest",
				len(uneditable), folderCount, fileCount)
		}
		fmt.Fprintf(os.Stderr, "WARNING: %d crawled item(s) are not editable by the crawling account: %d folder(s), %d file(s).\n",
			len(uneditable), folderCount, fileCount)
		fmt.Fprintln(os.Stderr, "Proceeding due to skip-unmovable; these items will be skipped, not moved. Run check-edit-access for the full list.")
	}

	// unmovable is the set of nodes the crawling account cannot edit, keyed by
	// Drive ID. When --skip-unmovable is set we consult it before attempting any
	// move and skip those items outright — moving one is a guaranteed Google API
	// failure, so checking the database first avoids the wasted call and keeps it
	// from eating the --max-errors budget. Empty (a no-op) when nothing is
	// uneditable; only ever populated here because the run aborts above otherwise.
	unmovable := make(map[string]bool, len(uneditable))
	for _, r := range uneditable {
		unmovable[r.driveID] = true
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
	if strings.EqualFold(me.EmailAddress, account) || me.PermissionId == account {
		return fmt.Errorf("pack is running as %s, the account being migrated; run it as an org account with edit access instead", me.EmailAddress)
	}

	packing, err := getConfiguredFolder(ctx, svc, cfg.Migration.PackingFolder, "migration.packing-folder")
	if err != nil {
		return err
	}
	if packing.DriveId != "" {
		return fmt.Errorf("packing folder %s is in a shared drive (%s); it must be a regular My-Drive folder so Stashes can hold third-party-owned files", packing.Id, packing.DriveId)
	}
	if inside, err := folderInsideRoot(ctx, svc, packing, crawlRoot); err != nil {
		return fmt.Errorf("checking the packing folder against the crawl root: %w", err)
	} else if inside {
		return fmt.Errorf("packing folder %s (%q) is inside the crawl root %s; move it outside so re-crawls don't ingest mid-migration scaffolding", packing.Id, packing.Name, crawlRoot)
	}
	dropoff, err := getConfiguredFolder(ctx, svc, cfg.Migration.DropoffFolder, "migration.dropoff-folder")
	if err != nil {
		return err
	}
	if dropoff.DriveId == "" {
		return fmt.Errorf("dropoff folder %s is not in a shared drive; dragging the Container there would not transfer ownership to the org", dropoff.Id)
	}

	// The moves below run concurrently (see --concurrency), but every call still
	// passes through this one shared limiter, so it — not the worker count — is
	// the quota safety cap. 20/sec is far under Drive's per-user ceiling
	// (~12k/min); backoff-on-429/403 self-throttles if we ever overshoot.
	limiter := rate.NewLimiter(rate.Limit(20), 20)

	// Resolve (or create) the per-user scaffolding: <packing>/<account>/ with
	// Stash inside. Find-before-create keeps re-runs and crash recovery safe.
	userFolder, err := findChildFolder(ctx, svc, limiter, packing.Id, account)
	if err != nil {
		return fmt.Errorf("looking up the Packing folder for %s: %w", account, err)
	}
	if userFolder == nil {
		if dryRun {
			log.Printf("WOULD create Packing folder for %q under %q", account, packing.Name)
		} else if userFolder, err = rec.createFolder(ctx, svc, limiter, packing.Id, account); err != nil {
			return fmt.Errorf("creating the Packing folder for %s: %w", account, err)
		}
	}
	var stashF, pickupF, containerF *drive.File
	if userFolder != nil {
		stashF, err = findChildFolder(ctx, svc, limiter, userFolder.Id, stashFolderName)
		if err != nil {
			return fmt.Errorf("looking up the Stash folder: %w", err)
		}
		if stashF == nil {
			if dryRun {
				log.Printf("WOULD create %q under %s/%s", stashFolderName, packing.Name, account)
			} else if stashF, err = rec.createFolder(ctx, svc, limiter, userFolder.Id, stashFolderName); err != nil {
				return fmt.Errorf("creating the Stash folder: %w", err)
			}
		}
		pickupF, err = findChildFolder(ctx, svc, limiter, userFolder.Id, pickupFolderName(account))
		if err != nil {
			return fmt.Errorf("looking up the Pickup folder: %w", err)
		}
		if pickupF == nil {
			if dryRun {
				log.Printf("WOULD create %q under %s/%s", pickupFolderName(account), packing.Name, account)
			} else if pickupF, err = rec.createFolder(ctx, svc, limiter, userFolder.Id, pickupFolderName(account)); err != nil {
				return fmt.Errorf("creating the Pickup folder: %w", err)
			}
		}
	}
	if pickupF != nil {
		containerF, err = findChildFolder(ctx, svc, limiter, pickupF.Id, containerFolderName(account))
		if err != nil {
			return fmt.Errorf("looking up the Container folder: %w", err)
		}
	}

	// Grant the migrating user Manager (organizer) access on the dropoff folder
	// — the shared drive itself — so they can move (drag) their Container in.
	// This needs their email; an owner-id-only account must be granted access
	// manually. The grant is idempotent — skip it when they already have a role
	// so a re-run does not re-notify them. unpack revokes it once the round trip
	// is done.
	switch {
	case !strings.Contains(account, "@"):
		log.Printf("WARN %q is not an email address; cannot grant dropoff access automatically — grant it Manager access on %q manually", account, dropoff.Name)
	case dryRun:
		log.Printf("WOULD grant %q Manager access on the dropoff folder %q", account, dropoff.Name)
	default:
		existing, err := findUserPermission(ctx, svc, limiter, dropoff.Id, account)
		if err != nil {
			return fmt.Errorf("checking dropoff access for %s: %w", account, err)
		}
		if existing == nil {
			if err := rec.grantPermission(ctx, svc, limiter, dropoff.Id, account, dropoffRole); err != nil {
				return fmt.Errorf("granting %s Manager access on the dropoff folder: %w", account, err)
			}
			fmt.Fprintf(os.Stderr, "Granted %s Manager access on %s.\n", account, dropoff.Name)
		} else {
			fmt.Fprintf(os.Stderr, "Dropoff access: %s already has %q on %s.\n", account, existing.Role, dropoff.Name)
		}
	}

	// Give the migrating user read access to the Pickup folder — see
	// pickupFolderName for why (it keeps the Container in place when they accept
	// ownership). Idempotent like the dropoff grant above; unpack removes the
	// access by deleting the Pickup folder during cleanup.
	switch {
	case !strings.Contains(account, "@"):
		log.Printf("WARN %q is not an email address; cannot grant Pickup access automatically — grant it read access on %q manually", account, pickupFolderName(account))
	case dryRun:
		log.Printf("WOULD grant %q read access on the Pickup folder %q", account, pickupFolderName(account))
	default:
		existing, err := findUserPermission(ctx, svc, limiter, pickupF.Id, account)
		if err != nil {
			return fmt.Errorf("checking Pickup access for %s: %w", account, err)
		}
		if existing == nil {
			if err := rec.grantPermission(ctx, svc, limiter, pickupF.Id, account, "reader"); err != nil {
				return fmt.Errorf("granting %s read access on the Pickup folder: %w", account, err)
			}
			fmt.Fprintf(os.Stderr, "Granted %s read access on %s.\n", account, pickupFolderName(account))
		} else {
			fmt.Fprintf(os.Stderr, "Pickup access: %s already has %q on %s.\n", account, existing.Role, pickupFolderName(account))
		}
	}

	if containerF == nil && !dryRun {
		parentLink := "https://drive.google.com/drive/folders/" + pickupF.Id
		return fmt.Errorf("no %q folder yet.\n\nPlease create the container:\n\n"+
			"1. Log into Google Drive with admin's PERSONAL Gmail account (only personal accounts can transfer ownership to other personal accounts; the packing folder must be shared with that account as editor)\n"+
			"2. Open pickup folder: %s\n"+
			"3. Create a new folder inside called %q\n"+
			"4. Re-run pack",
			containerFolderName(account), parentLink, containerFolderName(account))
	}

	if !dryRun {
		// The Container must not have been dragged already, and ideally is (or
		// will be) owned by the user — the drag requires it.
		cState, err := getFileState(ctx, svc, limiter, containerF.Id)
		if err != nil {
			return fmt.Errorf("fetching Container %s: %w", containerF.Id, err)
		}
		if cState.DriveId != "" {
			return fmt.Errorf("Container %s is already in a shared drive (%s); it looks like it was already dragged to the dropoff folder — run unpack instead", containerF.Id, cState.DriveId)
		}
		if ownedByAccount(cState, account) {
			fmt.Fprintf(os.Stderr, "Container ownership: already owned by %s.\n", account)
		} else {
			owner := "(unknown)"
			if len(cState.Owners) > 0 {
				owner = cState.Owners[0].EmailAddress
			}
			fmt.Fprintf(os.Stderr, "Container ownership: currently %s — transfer it to %s (invite + accept, via the Drive UI) before the drag.\n", owner, account)
		}
		if err := upsertUserMigration(db, account, userFolder.Id, pickupF.Id, containerF.Id, stashF.Id); err != nil {
			return fmt.Errorf("recording the migration: %w", err)
		}
	}

	// scopeNote describes the subfolder restriction for the confirmation, empty
	// for a whole-tree pack.
	scopeNote := ""
	if subfolder != "" {
		scopeNote = fmt.Sprintf(" from subfolder %q", subfolderPath)
	}
	if dryRun {
		fmt.Fprintf(os.Stderr, "DRY RUN: no changes will be made. Would pack %d item(s) owned by %q%s (%d owned subtree root(s)).\n", len(owned), account, scopeNote, len(roots))
	} else {
		fmt.Fprintf(os.Stderr, "About to pack %d item(s) owned by %q%s (%d owned subtree root(s)) into %s/%s/%s/%s.\n",
			len(owned), account, scopeNote, len(roots), packing.Name, account, pickupFolderName(account), containerFolderName(account))
		if !promptYesNo("Continue? [y/N] ") {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return nil
		}
	}

	// The moves run through a bounded worker pool sharing one context: when the
	// error budget is spent, stats.fail cancels it so in-flight workers stop and
	// the later phases short-circuit. A dry run only logs, so keep it single-file
	// for deterministic output.
	moveCtx, moveCancel := context.WithCancel(ctx)
	defer moveCancel()
	stats := &moveStats{errorBudget: &errorBudget{cmd: "pack", maxErrors: maxErrors, cancel: moveCancel}}
	workers := concurrency
	if dryRun {
		workers = 1
	}
	warnExtraParents := func(id, name string) {
		if extraParents[id] {
			log.Printf("WARN %q (%s) has extra parents recorded in the database; the round trip keeps only the traversal parent (see the extra_parents table)", name, id)
		}
	}
	// interrupted reports (and logs) whether the run was cancelled by a signal, as
	// opposed to finishing a phase normally or hitting the error budget. Safe to
	// read stats directly here — every worker has drained before it is called.
	interrupted := func() bool {
		if ctx.Err() == nil {
			return false
		}
		log.Printf("interrupted: %d moved to Container, %d swept to Stash, %d failed", stats.movedToContainer, stats.sweptToStash, stats.failed)
		return true
	}

	// Phase A: move each owned root into the Container. Moves are optimistic —
	// one files.update per item, removing the parent the crawl recorded. If
	// that parent is stale the update fails loudly (Drive items have a single
	// parent), and only then do we fetch the item's live state to diagnose.
	prog := newProgress()
	forEachConcurrent(moveCtx, workers, roots, func(r ownedRoot) {
		prog.tick("progress: %d/%d moved to Container", stats.movedCount(), len(roots))
		warnExtraParents(r.driveID, r.name)
		if unmovable[r.driveID] {
			log.Printf("SKIP %s %q (%s): not editable by the crawling account; cannot move it", r.typ, r.name, r.driveID)
			stats.skip()
			return
		}
		if dryRun {
			log.Printf("WOULD move %s %q (%s) from %s into the Container", r.typ, r.name, r.driveID, r.parentDriveID)
			stats.moved()
			return
		}
		err := rec.moveFile(moveCtx, svc, limiter, r.driveID, containerF.Id, r.parentDriveID)
		if err == nil {
			detailf("OK %s %q (%s) -> Container", r.typ, r.name, r.driveID)
			stats.moved()
			return
		}
		if moveCtx.Err() != nil {
			return
		}
		// Diagnose this one item: gone, trashed, already packed, ownership
		// changed, or just a stale parent to retry from the live one. A cancelled
		// context (error budget spent elsewhere, or SIGINT) makes these calls fail
		// with context.Canceled; that is not a failure of this item, so bail before
		// counting anything.
		f, gerr := getFileState(moveCtx, svc, limiter, r.driveID)
		if moveCtx.Err() != nil {
			return
		}
		switch {
		case isNotFound(gerr):
			log.Printf("SKIP %q (%s): no longer exists", r.name, r.driveID)
			stats.skip()
		case gerr != nil:
			stats.fail("ERROR %q (%s): move failed (%v) and live lookup failed (%v)", r.name, r.driveID, err, gerr)
		case f.Trashed:
			log.Printf("SKIP %q (%s): trashed since the crawl", r.name, r.driveID)
			stats.skip()
		case hasParent(f, containerF.Id):
			detailf("OK %s %q (%s): already in the Container", r.typ, r.name, r.driveID)
			stats.already()
		case !ownedByAccount(f, account):
			log.Printf("SKIP %q (%s): no longer owned by %s; leaving it in place", r.name, r.driveID, account)
			stats.skip()
		default:
			if merr := rec.moveFile(moveCtx, svc, limiter, r.driveID, containerF.Id, strings.Join(f.Parents, ",")); merr != nil {
				if moveCtx.Err() != nil {
					return
				}
				stats.fail("ERROR moving %q (%s) into the Container: %v", r.name, r.driveID, merr)
			} else {
				// Its recorded parent was stale — expected, since packing moves
				// roots out from under one another. Retrying from the live parent
				// (above) is the normal path, so this is not worth a warning.
				detailf("OK %s %q (%s) -> Container (from live parent %v)", r.typ, r.name, r.driveID, f.Parents)
				stats.moved()
			}
		}
	})
	if interrupted() {
		return ctx.Err()
	}
	if stats.aborted {
		return stats.err
	}

	// Phase B: sweep the live Container tree. Everything the user does not own
	// goes to the flat Stash — a shared drive cannot hold third-party-owned
	// items, so anything left would block the drag. Working from live listings
	// (rather than the database) also catches items created or re-owned after
	// the crawl; the sweep doubles as the pre-drag ownership verification.
	// seen collects every user-owned item observed in the tree for Phase C.
	seen := make(map[string]bool)
	if dryRun {
		preview, err := unownedChildrenOfOwned(db, account, subfolder)
		if err != nil {
			return err
		}
		for _, s := range preview {
			if unmovable[s.driveID] {
				log.Printf("WOULD skip %q (%s): not editable by the crawling account; cannot sweep it to the Stash", s.name, s.driveID)
				stats.skip()
				continue
			}
			log.Printf("WOULD move %q (%s) out of owned folder %s into the Stash", s.name, s.driveID, s.parentDriveID)
			stats.swept()
		}
		log.Printf("NOTE the real sweep works from live Drive listings, so it also catches items the crawl never saw")
	} else {
		// First walk the tree (listing only, on this goroutine) to enumerate the
		// items to sweep — the database bookkeeping (orphan recording) and the
		// owned/unowned classification stay single-threaded, so the DB and the
		// seen map need no locking. Moving unowned items to the Stash never
		// changes which folders are owned, so enumerating before moving is safe.
		type stashTarget struct{ id, name, parent string }
		var toStash []stashTarget
		queue := []string{containerF.Id}
		for len(queue) > 0 {
			if interrupted() {
				return ctx.Err()
			}
			folderID := queue[0]
			queue = queue[1:]
			children, err := listChildren(ctx, svc, limiter, folderID,
				"nextPageToken, files(id, name, mimeType, owners(emailAddress, permissionId))")
			if err != nil {
				return fmt.Errorf("sweeping Container tree (listing %s): %w", folderID, err)
			}
			for _, c := range children {
				if ownedByAccount(c, account) {
					seen[c.Id] = true
					if c.MimeType == folderMimeType {
						queue = append(queue, c.Id)
					}
					continue
				}
				if unmovable[c.Id] {
					log.Printf("SKIP %q (%s): not editable by the crawling account; cannot sweep it to the Stash (it will block the drag until handled manually)", c.Name, c.Id)
					stats.skip()
					continue
				}
				if _, derr := nodeTypeByDriveID(db, c.Id); derr == sql.ErrNoRows {
					log.Printf("WARN not in database: %q (%s), swept from %s — if unpack cannot place it, it lands in %s/%s for manual restore",
						c.Name, c.Id, folderID, errorsFolderName, folderID)
					if err := recordPackOrphan(db, account, c.Id, folderID); err != nil {
						return fmt.Errorf("recording orphan %s: %w", c.Id, err)
					}
				} else if derr != nil {
					return derr
				}
				warnExtraParents(c.Id, c.Name)
				toStash = append(toStash, stashTarget{c.Id, c.Name, folderID})
			}
		}
		// Then sweep them all to the Stash concurrently.
		forEachConcurrent(moveCtx, workers, toStash, func(t stashTarget) {
			prog.tick("progress: %d/%d swept to Stash", stats.sweptCount(), len(toStash))
			if merr := rec.moveFile(moveCtx, svc, limiter, t.id, stashF.Id, t.parent); merr != nil {
				if moveCtx.Err() != nil {
					return
				}
				stats.fail("ERROR sweeping %q (%s) into the Stash: %v", t.name, t.id, merr)
				return
			}
			detailf("OK swept %q (%s) -> Stash", t.name, t.id)
			stats.swept()
		})
		if interrupted() {
			return ctx.Err()
		}
		if stats.aborted {
			return stats.err
		}
	}

	// Phase C: stragglers — items the database says the user owns that never
	// showed up inside the Container tree (usually items that moved since the
	// crawl, so their owned ancestor no longer carries them). Costs no API
	// calls unless something is actually missing. seen is read-only here, so the
	// workers can share it without locking.
	if !dryRun {
		forEachConcurrent(moveCtx, workers, owned, func(n ownedNode) {
			if seen[n.driveID] || n.driveID == crawlRoot {
				return
			}
			if unmovable[n.driveID] {
				detailf("SKIP straggler %q (%s): not editable by the crawling account; cannot move it", n.name, n.driveID)
				stats.skip()
				return
			}
			if moveCtx.Err() != nil {
				return
			}
			f, gerr := getFileState(moveCtx, svc, limiter, n.driveID)
			if moveCtx.Err() != nil {
				return
			}
			switch {
			case isNotFound(gerr):
				detailf("SKIP straggler check %q (%s): no longer exists", n.name, n.driveID)
			case gerr != nil:
				stats.fail("ERROR straggler check %q (%s): %v", n.name, n.driveID, gerr)
			case f.Trashed, hasParent(f, containerF.Id), hasParent(f, stashF.Id):
				// Trashed, or already handled this run (Phase A skips land here too).
			case !ownedByAccount(f, account):
				// Ownership changed since the crawl; nothing of the user's to move.
			default:
				// Not in the live sweep's `seen` set and not a direct child of the
				// Container or Stash — but it may still sit *inside* the Container,
				// nested under an owned folder that moved in Phase A, if the sweep's
				// listing hadn't caught up with that move yet (Drive files.list is
				// eventually consistent and lags just-completed moves). Walk the live
				// parent chain up to the Container before flattening: re-moving an item
				// that already rode in would strip its nesting for nothing and log a
				// misleading "outside the packed tree" warning.
				inside, ierr := insideAncestor(moveCtx, svc, limiter, f.Parents, containerF.Id)
				if moveCtx.Err() != nil {
					return
				}
				if ierr != nil {
					stats.fail("ERROR straggler check %q (%s): walking its parent chain: %v", n.name, n.driveID, ierr)
					return
				}
				if inside {
					detailf("OK straggler %q (%s): already nested inside the Container; leaving it in place", n.name, n.driveID)
					return
				}
				if merr := rec.moveFile(moveCtx, svc, limiter, n.driveID, containerF.Id, strings.Join(f.Parents, ",")); merr != nil {
					if moveCtx.Err() != nil {
						return
					}
					stats.fail("ERROR moving straggler %q (%s) into the Container: %v", n.name, n.driveID, merr)
				} else {
					log.Printf("WARN straggler %q (%s) was outside the packed tree (live parent %v); moved flat into the Container", n.name, n.driveID, f.Parents)
					stats.moved()
					if f.MimeType == folderMimeType {
						log.Printf("WARN straggler %q (%s) is a folder; re-run pack to sweep any third-party items inside it", n.name, n.driveID)
					}
				}
			}
		})
		if interrupted() {
			return ctx.Err()
		}
		if stats.aborted {
			return stats.err
		}
	}

	verb := "moved"
	if dryRun {
		verb = "would move"
	}
	log.Printf("done: %d item(s) %s to Container (%d already there), %d swept to Stash, %d skipped, %d failed",
		stats.movedToContainer, verb, stats.alreadyPacked, stats.sweptToStash, stats.skipped, stats.failed)
	if stats.failed > 0 {
		return fmt.Errorf("%d item(s) failed; re-run pack to retry", stats.failed)
	}
	if !dryRun {
		if err := markPacked(db, account); err != nil {
			return fmt.Errorf("recording pack completion: %w", err)
		}
		fmt.Fprintf(os.Stderr, `Pack complete. Next steps:
  1. Transfer ownership of the Container to %[1]s via the Drive UI (invite + accept).
  2. Ask %[1]s to drag the Container from %[3]q into the %[2]q folder, where they now have Manager access (one drag; this flips ownership of everything inside to the org).
  3. Run: drive-cleanup unpack %[1]s

Provide this link to the Pickup folder to the user so they can drag and drop the Container to the Shared Drive:
%[4]s
`, account, dropoff.Name, pickupFolderName(account), "https://drive.google.com/drive/folders/"+pickupF.Id)
	}
	return nil
}
