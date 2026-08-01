package main

// The archive command soft-deletes everything marked delete in the review UI:
// files move into a configured archive folder (config archive.root) whose
// inside mirrors the crawl root's folder structure as "ARCH "-prefixed replica
// folders, so an archived file remains findable by its original location and
// name. Individually-added permissions on archived files are replaced with the
// running account's own, so archived content stops being shared. Delete-marked
// folders that are empty on Drive (their contents archived or gone) are
// archived too, descendants before ancestors. The restore command reverses one
// item; the delete command permanently deletes archived items.

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

const (
	// archReplicaPrefix marks a replica folder inside the archive tree so it is
	// never confused with its original.
	archReplicaPrefix = "ARCH "
	// maxReplicaNameRunes bounds a replica folder's name. Drive tolerates very
	// long names, but a fixed bound keeps the name deterministic for the
	// find-before-create lookup no matter how long the original's name is.
	maxReplicaNameRunes = 200
	// archivedPermissionRole is what the running account grants itself on an
	// archived file before revoking the other direct permissions — writer, so
	// the file can still be moved by restore and delete.
	archivedPermissionRole = "writer"
)

// replicaName returns the archive replica folder name for an original folder
// name: prefixed and rune-safely truncated.
func replicaName(original string) string {
	r := []rune(archReplicaPrefix + original)
	if len(r) > maxReplicaNameRunes {
		r = r[:maxReplicaNameRunes]
	}
	return string(r)
}

