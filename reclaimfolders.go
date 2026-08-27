package main

// The reclaim-folders command replaces folders owned by somebody else with folders
// owned by the account running the tool. Ownership of a folder cannot be taken
// over (only its owner can hand it away), so instead of transferring anything
// it builds a parallel folder we do own and empties theirs into it:
//
//	their folder  -> renamed "(old) <name>", left in place holding a
//	                 "(new) <name>" shortcut pointing at the replacement
//	our folder    -> created next to it as "<name>", holding everything that
//	                 used to be directly inside theirs
//
// Anything that could not be moved stays in their folder, and our folder then
// gets an "(old) <name>" shortcut back to it so the leftovers stay reachable.
// That backwards shortcut is the only one that is conditional: the "(new)
// <name>" shortcut in their folder is always there, and their folder itself is
// always marked keep and never deleted, so links and bookmarks pointing at it
// go on working and lead whoever follows them to the replacement.
//
// The snapshot is updated as the moves happen — the replacement folder and the
// shortcuts are inserted as nodes rows and the moved children reparented — so
// decisions and later archive/delete runs see where things really are.
//
// Folders are processed shallowest first, so a folder nested inside another
// reclaimed folder is replaced after it has been carried across into the new
// parent, and its replacement is created there.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
)

const (
	// oldFolderPrefix marks the folder we are replacing, so its owner can see at
	// a glance that it is the superseded one. Idempotent: a folder already
	// carrying the prefix is not renamed again, and the name after the prefix is
	// the name the replacement folder takes.
	oldFolderPrefix = "(old) "
	// newFolderPrefix names the shortcut left inside the replaced folder that
	// points at the replacement.
	newFolderPrefix = "(new) "
)

// reclaimNames splits a folder's current name into the name its replacement
// takes and the "(old) "-prefixed name the folder itself should carry. A folder
// already carrying the prefix was renamed by an earlier run, so its prefixed
// name is already correct and the name behind the prefix is the original. A
// folder named exactly "(old) " is left as it is rather than reduced to an
// empty name.
func reclaimNames(current string) (origName, oldName string) {
	origName = strings.TrimPrefix(current, oldFolderPrefix)
	if origName == "" {
		origName = current
	}
	return origName, oldFolderPrefix + origName
}

var reclaimFoldersCmd = &cobra.Command{
	Use:   "reclaim-folders <email>",
	Short: "Replace folders owned by another account with folders owned by you",
	Long: `Replace every folder owned by <email> with an identically-named folder
owned by the account running the tool, moving the contents across.

Drive has no way to take a folder's ownership away from its owner, so each of
their folders is superseded rather than transferred. For every folder they own,
in the crawled tree (or, with --subtree, in that subtree, which includes the
subtree folder itself; or, with --folder, that one folder alone):

  1. Their folder is renamed "(old) <name>" (skipped if it already is).
  2. A new folder "<name>", owned by you, is created under the same parent —
     an existing one you own is reused, so re-runs never duplicate.
  3. Their folder's sharing is copied onto yours, WITHOUT sending anybody a
     notification email, so nobody loses access when the contents move. The
     owner grant is skipped (you own the replacement), as are grants naming
     you and grants whose user or group Drive reports as deleted.
  4. Everything directly inside their folder is moved into yours.
  5. A shortcut "(new) <name>" is created inside their folder, pointing at
     yours, so anyone who lands on the old folder finds the new one.
  6. If anything could not be moved, a shortcut "(old) <name>" is created
     inside your folder, pointing back at theirs.
  7. Their folder is marked keep, so it is never archived or deleted. Their
     emptied folder plus its "(new) <name>" shortcut is what keeps existing
     links to the folder working and points people at the replacement. The
     keep also marks their folder's still-undecided leftovers keep and clears
     any delete decision on its ancestors, exactly as clicking Keep on the
     folder in the review UI does.

The sharing is copied before anything moves, so nobody is locked out even
briefly. Drive reports a My-Drive folder's inherited grants alongside its own,
and your replacement is created under the same parent, so it starts with the
same inherited access — only what their folder has beyond that is actually
created on yours. Roles are compared, not just grantees: a folder of theirs
that widens an inherited grant (shared with "anyone" as writer inside a parent
shared as reader, say) is matched by an explicit grant on yours, and a grant
your folder already provides at an equal or stronger role is left alone.

Folders are handled shallowest first, so a folder of theirs nested inside
another is replaced inside the new parent. The database is updated as the run
goes — the new folders and shortcuts are recorded, the moved items reparented,
and the replacement's sharing written to folder_permissions — so the snapshot
keeps describing where things actually live and who can reach them; a
re-crawl is still worth running afterwards to pick up anything created since
the last one.

At the end every replacement is printed as a pair of Drive links labelled
"theirs" and "ours".

This command requires the full Drive scope. If the cached token.json only has
read-only access, the tool re-runs consent automatically to obtain it.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		subtree, _ := cmd.Flags().GetString("subtree")
		folder, _ := cmd.Flags().GetString("folder")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		maxErrors, _ := cmd.Flags().GetInt("max-errors")
		concurrency, _ := cmd.Flags().GetInt("concurrency")
		return runReclaimFolders(dbPath, cfgPath, args[0], subtree, folder, dryRun, maxErrors, concurrency)
	},
}

func init() {
	reclaimFoldersCmd.Flags().String("subtree", "", "Google Drive folder ID (crawled, under the crawl root) to scope the run to; the folder itself and everything below it is included. Defaults to the whole crawl root")
	reclaimFoldersCmd.Flags().String("folder", "", "Google Drive folder ID (crawled, under the crawl root) to replace on its own; nothing below it is touched")
	reclaimFoldersCmd.MarkFlagsMutuallyExclusive("subtree", "folder")
	reclaimFoldersCmd.Flags().Bool("dry-run", false, "report what would be replaced without changing anything (read-only scope)")
	reclaimFoldersCmd.Flags().Int("max-errors", 5, "abort once more than this many items fail")
	reclaimFoldersCmd.Flags().Int("concurrency", defaultMoveConcurrency, "how many item moves to run in parallel (all still share the global rate limiter)")
}

// reclaimStats holds a run's tallies. The per-folder move phase updates the
// item counters from its worker pool, so they are guarded by mu; the shared
// error budget (embedded) carries the failure count and abort logic.
type reclaimStats struct {
	*errorBudget
	mu       sync.Mutex
	replaced int
	skipped  int
	moved    int
	left     int
}

func (s *reclaimStats) replace()    { s.mu.Lock(); s.replaced++; s.mu.Unlock() }
func (s *reclaimStats) skip()       { s.mu.Lock(); s.skipped++; s.mu.Unlock() }
func (s *reclaimStats) move(n int)  { s.mu.Lock(); s.moved += n; s.mu.Unlock() }
func (s *reclaimStats) leave(n int) { s.mu.Lock(); s.left += n; s.mu.Unlock() }

// skipErr marks a folder reclaim-folders deliberately left alone — it is gone, no
// longer theirs, or not writable by us. Skips are reported but, unlike a
// failure, never spend the error budget.
type skipErr struct{ reason string }

func (e skipErr) Error() string { return e.reason }

// reclaimResult is one folder's outcome, printed at the end as a
// "theirs"/"ours" pair of Drive links.
type reclaimResult struct {
	path     string // the folder's path in the crawled tree, for the heading
	name     string // the name without the "(old) " prefix, i.e. what ours is called
	theirsID string
	oursID   string // empty only in a dry run, when the replacement does not exist yet
	moved    int
	left     int // items that stayed behind in their folder
	grants   int // grants copied from their folder onto ours
}

func runReclaimFolders(dbPath, cfgPath, email, subtree, folder string, dryRun bool, maxErrors, concurrency int) error {
	if subtree != "" && folder != "" {
		return fmt.Errorf("--subtree and --folder cannot be combined: --subtree scopes the run to a whole subtree, --folder replaces that one folder only")
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
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
	// Reclaiming moves live files based on the snapshot; a config root that no
	// longer matches what was crawled means every parent decision could be
	// wrong. Same refusal as archive and pack.
	if crawlRoot != cfg.Crawl.Root.ID {
		return fmt.Errorf("crawl root in config (%s, %q) does not match the root in the database (%s); crawl.root.id changed since the last crawl — re-run `drive-cleanup crawl` to rebuild the snapshot before reclaiming folders",
			cfg.Crawl.Root.ID, cfg.Crawl.Root.Name, crawlRoot)
	}

	// Both --subtree and --folder name one crawled folder: --subtree opens the
	// run onto everything below it, --folder narrows it to that folder alone.
	// Requiring either to be under the crawl root also keeps the archive tree
	// (which must live outside) out of reach.
	scope := reclaimScope{driveID: crawlRoot, recursive: true}
	if named := subtree + folder; named != "" {
		path, err := scopeFolderPath(db, crawlRoot, named)
		if err != nil {
			return err
		}
		scope = reclaimScope{driveID: named, path: path, recursive: subtree != ""}
	}

	var targets []reclaimTarget
	if scope.recursive {
		if pending, err := countPendingFolders(db, scope.driveID); err != nil {
			return err
		} else if pending > 0 {
			log.Printf("WARN the crawl is incomplete (%d folder(s) not fully listed); folders it never reached will not be reclaimed. Re-run crawl for a complete pass", pending)
		}

		targets, err = foldersOwnedBy(db, scope.driveID, email)
		if err != nil {
			return err
		}
		// The crawl root is the boundary of the snapshot and is never renamed or
		// replaced, even when the account being reclaimed owns it.
		kept := targets[:0]
		for _, t := range targets {
			if t.driveID != crawlRoot {
				kept = append(kept, t)
			}
		}
		targets = kept
	} else {
		// --folder names the one folder to replace, so anything that would make it
		// no target at all is an error rather than a quiet empty run.
		if scope.driveID == crawlRoot {
			return fmt.Errorf("folder %s is the crawl root; reclaim-folders never replaces the root of the crawled tree", scope.driveID)
		}
		t, owned, err := folderOwnedByAccount(db, scope.driveID, email)
		if err != nil {
			return err
		}
		if !owned {
			return fmt.Errorf("folder %s (%q) is not owned by %s according to the snapshot; re-run `drive-cleanup crawl` if its ownership changed since the last crawl", scope.driveID, t.name, email)
		}
		targets = []reclaimTarget{t}
	}
	if len(targets) == 0 {
		fmt.Fprintf(os.Stderr, "No folders owned by %s%s; nothing to do.\n", email, scope.note())
		return nil
	}

	ctx, cancel := cancelOnSignal()
	defer cancel()

	// A dry run only reads, so request the narrower scope — previewing never
	// forces a write-scope re-consent.
	driveScope := drive.DriveScope
	if dryRun {
		driveScope = drive.DriveReadonlyScope
	}
	svc, err := newDriveService(ctx, driveScope)
	if err != nil {
		return err
	}
	about, err := svc.About.Get().Fields("user").Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("fetching current user info: %w", err)
	}
	me := about.User
	if strings.EqualFold(me.EmailAddress, email) {
		return fmt.Errorf("%s is the account you are signed in as; reclaim-folders replaces somebody else's folders with yours", email)
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "DRY RUN: no changes will be made. Would replace %d folder(s) owned by %s%s with folders owned by %s.\n",
			len(targets), email, scope.note(), me.EmailAddress)
	} else {
		fmt.Fprintf(os.Stderr, "About to replace %d folder(s) owned by %s%s with folders owned by %s: each is renamed %q, a folder of the same name is created beside it, and its contents are moved across.\n",
			len(targets), email, scope.note(), me.EmailAddress, oldFolderPrefix+"<name>")
		if !promptYesNo("Continue? [y/N] ") {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return nil
		}
	}

	moveCtx, moveCancel := context.WithCancel(ctx)
	defer moveCancel()
	r := &reclaimer{
		db:      db,
		svc:     svc,
		limiter: rate.NewLimiter(rate.Limit(20), 20),
		rec:     &opLog{db: db, account: me.EmailAddress, command: "reclaim-folders"},
		me:      me,
		account: email,
		dryRun:  dryRun,
		workers: concurrency,
		stats:   &reclaimStats{errorBudget: &errorBudget{cmd: "reclaim-folders", maxErrors: maxErrors, cancel: moveCancel}},
	}

	var results []reclaimResult
	prog := newProgress()
	for i, t := range targets {
		if err := moveCtx.Err(); err != nil {
			break
		}
		path, err := nodePath(db, t.driveID)
		if err != nil {
			return err
		}
		res, err := r.replace(moveCtx, t, strings.Join(path, " / "))
		prog.tick("reclaim-folders: %d/%d folder(s) processed", i+1, len(targets))
		var skip skipErr
		switch {
		case errors.As(err, &skip):
			log.Printf("SKIP %q (%s): %s", t.name, t.driveID, skip.reason)
			r.stats.skip()
			continue
		case err != nil:
			r.stats.fail("ERROR reclaiming %q (%s): %v", t.name, t.driveID, err)
			continue
		}
		r.stats.replace()
		results = append(results, res)
	}

	printReclaimPairs(results, email, me.EmailAddress, dryRun)

	if dryRun {
		fmt.Fprintf(os.Stderr, "\nWould replace %d folder(s), moving %d item(s); %d folder(s) skipped, %d failure(s).\n",
			r.stats.replaced, r.stats.moved, r.stats.skipped, r.stats.failedCount())
	} else {
		fmt.Fprintf(os.Stderr, "\nReplaced %d folder(s); %d item(s) moved, %d left behind, %d folder(s) skipped, %d failure(s).\n",
			r.stats.replaced, r.stats.moved, r.stats.left, r.stats.skipped, r.stats.failedCount())
	}
	return r.stats.err
}

// reclaimScope is the part of the tree a run covers: the whole crawl root by
// default, the subtree named by --subtree, or the single folder named by
// --folder.
type reclaimScope struct {
	driveID   string // the folder the run is scoped to; the crawl root when unscoped
	path      string // its path relative to the crawl root, empty when unscoped
	recursive bool   // everything below driveID (--subtree), or driveID alone (--folder)
}

// note renders the " under <path>" / " at <path>" clause for messages, empty
// when the run covers the whole crawl root.
func (s reclaimScope) note() string {
	switch {
	case s.path == "":
		return ""
	case s.recursive:
		return fmt.Sprintf(" under %q", s.path)
	default:
		return fmt.Sprintf(" at %q", s.path)
	}
}

// scopeFolderPath checks that driveID is a folder crawled under crawlRoot — as
// both --subtree and --folder must be — and returns its path relative to the
// root, for the scope clause in messages.
func scopeFolderPath(db *sql.DB, crawlRoot, driveID string) (string, error) {
	typ, err := nodeTypeByDriveID(db, driveID)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("folder %s not found in the database; it must be a folder crawled under the crawl root", driveID)
	}
	if err != nil {
		return "", err
	}
	if typ != typeFolder {
		return "", fmt.Errorf("%s is a %s, not a folder", driveID, typ)
	}
	if inside, err := nodeInSubtree(db, crawlRoot, driveID); err != nil {
		return "", err
	} else if !inside {
		return "", fmt.Errorf("folder %s is not under the crawl root; reclaim-folders only acts on the crawled tree", driveID)
	}
	return subtreeRelativePath(db, driveID)
}

// reclaimer carries everything one run needs: the Drive client and its shared
// rate limiter, the audit-log recorder, who we are, and whose folders we are
// replacing.
type reclaimer struct {
	db      *sql.DB
	svc     *drive.Service
	limiter *rate.Limiter
	rec     *opLog
	me      *drive.User
	account string // the email whose folders are being replaced
	dryRun  bool
	workers int
	stats   *reclaimStats
}

// replace supersedes one of their folders with one of ours: rename, create,
// move the contents, cross-link with shortcuts, then record it all in the
// snapshot. A returned skipErr means the folder was deliberately left alone;
// any other error means this folder failed and spends the error budget.
// Failures moving individual items do not fail the folder — they are counted,
// left behind, and reflected in the shortcut back to their folder.
func (r *reclaimer) replace(ctx context.Context, t reclaimTarget, path string) (reclaimResult, error) {
	res := reclaimResult{path: path, theirsID: t.driveID}

	f, err := r.getFolder(ctx, t.driveID)
	if err != nil {
		if isNotFound(err) {
			return res, skipErr{"no longer on Drive"}
		}
		return res, fmt.Errorf("fetching folder: %w", err)
	}
	switch {
	case f.Trashed:
		return res, skipErr{"in the trash"}
	case f.MimeType != folderMimeType:
		return res, skipErr{fmt.Sprintf("no longer a folder (mimeType %q)", f.MimeType)}
	case !ownedByAccount(f, r.account):
		return res, skipErr{fmt.Sprintf("no longer owned by %s", r.account)}
	case len(f.Parents) == 0:
		return res, skipErr{"has no parent to create the replacement under"}
	case f.Capabilities != nil && (!f.Capabilities.CanRename || !f.Capabilities.CanAddChildren):
		return res, skipErr{fmt.Sprintf("%s cannot rename it or add items to it", r.me.EmailAddress)}
	}
	// Legacy multi-parenting: the replacement can only sit in one place, so it
	// goes beside the first parent and the rest are called out for manual review.
	parent := f.Parents[0]
	if len(f.Parents) > 1 {
		log.Printf("WARN %q (%s) has %d parents (%s); replacing it under %s only",
			f.Name, f.Id, len(f.Parents), strings.Join(f.Parents, ", "), parent)
	}

	origName, oldName := reclaimNames(f.Name)
	res.name = origName
	if f.Name != oldName && !r.dryRun {
		if err := r.rec.renameFile(ctx, r.svc, r.limiter, f.Id, oldName); err != nil {
			return res, fmt.Errorf("renaming to %q: %w", oldName, err)
		}
		detailf("OK renamed %q -> %q (%s)", f.Name, oldName, f.Id)
		f.Name = oldName // so the log lines below name the folder as it is now
	}

	// Find-before-create, so a re-run (or a run interrupted midway) adopts the
	// replacement it already made instead of building a second one. Only a
	// candidate we own counts: a same-named folder owned by somebody else is a
	// different folder, not our replacement.
	ours, err := r.findOurFolder(ctx, parent, origName)
	if err != nil {
		return res, fmt.Errorf("looking for an existing %q under %s: %w", origName, parent, err)
	}
	if ours == nil && !r.dryRun {
		if ours, err = r.rec.createFolder(ctx, r.svc, r.limiter, parent, origName); err != nil {
			return res, fmt.Errorf("creating %q under %s: %w", origName, parent, err)
		}
		detailf("OK created %q (%s) under %s", origName, ours.Id, parent)
	}
	if ours != nil {
		res.oursID = ours.Id
	}

	// Sharing is recreated before anything moves, so nobody who had access
	// through their folder is locked out even briefly.
	var oursPerms []*drive.Permission
	if !r.dryRun {
		if oursPerms, res.grants, err = r.copyFolderPermissions(ctx, f, ours); err != nil {
			r.stats.fail("ERROR copying the sharing of %q (%s) onto %q: %v", f.Name, f.Id, origName, err)
		}
	}

	// One listing serves both the move and the look for the "(new) <name>"
	// shortcut an earlier run may have left inside — matched by target rather
	// than by name, so a renamed shortcut is still recognised and not moved.
	children, err := listChildren(ctx, r.svc, r.limiter, f.Id,
		"nextPageToken, files(id, name, mimeType, shortcutDetails(targetId))")
	if err != nil {
		return res, fmt.Errorf("listing the folder's contents: %w", err)
	}
	var (
		toMove   []*drive.File
		backlink *drive.File
	)
	for _, c := range children {
		if ours != nil && backlink == nil && isShortcutTo(c, ours.Id) {
			backlink = c
			continue
		}
		toMove = append(toMove, c)
	}

	if r.dryRun {
		// Nothing was renamed, created or moved, so report the plan and stop
		// before any bookkeeping. The replacement usually does not exist yet, so
		// the sharing preview is simply everything worth copying off theirs.
		res.moved = len(toMove)
		r.stats.move(res.moved)
		baseline := parent // what the replacement would inherit if created now
		if ours != nil {
			baseline = ours.Id
		}
		missing, _, err := permissionsToCopy(ctx, r.svc, r.limiter, f.Id, baseline, r.me)
		if err != nil {
			return res, err
		}
		res.grants = len(missing)
		return res, nil
	}

	moved := r.moveChildren(ctx, f, ours, toMove)
	res.moved = len(moved)
	res.left = len(toMove) - len(moved)
	r.stats.move(res.moved)
	r.stats.leave(res.left)

	if backlink == nil {
		sc, err := r.rec.createShortcut(ctx, r.svc, r.limiter, f.Id, newFolderPrefix+origName, ours.Id)
		if err != nil {
			// Cosmetic next to the move: log it against the budget and carry on to
			// the bookkeeping, so the snapshot still matches what really happened.
			r.stats.fail("ERROR creating the %q shortcut in %q (%s): %v", newFolderPrefix+origName, oldName, f.Id, err)
		} else {
			backlink = sc
			detailf("OK created shortcut %q (%s) in %q", sc.Name, sc.Id, oldName)
		}
	}

	// Leftovers stay in their folder, so our folder gets a way back to them.
	var leftoverLink *drive.File
	if res.left > 0 {
		if leftoverLink, err = r.ensureShortcut(ctx, ours.Id, oldName, f.Id); err != nil {
			r.stats.fail("ERROR creating the %q shortcut in %q (%s): %v", oldName, origName, ours.Id, err)
		}
	}

	if err := r.record(t, f, ours, origName, parent, moved, backlink, leftoverLink, oursPerms); err != nil {
		return res, fmt.Errorf("recording the replacement in the database: %w", err)
	}
	return res, nil
}

// getFolder fetches the live state reclaim-folders decides on: whether the folder is
// still there, still a folder, still theirs, and still writable by us.
func (r *reclaimer) getFolder(ctx context.Context, driveID string) (*drive.File, error) {
	if err := r.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return r.svc.Files.Get(driveID).
		Fields("id, name, mimeType, trashed, parents, owners(emailAddress, permissionId), capabilities(canRename, canAddChildren)").
		SupportsAllDrives(true).
		Context(ctx).Do()
}

// findOurFolder returns the subfolder of parentID named name that the running
// account owns, or nil if there is none — the replacement an earlier run left
// behind.
func (r *reclaimer) findOurFolder(ctx context.Context, parentID, name string) (*drive.File, error) {
	matches, err := findChildrenNamed(ctx, r.svc, r.limiter, parentID, name, folderMimeType,
		"id, name, owners(emailAddress, permissionId)")
	if err != nil {
		return nil, err
	}
	for _, m := range matches {
		if ownedByAccount(m, r.me.EmailAddress) {
			return m, nil
		}
	}
	return nil, nil
}

// ensureShortcut returns the shortcut named name inside parentID that points at
// targetID, creating it if it is not there yet.
func (r *reclaimer) ensureShortcut(ctx context.Context, parentID, name, targetID string) (*drive.File, error) {
	matches, err := findChildrenNamed(ctx, r.svc, r.limiter, parentID, name, shortcutMimeType,
		"id, name, mimeType, shortcutDetails(targetId)")
	if err != nil {
		return nil, err
	}
	for _, m := range matches {
		if isShortcutTo(m, targetID) {
			return m, nil
		}
	}
	return r.rec.createShortcut(ctx, r.svc, r.limiter, parentID, name, targetID)
}

// copyFolderPermissions recreates their folder's sharing on our replacement, so
// nobody who reached the content through a grant on the old folder loses it
// when the content moves. Returns the permissions our folder ends up with, for
// the snapshot, and how many were created. See copyMissingPermissions for what
// is copied and what is deliberately left alone.
func (r *reclaimer) copyFolderPermissions(ctx context.Context, theirs, ours *drive.File) ([]*drive.Permission, int, error) {
	return copyMissingPermissions(ctx, r.svc, r.limiter, r.rec, theirs.Id, theirs.Name, ours.Id, ours.Name, r.me, r.stats.fail)
}

// isShortcutTo reports whether f is a Drive shortcut pointing at targetID.
func isShortcutTo(f *drive.File, targetID string) bool {
	return f.MimeType == shortcutMimeType && f.ShortcutDetails != nil && f.ShortcutDetails.TargetId == targetID
}

// moveChildren moves every item out of their folder into ours, in parallel, and
// returns the ids that made it. A failure is counted against the error budget
// and the item simply stays where it is.
func (r *reclaimer) moveChildren(ctx context.Context, theirs, ours *drive.File, items []*drive.File) []string {
	var (
		mu    sync.Mutex
		moved = make([]string, 0, len(items))
	)
	forEachConcurrent(ctx, r.workers, items, func(c *drive.File) {
		if err := r.rec.moveFile(ctx, r.svc, r.limiter, c.Id, ours.Id, theirs.Id); err != nil {
			r.stats.fail("ERROR moving %q (%s) out of %q: %v", c.Name, c.Id, theirs.Name, err)
			return
		}
		detailf("OK moved %q (%s) into %q (%s)", c.Name, c.Id, ours.Name, ours.Id)
		mu.Lock()
		moved = append(moved, c.Id)
		mu.Unlock()
	})
	return moved
}

// record writes one replacement into the snapshot, in a single transaction: the
// replacement folder and the shortcuts become nodes rows, every item that moved
// is reparented under the replacement, and their emptied folder is marked keep.
//
// The keep reuses the review server's own propagation (markInTx), so the
// invariants the review UI relies on survive: their folder's undecided
// leftovers are kept with it, delete decisions on its ancestors are cleared so
// it cannot sit inside a delete subtree, and ancestors are rolled back up.
func (r *reclaimer) record(t reclaimTarget, theirs, ours *drive.File, origName, parentDriveID string, moved []string, backlink, leftoverLink *drive.File, oursPerms []*drive.Permission) error {
	// Where the replacement sits in the snapshot. The live parent is normally
	// the folder's recorded parent, but for a folder nested inside another
	// reclaimed one it is the replacement created a step earlier; fall back to
	// the recorded parent if that folder was never crawled.
	parentRow, err := folderRefByDriveID(r.db, parentDriveID)
	var parentRowID int64
	switch err {
	case nil:
		parentRowID = parentRow.rowID
	case sql.ErrNoRows:
		var ok bool
		if parentRowID, ok, err = parentRowOf(r.db, t.driveID); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("folder %s has no recorded parent to place the replacement under", t.driveID)
		}
	default:
		return err
	}

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	oursRow, _, _, _, err := upsertNode(tx, node{
		driveID:      ours.Id,
		name:         origName,
		typ:          typeFolder,
		mimeType:     folderMimeType,
		ownerEmail:   nullString(r.me.EmailAddress),
		ownerID:      nullString(r.me.PermissionId),
		ownerDisplay: nullString(r.me.DisplayName),
		parentID:     sql.NullInt64{Int64: parentRowID, Valid: true},
		canEdit:      true,
	}, true)
	if err != nil {
		return err
	}

	// Items created since the last crawl have no row to reparent; the
	// replacement then stays "not fully listed" so the next crawl lists it.
	unrecorded := 0
	for _, id := range moved {
		found, err := reparentNode(tx, id, oursRow)
		if err != nil {
			return err
		}
		if !found {
			unrecorded++
		}
	}
	if backlink != nil {
		if _, _, _, _, err := upsertNode(tx, node{
			driveID: backlink.Id, name: backlink.Name, typ: typeShortcut, mimeType: shortcutMimeType,
			ownerEmail: nullString(r.me.EmailAddress), ownerID: nullString(r.me.PermissionId),
			ownerDisplay: nullString(r.me.DisplayName), shortcutTarget: nullString(ours.Id),
			parentID: sql.NullInt64{Int64: t.rowID, Valid: true}, canEdit: true,
		}, true); err != nil {
			return err
		}
	}
	if leftoverLink != nil {
		if _, _, _, _, err := upsertNode(tx, node{
			driveID: leftoverLink.Id, name: leftoverLink.Name, typ: typeShortcut, mimeType: shortcutMimeType,
			ownerEmail: nullString(r.me.EmailAddress), ownerID: nullString(r.me.PermissionId),
			ownerDisplay: nullString(r.me.DisplayName), shortcutTarget: nullString(theirs.Id),
			parentID: sql.NullInt64{Int64: oursRow, Valid: true}, canEdit: true,
		}, true); err != nil {
			return err
		}
	}
	if unrecorded == 0 {
		if err := markChildrenDone(tx, oursRow); err != nil {
			return err
		}
	}
	// The replacement's sharing, as it stands after the copy — folder_permissions
	// is what tells a later run (or a human) who could reach a folder.
	if oursPerms != nil {
		if err := replacePermissions(tx, ours.Id, permissionRows(oursPerms)); err != nil {
			return err
		}
	}

	// Their emptied folder is always kept, never deleted: it and the "(new)
	// <name>" shortcut inside it are what stop links and bookmarks pointing at
	// the old folder from breaking, and what lead whoever follows one to the
	// replacement. "preserve" is the review UI's default for a keep, so anything
	// left behind that was already marked delete stays delete.
	rec := make(map[int64]string)
	if _, err := markInTx(tx, t.driveID, decisionKeep, "preserve", rec); err != nil {
		return err
	}
	// The replacement inherits whatever its new contents say: all-delete makes it
	// delete, anything else leaves it for the review UI to decide.
	if err := rollupSelfAndAncestors(tx, oursRow, rec); err != nil {
		return err
	}
	return tx.Commit()
}

// printReclaimPairs writes the run's report to stdout: one block per folder
// with the "theirs" and "ours" Drive links side by side.
func printReclaimPairs(results []reclaimResult, theirEmail, ourEmail string, dryRun bool) {
	if len(results) == 0 {
		return
	}
	heading := "Replaced folders"
	if dryRun {
		heading = "Folders that would be replaced"
	}
	fmt.Printf("%s (theirs = %s, ours = %s):\n", heading, theirEmail, ourEmail)
	for _, res := range results {
		fmt.Printf("\n%s\n", res.path)
		fmt.Printf("  theirs: %s\n", driveFolderURL(res.theirsID))
		if res.oursID == "" {
			fmt.Printf("  ours:   (would be created as %q)\n", res.name)
		} else {
			fmt.Printf("  ours:   %s\n", driveFolderURL(res.oursID))
		}
		switch {
		case dryRun:
			fmt.Printf("          %d item(s) would move, %d grant(s) would be copied\n", res.moved, res.grants)
		case res.left > 0:
			fmt.Printf("          %d item(s) moved, %d left behind, %d grant(s) copied; theirs kept\n",
				res.moved, res.left, res.grants)
		default:
			fmt.Printf("          %d item(s) moved, %d grant(s) copied; theirs kept\n", res.moved, res.grants)
		}
	}
}

func driveFolderURL(driveID string) string {
	return "https://drive.google.com/drive/folders/" + driveID
}