var archiveCmd = &cobra.Command{
	Use:   "archive",
	Short: "Move files marked delete into the archive folder, preserving folder structure",
	Long: `Move every file marked delete in the review UI into the archive folder
(config archive.root), recreating the file's ancestor folders beneath it as
"ARCH "-prefixed replicas so archived files keep their original location and
name. Replica folders are created without the originals' sharing. After each
file moves, its individually-added permissions (users and groups) are replaced
with the running account's own, so the archived copy stops being shared; the
running account is added first, and nothing is removed if that fails.

Delete-marked folders are archived too when a live check shows them empty
(their contents already archived or gone) — descendants before ancestors, so a
folder's turn comes after everything inside it. Folders keep their permissions.

The archive folder must be a regular My-Drive folder OUTSIDE the crawl root:
inside it, the archive would inherit the crawl root's sharing. The archive tree
is crawled (after the crawl root) so archived files stay in the snapshot and
remain packable for ownership transfer; the review UI simply hides it.

Pass --folder <id> (a crawled folder) to archive only that subtree. Use the
restore command to bring an archived item back, and delete to permanently
delete archived items.

This command requires the full Drive scope. If the cached token.json only has
read-only access, the tool re-runs consent automatically to obtain it.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		maxErrors, _ := cmd.Flags().GetInt("max-errors")
		subfolder, _ := cmd.Flags().GetString("folder")
		concurrency, _ := cmd.Flags().GetInt("concurrency")
		return runArchive(dbPath, cfgPath, subfolder, dryRun, maxErrors, concurrency)
	},
}

func init() {
	archiveCmd.Flags().Bool("dry-run", false, "report what would be archived without changing anything (read-only scope)")
	archiveCmd.Flags().Int("max-errors", 5, "abort once more than this many items fail")
	archiveCmd.Flags().String("folder", "", "Google Drive folder ID (crawled, under the crawl root) to archive only that subtree")
	archiveCmd.Flags().Int("concurrency", defaultMoveConcurrency, "how many file moves to run in parallel (all still share the global rate limiter)")
}

// archiveStats holds an archive run's tallies, updated from the worker pool.
type archiveStats struct {
	*errorBudget
	mu      sync.Mutex
	moved   int
	already int
	skipped int
}

func (s *archiveStats) move()         { s.mu.Lock(); s.moved++; s.mu.Unlock() }
func (s *archiveStats) alreadyThere() { s.mu.Lock(); s.already++; s.mu.Unlock() }
func (s *archiveStats) skip()         { s.mu.Lock(); s.skipped++; s.mu.Unlock() }

func (s *archiveStats) movedCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.moved }

// replicaRef is one resolved replica folder: its Drive ID and its nodes row.
type replicaRef struct {
	driveID string
	rowID   int64
}

// replicaResolver builds and caches the replica folder chain for original
// folders. It runs strictly sequentially (one resolver, before the concurrent
// move phase), so find-before-create can never race itself into duplicates.
// Resolved replicas are cached on the original folder's row
// (archive_folder_drive_id) and inserted as nodes rows under the archive tree,
// so the snapshot keeps describing where archived items actually live.
type replicaResolver struct {
	db      *sql.DB
	svc     *drive.Service
	limiter *rate.Limiter
	rec     *opLog
	me      string // running account's email, recorded as replica rows' owner

	archiveRoot replicaRef

	// verified maps an original folder's Drive ID (or the crawl root's) to its
	// live-verified replica for this run.
	verified map[string]replicaRef
}

// resolve returns the replica folder that children of the original folder
// parentDriveID belong in, creating any missing replicas along the way. The
// crawl root's replica is the archive root itself.
func (r *replicaResolver) resolve(ctx context.Context, parentDriveID string) (replicaRef, error) {
	if ref, ok := r.verified[parentDriveID]; ok {
		return ref, nil
	}
	chain, err := folderChainToRoot(r.db, parentDriveID)
	if err != nil {
		return replicaRef{}, err
	}
	cur := r.archiveRoot
	for _, folder := range chain {
		if cur, err = r.ensure(ctx, folder, cur); err != nil {
			return replicaRef{}, err
		}
	}
	r.verified[parentDriveID] = cur
	return cur, nil
}

// ensure returns the live replica of one original folder under parentReplica,
// resolving in order: the cached id (verified live; a trashed or deleted
// replica is re-created per the design), an existing same-named folder
// (adopted, so re-runs and crashes never duplicate), or a newly created one.
func (r *replicaResolver) ensure(ctx context.Context, folder archiveTarget, parentReplica replicaRef) (replicaRef, error) {
	if ref, ok := r.verified[folder.driveID]; ok {
		return ref, nil
	}
	name := replicaName(folder.name)
	var replica *drive.File
	if folder.archiveFolder.Valid {
		f, err := getFileState(ctx, r.svc, r.limiter, folder.archiveFolder.String)
		switch {
		case isNotFound(err):
			// Cached replica no longer exists; fall through and re-create.
		case err != nil:
			return replicaRef{}, fmt.Errorf("verifying cached replica %s of %q: %w", folder.archiveFolder.String, folder.name, err)
		case !f.Trashed:
			replica = f
		}
	}
	if replica == nil {
		f, err := findChildFolder(ctx, r.svc, r.limiter, parentReplica.driveID, name)
		if err != nil {
			return replicaRef{}, fmt.Errorf("looking up replica %q: %w", name, err)
		}
		replica = f
	}
	if replica == nil {
		f, err := r.rec.createFolder(ctx, r.svc, r.limiter, parentReplica.driveID, name)
		if err != nil {
			return replicaRef{}, fmt.Errorf("creating replica %q under %s: %w", name, parentReplica.driveID, err)
		}
		detailf("OK created replica %q (%s)", name, f.Id)
		replica = f
	}
	rowID, err := upsertReplicaRow(r.db, replica.Id, name, sql.NullInt64{Int64: parentReplica.rowID, Valid: true}, r.me)
	if err != nil {
		return replicaRef{}, err
	}
	if err := setArchiveFolder(r.db, folder.driveID, replica.Id); err != nil {
		return replicaRef{}, err
	}
	ref := replicaRef{driveID: replica.Id, rowID: rowID}
	r.verified[folder.driveID] = ref
	return ref, nil
}

// upsertReplicaRow records a replica folder (or the archive root) in the
// snapshot. children_done is set — the row's children are exactly the archived
// rows we reparent under it, so there is nothing pending to list; a --refresh
// crawl re-lists it like any other folder.
func upsertReplicaRow(db *sql.DB, driveID, name string, parentRowID sql.NullInt64, ownerEmail string) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rowID, _, _, _, err := upsertNode(tx, node{
		driveID:    driveID,
		name:       name,
		typ:        typeFolder,
		mimeType:   folderMimeType,
		ownerEmail: nullString(ownerEmail),
		parentID:   parentRowID,
		canEdit:    true,
	}, parentRowID.Valid)
	if err != nil {
		return 0, err
	}
	if err := markChildrenDone(tx, rowID); err != nil {
		return 0, err
	}
	return rowID, tx.Commit()
}

func runArchive(dbPath, cfgPath, subfolder string, dryRun bool, maxErrors, concurrency int) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Archive.Root.validate("archive.root"); err != nil {
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
	// Archiving moves live files based on the snapshot; a config root that no
	// longer matches what was crawled means every parent decision could be
	// wrong. Same refusal as pack.
	if crawlRoot != cfg.Crawl.Root.ID {
		return fmt.Errorf("crawl root in config (%s, %q) does not match the root in the database (%s); crawl.root.id changed since the last crawl — re-run `drive-cleanup crawl` to rebuild the snapshot before archiving",
			cfg.Crawl.Root.ID, cfg.Crawl.Root.Name, crawlRoot)
	}
	if cfg.Archive.Root.ID == crawlRoot {
		return fmt.Errorf("archive.root.id equals crawl.root.id (%s); the archive folder must be a separate folder outside the crawl root", crawlRoot)
	}

	// An optional subfolder scopes the run to one crawled folder of the tree.
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
		if inside, err := nodeInSubtree(db, crawlRoot, subfolder); err != nil {
			return err
		} else if !inside {
			return fmt.Errorf("subfolder %s is not under the crawl root; archive only acts on the crawled tree", subfolder)
		}
		if subfolderPath, err = subtreeRelativePath(db, subfolder); err != nil {
			return err
		}
	}

	if pending, err := countPendingFolders(db, ""); err != nil {
		return err
	} else if pending > 0 {
		return fmt.Errorf("the crawl is incomplete (%d folder(s) not fully listed); the database may be missing items. Re-run crawl first for a complete archive", pending)
	}

	files, err := archivableFiles(db, subfolder)
	if err != nil {
		return err
	}
	folders, err := archivableFolders(db, subfolder)
	if err != nil {
		return err
	}
	if len(files)+len(folders) == 0 {
		fmt.Fprintln(os.Stderr, "Nothing marked delete that is not already archived; nothing to do.")
		return nil
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
	rec := &opLog{db: db, account: me.EmailAddress, command: "archive"}

	archiveFolder, err := getConfiguredFolder(ctx, svc, cfg.Archive.Root, "archive.root")
	if err != nil {
		return err
	}
	if archiveFolder.DriveId != "" {
		return fmt.Errorf("archive folder %s is in a shared drive (%s); it must be a regular My-Drive folder — archived files may still be owned by third parties, which a shared drive cannot hold", archiveFolder.Id, archiveFolder.DriveId)
	}
	if inside, err := folderInsideRoot(ctx, svc, archiveFolder, crawlRoot); err != nil {
		return fmt.Errorf("checking the archive folder against the crawl root: %w", err)
	} else if inside {
		return fmt.Errorf("archive folder %s (%q) is inside the crawl root %s; move it outside — inside, the archive inherits the crawl root's sharing", archiveFolder.Id, archiveFolder.Name, crawlRoot)
	}

	// All Drive calls below share this one limiter; it — not the worker count —
	// is the quota safety cap (see pack).
	limiter := rate.NewLimiter(rate.Limit(20), 20)

	scopeNote := ""
	if subfolder != "" {
		scopeNote = fmt.Sprintf(" under subfolder %q", subfolderPath)
	}
	if dryRun {
		fmt.Fprintf(os.Stderr, "DRY RUN: no changes will be made. Would archive %d file(s) and %d empty folder(s) marked delete%s into %q.\n",
			len(files), len(folders), scopeNote, archiveFolder.Name)
	} else {
		fmt.Fprintf(os.Stderr, "About to archive %d file(s) and up to %d folder(s) marked delete%s into %q (%s), replacing archived files' individually-added permissions.\n",
			len(files), len(folders), scopeNote, archiveFolder.Name, archiveFolder.Id)
		if !promptYesNo("Continue? [y/N] ") {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return nil
		}
	}

	// Resolve every replica folder up front, sequentially: the concurrent move
	// phase then only reads the resolver's cache, so replicas can never be
	// created twice. A dry run skips this entirely (it would create folders).
	resolver := &replicaResolver{
		db: db, svc: svc, limiter: limiter, rec: rec, me: me.EmailAddress,
		verified: make(map[string]replicaRef),
	}
	if !dryRun {
		rootRow, err := upsertReplicaRow(db, archiveFolder.Id, archiveFolder.Name, sql.NullInt64{}, me.EmailAddress)
		if err != nil {
			return fmt.Errorf("recording the archive root: %w", err)
		}
		resolver.archiveRoot = replicaRef{driveID: archiveFolder.Id, rowID: rootRow}

		seen := make(map[string]bool)
		var parents []string
		for _, t := range files {
			if p := t.parentDriveID.String; !seen[p] {
				seen[p] = true
				parents = append(parents, p)
			}
		}
		for _, t := range folders {
			if p := t.parentDriveID.String; !seen[p] {
				seen[p] = true
				parents = append(parents, p)
			}
		}
		for _, p := range parents {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := resolver.resolve(ctx, p); err != nil {
				return fmt.Errorf("preparing the archive replica of folder %s: %w", p, err)
			}
		}
	}

	moveCtx, moveCancel := context.WithCancel(ctx)
	defer moveCancel()
	stats := &archiveStats{errorBudget: &errorBudget{cmd: "archive", maxErrors: maxErrors, cancel: moveCancel}}
	workers := concurrency
	if dryRun {
		workers = 1
	}
	prog := newProgress()

	// archiveOne moves one item into its parent's replica and records the
	// archival. Moves are optimistic (one files.update, removing the recorded
	// parent); a failure is diagnosed from the item's live state, pack-style.
	// Reports whether the item ended up archived (moved or already there).
	archiveOne := func(t archiveTarget) bool {
		parent := t.parentDriveID.String
		replica, ok := resolver.verified[parent]
		if !ok { // resolved up front; missing means the pre-phase was interrupted
			return false
		}
		err := rec.moveFile(moveCtx, svc, limiter, t.driveID, replica.driveID, parent)
		if err == nil {
			detailf("OK %s %q (%s) -> archive", t.typ, t.name, t.driveID)
			if merr := markArchived(db, t.driveID, parent, replica.rowID); merr != nil {
				stats.fail("ERROR recording archival of %q (%s): %v", t.name, t.driveID, merr)
				return false
			}
			stats.move()
			return true
		}
		if moveCtx.Err() != nil {
			return false
		}
		f, gerr := getFileState(moveCtx, svc, limiter, t.driveID)
		if moveCtx.Err() != nil {
			return false
		}
		switch {
		case isNotFound(gerr):
			log.Printf("SKIP %q (%s): no longer exists", t.name, t.driveID)
			stats.skip()
		case gerr != nil:
			stats.fail("ERROR %q (%s): move failed (%v) and live lookup failed (%v)", t.name, t.driveID, err, gerr)
		case f.Trashed:
			log.Printf("SKIP %q (%s): trashed since the crawl", t.name, t.driveID)
			stats.skip()
		case hasParent(f, replica.driveID):
			// Already in the replica — a crash between a previous run's move and
			// its bookkeeping. Record the archival now.
			detailf("OK %s %q (%s): already in the archive replica", t.typ, t.name, t.driveID)
			if merr := markArchived(db, t.driveID, parent, replica.rowID); merr != nil {
				stats.fail("ERROR recording archival of %q (%s): %v", t.name, t.driveID, merr)
				return false
			}
			stats.alreadyThere()
			return true
		default:
			// The recorded parent is stale (the item moved since the crawl);
			// retry from its live parents.
			if merr := rec.moveFile(moveCtx, svc, limiter, t.driveID, replica.driveID, strings.Join(f.Parents, ",")); merr != nil {
				if moveCtx.Err() != nil {
					return false
				}
				stats.fail("ERROR moving %q (%s) into the archive: %v", t.name, t.driveID, merr)
			} else {
				detailf("OK %s %q (%s) -> archive (from live parent %v)", t.typ, t.name, t.driveID, f.Parents)
				if merr := markArchived(db, t.driveID, parent, replica.rowID); merr != nil {
					stats.fail("ERROR recording archival of %q (%s): %v", t.name, t.driveID, merr)
					return false
				}
				stats.move()
				return true
			}
		}
		return false
	}

	// destination pretty-prints where an item would go, for dry-run output.
	destination := func(t archiveTarget) string {
		rel, err := subtreeRelativePath(db, t.parentDriveID.String)
		if err != nil || rel == "" {
			return archiveFolder.Name
		}
		return archiveFolder.Name + "/" + rel + " (as ARCH replicas)"
	}

	// Phase A: files, concurrently. Permission replacement happens per file
	// right after its move, so an interrupted run leaves no moved-but-shared
	// stragglers beyond the items in flight.
	forEachConcurrent(moveCtx, workers, files, func(t archiveTarget) {
		prog.tick("progress: %d/%d file(s) archived", stats.movedCount(), len(files))
		if dryRun {
			log.Printf("WOULD move %s %q (%s) into %q", t.typ, t.name, t.driveID, destination(t))
			if t.canEdit {
				log.Printf("WOULD replace individually-added permissions on %q with %s", t.name, me.EmailAddress)
			}
			stats.move()
			return
		}
		if !archiveOne(t) {
			return
		}
		if t.canEdit {
			replaceDirectPermissions(moveCtx, svc, limiter, rec, t.driveID, t.name, me)
		}
	})
	if ctx.Err() != nil {
		log.Printf("interrupted: %d archived, %d already archived, %d skipped, %d failed", stats.moved, stats.already, stats.skipped, stats.failed)
		return ctx.Err()
	}
	if stats.aborted {
		return stats.err
	}

	// Phase B: folders, sequentially and deepest-first — a folder is archived
	// only once a live check shows it empty, which requires its own contents
	// (deeper in the list) to have been archived first. Folders keep their
	// permissions.
	for _, t := range folders {
		if moveCtx.Err() != nil {
			break
		}
		if dryRun {
			log.Printf("WOULD archive folder %q (%s) into %q, if empty once its contents are archived", t.name, t.driveID, destination(t))
			stats.move()
			continue
		}
		empty, err := folderIsEmpty(moveCtx, svc, limiter, t.driveID)
		if isNotFound(err) {
			log.Printf("SKIP folder %q (%s): no longer exists", t.name, t.driveID)
			stats.skip()
			continue
		}
		if err != nil {
			if moveCtx.Err() != nil {
				break
			}
			stats.fail("ERROR checking whether folder %q (%s) is empty: %v", t.name, t.driveID, err)
			continue
		}
		if !empty {
			log.Printf("SKIP folder %q (%s): not empty on Drive (unarchived or new items inside; note Drive listings can lag just-moved items) — re-run archive later", t.name, t.driveID)
			stats.skip()
			continue
		}
		archiveOne(t)
	}
	if ctx.Err() != nil {
		log.Printf("interrupted: %d archived, %d already archived, %d skipped, %d failed", stats.moved, stats.already, stats.skipped, stats.failed)
		return ctx.Err()
	}
	if stats.aborted {
		return stats.err
	}

	verb := "archived"
	if dryRun {
		verb = "would be archived"
	}
	log.Printf("done: %d item(s) %s (%d already archived), %d skipped, %d failed",
		stats.moved, verb, stats.already, stats.skipped, stats.failed)
	if stats.failed > 0 {
		return fmt.Errorf("%d item(s) failed; re-run archive to retry", stats.failed)
	}
	return nil
}

// replaceDirectPermissions replaces a just-archived file's individually-added
// permissions (users and groups) with the running account's own: the account
// is granted access FIRST — it may so far only have access via a Google Group,
// and revoking that group's permission before holding a direct one would lock
// it out — and only if that grant is in place are the other permissions
// removed. The owner's permission is never removable and domain/anyone grants
// are not "individually-added", so both are left alone. Every failure here is
// a warning, never fatal: the file is already archived, and a re-run cannot
// redo this pass (the file no longer matches the archive query), so we do as
// much as possible.
func replaceDirectPermissions(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, rec *opLog, fileID, name string, me *drive.User) {
	perms, err := listPermissions(ctx, svc, limiter, fileID)
	if err != nil {
		log.Printf("WARN listing permissions on %q (%s): %v; leaving its permissions as they are", name, fileID, err)
		return
	}
	selfID := me.PermissionId
	haveSelf := false
	for _, p := range perms {
		if p.Type == "user" && !p.Deleted && (p.Id == me.PermissionId || strings.EqualFold(p.EmailAddress, me.EmailAddress)) {
			selfID = p.Id
			haveSelf = true
			break
		}
	}
	if !haveSelf {
		if err := rec.grantPermission(ctx, svc, limiter, fileID, me.EmailAddress, archivedPermissionRole); err != nil {
			log.Printf("WARN granting %s access on archived %q (%s): %v; leaving its permissions as they are", me.EmailAddress, name, fileID, err)
			return
		}
	}
	for _, p := range perms {
		if p.Id == selfID || p.Deleted || p.Role == "owner" {
			continue
		}
		if p.Type != "user" && p.Type != "group" {
			continue // domain/anyone grants are not individually-added
		}
		if err := rec.revokePermission(ctx, svc, limiter, fileID, p.Id); err != nil {
			log.Printf("WARN revoking permission of %s on archived %q (%s): %v", p.EmailAddress, name, fileID, err)
		} else {
			detailf("OK revoked %s on archived %q", p.EmailAddress, name)
		}
	}
}
