package main

// The delete command permanently deletes items that archive already
// soft-deleted (only rows with a recorded original parent are ever touched).
// Ownership decides the mechanics:
//
//   - items the running account owns are deleted directly;
//   - FILES owned by other INTERNAL accounts (config internal-domains) cannot
//     be deleted directly, but moving them into a shared drive flips ownership
//     to the org — they go through a "Deletion pending" folder inside the
//     dropoff shared drive and are deleted there;
//   - FOLDERS owned by other internal accounts have no such route: Drive moves
//     a file into a shared drive but never a folder, and only an owner may
//     delete their own folder. They are gathered instead — renamed
//     "(deleteme) <name>" and moved into an "(emptied internal folders)" folder
//     directly under the archive root, empty (a folder is only ever touched
//     once nothing is left inside it) — where an IT admin can take ownership of
//     the lot through the Drive web interface and delete them;
//   - EXTERNALLY-owned items are skipped (and flagged delete_skipped) unless
//     --remove-unowned is given, which removes them from their folder — Drive
//     relocates them to their owner's My Drive, a folder renamed "(deleteme) "
//     first — and then drops every direct permission, the running account's own
//     last. No collection folder for these: an admin cannot take over an
//     outsider's folder either.
//
// The snapshot's owner only decides which bucket an item starts in; before
// anything irreversible the live owner decides what actually happens to it, in
// both directions (a "mine" item Drive says is somebody else's, and an
// "external" one Drive says is ours). Drive will not strip the only parent off
// an item we own — it answers 400 badRequest — so an item misfiled as external
// would otherwise fail every run.
//
// A folder — an archived one, or one of the "ARCH " replicas mirroring the
// crawl root's structure, which are pruned afterwards including shells left
// behind by earlier runs — goes once a live listing shows nothing inside it but
// items this run already removed (see remainingContents). A database row is
// removed only once Drive confirms the item is gone — deleted, out of our tree
// (handed to its owner, or gathered for an admin), or reported absent by a live
// read; every other result (an API error, an interrupted run, a skip) keeps the
// row so the next run retries the item. Each handler says which happened by
// returning an itemOutcome, and dropRowIfGone is the one place that acts on it.
// Run pack/unpack before delete to transfer internal ownership to the org and
// shrink the dropoff bucket.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
)

// deletionPendingFolderName is the working folder created inside the dropoff
// shared drive for internally-owned files: moving a file there flips its
// ownership to the org so it becomes deletable. Removed again when empty.
const deletionPendingFolderName = "Deletion pending"

// emptiedFoldersFolderName is the collection folder created directly under the
// archive root for emptied folders owned by other internal accounts. Nothing
// this account can do deletes such a folder — only an owner may delete their
// own, and the shared-drive move that flips a FILE's ownership to the org does
// not exist for folders — so they are gathered here instead, where an IT admin
// can select the lot in the Drive web interface, transfer ownership to
// themselves and delete them in one go. Looked up by name, so a run picks up
// the folder an earlier run (or an admin) left behind rather than making
// another. Unlike deletionPendingFolderName it is deliberately NOT removed when
// it empties: it is the agreed place to look.
const emptiedFoldersFolderName = "(emptied internal folders)"

// deleteMePrefix is put in front of such a folder's name on its way there, and
// in front of an externally-owned folder handed back to its owner under
// --remove-unowned. It says plainly, on the folder itself, that it is finished
// with and there to be deleted — a label that survives the ownership transfer
// and lists the whole set in one search.
const deleteMePrefix = "(deleteme) "

// deleteMeName returns name carrying deleteMePrefix. Idempotent, so a folder an
// earlier run renamed but did not manage to move is not prefixed twice.
func deleteMeName(name string) string {
	if strings.HasPrefix(name, deleteMePrefix) {
		return name
	}
	return deleteMePrefix + name
}

var deleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Permanently delete archived items (see archive)",
	Long: `Permanently delete items the archive command already moved into the archive
tree. Only archived items (with their original parent recorded) that are still
marked delete are touched, and a confirmation with counts is asked first.

Items owned by the running account are deleted directly. FILES owned by other
internal accounts (config internal-domains) are moved into a "Deletion pending"
folder inside the dropoff shared drive — the move flips their ownership to the
org — and deleted there; this requires the Google Workspace privilege "Move any
file or folder into shared drives" (see the README). FOLDERS owned by other
internal accounts cannot take that route, since Drive moves a file into a shared
drive but never a folder, and only their owner may delete them: an emptied one
is renamed "(deleteme) <name>" and moved into an "(emptied internal folders)"
folder directly under the archive root instead, for an IT admin to take
ownership of and delete through the Drive web interface. Externally-owned items are
skipped and counted unless --remove-unowned is given, which removes them from
their archive folder (Drive relocates them to their owner's My Drive, sharing
intact; a folder is renamed "(deleteme) <name>" first) and then drops every
direct permission, the running account's own last.

An archived folder is deleted once a live listing shows nothing inside it but
items this run already removed, and the "ARCH " replica folders that mirror the
crawl root's structure are pruned the same way afterwards — deepest-first, and
including shells left behind by earlier runs. Drive's listings lag deletions, so
anything left behind by an EARLIER run may still be reported for a while; re-run
delete to pick those up. Pass --folder <id> (a folder inside the archive tree,
or a crawled folder whose replica exists) to delete only that subtree.

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

// liveOwnerClass buckets an item the same way classifyOwner does, but from the
// owner Drive reports right now rather than the one the snapshot recorded. An
// item Drive gives no owner for — anything living in a shared drive — comes
// back external, which is where its only caller already had it: none of the
// ownership mechanics here apply to such an item, so it keeps the handling the
// snapshot chose.
func liveOwnerClass(f *drive.File, me *drive.User, internalDomains []string) ownerClass {
	if len(f.Owners) == 0 {
		return ownerExternal
	}
	if ownedByAccount(f, me.EmailAddress) || (me.PermissionId != "" && ownedByAccount(f, me.PermissionId)) {
		return ownerMine
	}
	if isInternalEmail(nullString(f.Owners[0].EmailAddress), internalDomains) {
		return ownerInternal
	}
	return ownerExternal
}

// liveOwnerName names the owner Drive reports for an item, for a log line. An
// item living in a shared drive has no owner at all.
func liveOwnerName(f *drive.File) string {
	if len(f.Owners) == 0 || f.Owners[0].EmailAddress == "" {
		return "(nobody this account can see)"
	}
	return f.Owners[0].EmailAddress
}

// deleteRoute is what delete can actually do with one item, once its owner and
// its type are both known.
type deleteRoute int

const (
	routeDelete   deleteRoute = iota // delete it directly
	routeDropoff                     // via "Deletion pending" in the dropoff shared drive
	routeCollect                     // rename "(deleteme) " and gather it for an admin to take over
	routeExternal                    // skip it, or hand it back to its owner under --remove-unowned
)

// routeFor picks an item's route. It exists for its one asymmetry: a FILE owned
// by another internal account can be deleted after a move into the dropoff
// shared drive flips its ownership to the org, but a FOLDER cannot — the Drive
// API moves files into shared drives and folders never — so an internally-owned
// folder is collected for an admin instead of being routed at a 403.
func routeFor(class ownerClass, typ string) deleteRoute {
	switch class {
	case ownerMine:
		return routeDelete
	case ownerInternal:
		if typ == typeFolder {
			return routeCollect
		}
		return routeDropoff
	}
	return routeExternal
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
	outcomeGone                       // confirmed removed from Drive or from the archive tree, or confirmed already absent
	outcomeSkipped                    // deliberately left alone (dry run, externally owned, re-queued)
)

// goneSet collects the Drive IDs this run confirmed are out of the archive
// tree — deleted, or handed back to their owner. It exists because files.list
// is eventually consistent: a folder emptied seconds ago keeps listing what we
// just removed from it. Discounting exactly the IDs a handler reported
// outcomeGone lets the later phases — the archived-folder gate and the replica
// prune — see such a folder as the empty shell it really is, instead of waiting
// for Drive to catch up over another run. Written from the concurrent phases
// and read by the sequential ones after they have drained; the mutex guards
// that hand-off.
type goneSet struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (g *goneSet) add(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ids[id] = true
}

func (g *goneSet) has(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ids[id]
}

// remainingContents is delete's emptiness check: the live children of folderID
// that this run has not already removed, joined to what the database knows
// about them. An empty result means the folder can go; anything in it is real
// content (a file whose delete failed or was skipped, an externally-owned item
// left in place, an archived folder still awaiting its own delete) and, being
// blockers, explains itself through explainNotEmptyForDelete.
//
// It replaces a plain "does this folder have any child at all" probe, which
// could not tell content apart from Drive's lag: files.list keeps reporting
// items for a while after they are deleted, so a folder this run just emptied
// looked full, and its own deleted children — whose rows are gone by then —
// were reported back as "not in the database, run crawl".
func remainingContents(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, db *sql.DB, folderID string, gone *goneSet) ([]folderBlocker, error) {
	blockers, err := folderBlockers(ctx, svc, limiter, db, folderID)
	if err != nil {
		return nil, err
	}
	left := blockers[:0]
	for _, b := range blockers {
		if !gone.has(b.driveID) {
			left = append(left, b)
		}
	}
	return left, nil
}

// replicaPrune is one archive-tree folder mirroring the crawl root's structure
// that phase 5 offers to remove once a live check shows nothing left inside it.
type replicaPrune struct {
	replicaID  string // the folder on Drive
	originalID string // original folder whose cached pointer to clear; empty when orphaned
	name       string // the original folder's name, or the shell's own
	label      string // how log lines name it
	depth      int    // distance from the tree's root; deepest are pruned first
}

// replicasToPrune lists every replica folder this run may remove, deepest-first
// — a child folder's replica nests inside its parent's, so the stack has to
// collapse from the bottom. Two kinds, pruned identically apart from the
// bookkeeping: replicas an original folder still points at, and the shells left
// behind when that original was itself archived and deleted (see
// orphanedReplicaFolders). Both are measured from their tree's root, so their
// depths sort against each other.
func replicasToPrune(db *sql.DB, archiveRootDriveID string) ([]replicaPrune, error) {
	cached, err := foldersWithReplicas(db)
	if err != nil {
		return nil, err
	}
	orphans, err := orphanedReplicaFolders(db, archiveRootDriveID)
	if err != nil {
		return nil, err
	}
	out := make([]replicaPrune, 0, len(cached)+len(orphans))
	for _, t := range cached {
		out = append(out, replicaPrune{
			replicaID:  t.archiveFolder.String,
			originalID: t.driveID,
			name:       t.name,
			label:      fmt.Sprintf("replica %q of %q", replicaName(t.name), t.name),
			depth:      t.depth,
		})
	}
	for _, t := range orphans {
		out = append(out, replicaPrune{
			replicaID: t.driveID,
			name:      t.name,
			label:     fmt.Sprintf("leftover replica folder %q", t.name),
			depth:     t.depth,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].depth > out[j].depth })
	return out, nil
}

// deleteStats holds a delete run's tallies, updated from the worker pool.
type deleteStats struct {
	*errorBudget
	mu         sync.Mutex
	deleted    int
	viaDropoff int
	removed    int // externally-owned items removed from their folders (--remove-unowned)
	collected  int // internally-owned folders gathered under the archive root for an admin
	skipped    int // externally-owned items skipped (no --remove-unowned)
	notEmpty   int
	pruned     int
}

func (s *deleteStats) del()          { s.mu.Lock(); s.deleted++; s.mu.Unlock() }
func (s *deleteStats) dropoff()      { s.mu.Lock(); s.viaDropoff++; s.mu.Unlock() }
func (s *deleteStats) remove()       { s.mu.Lock(); s.removed++; s.mu.Unlock() }
func (s *deleteStats) collect()      { s.mu.Lock(); s.collected++; s.mu.Unlock() }
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
	// internally-owned files, so a broken config fails before anything deletes.
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
		return fmt.Errorf("dropoff folder %s is not in a shared drive; moving internally-owned files there would not flip their ownership to the org", dropoff.Id)
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
	var mineFolders, collectFolders, externalFolders int
	for _, t := range folders {
		switch routeFor(classifyOwner(t, me, cfg.InternalDomains), t.typ) {
		case routeDelete:
			mineFolders++
		case routeCollect:
			collectFolders++
		default:
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
		"%d archived file(s) and %d archived folder(s): %d owned by %s, %d file(s) owned by internal accounts (via the %q shared drive), %d folder(s) owned by internal accounts (renamed %q and gathered in %q under the archive root for an admin to take over), %d externally owned (%s).",
		len(files), len(folders), len(mineF)+mineFolders, me.EmailAddress, len(internalF), dropoff.Name,
		collectFolders, deleteMePrefix+"…", emptiedFoldersFolderName, len(externalF)+externalFolders, externalNote)
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
	gone := &goneSet{ids: make(map[string]bool)}
	workers := concurrency
	if dryRun {
		workers = 1
	}

	// deletionPending lazily finds or creates the working folder in the dropoff
	// shared drive; created only if an internally-owned file actually needs it.
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

	// emptiedFolders lazily finds or creates the collection folder directly
	// under the archive root, where emptied folders owned by other internal
	// accounts are gathered; created only once one actually needs it. The
	// archive root is re-read from the config here (name checked, as everywhere
	// else) so a mistyped root fails loudly instead of quietly making a
	// collection folder somewhere nobody will look.
	var emptiedFolders *drive.File
	ensureEmptiedFolders := func(ctx context.Context) (*drive.File, error) {
		if emptiedFolders != nil {
			return emptiedFolders, nil
		}
		root, err := getConfiguredFolder(ctx, svc, cfg.Archive.Root, "archive.root")
		if err != nil {
			return nil, err
		}
		f, err := findChildFolder(ctx, svc, limiter, root.Id, emptiedFoldersFolderName)
		if err != nil {
			return nil, fmt.Errorf("looking up %q in %q: %w", emptiedFoldersFolderName, root.Name, err)
		}
		if f == nil {
			if f, err = rec.createFolder(ctx, svc, limiter, root.Id, emptiedFoldersFolderName); err != nil {
				return nil, fmt.Errorf("creating %q in %q: %w", emptiedFoldersFolderName, root.Name, err)
			}
			log.Printf("created %q in %q (%s) for emptied folders owned by other internal accounts", emptiedFoldersFolderName, root.Name, f.Id)
		}
		emptiedFolders = f
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
		gone.add(t.driveID)
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

	// deleteViaDropoff handles an internally-owned FILE: move it into the
	// Deletion pending folder (the move into the shared drive flips ownership
	// to the org; requires the Workspace "Move any file or folder into shared
	// drives" privilege), then delete it there. Runs sequentially. A folder
	// cannot come this way — Drive moves no folder into a shared drive — and is
	// gathered under the archive root instead (see collectFolder).
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

	// queueInternal hands a FILE to phase 3's dropoff queue. Reached from the
	// concurrent phases when Drive's live owner contradicts the snapshot's, so
	// many workers may hit it at once — hence the mutex; phase 3 reads the
	// queue only after those pools have drained.
	var internalMu sync.Mutex
	queueInternal := func(t archiveTarget) itemOutcome {
		internalMu.Lock()
		internalF = append(internalF, t)
		internalMu.Unlock()
		// Re-queued, not dealt with: phase 3 decides its row's fate.
		return outcomeSkipped
	}
	// rerouteInternal is where an item the snapshot called externally owned goes
	// once Drive says an internal account owns it, handed the live state its
	// caller already read. Phases 1-2 deal in files and queue it for phase 3;
	// phase 4 runs after that queue has drained and is sequential, so it is
	// re-pointed there at the handling a folder needs before then.
	rerouteInternal := func(t archiveTarget, _ *drive.File) itemOutcome { return queueInternal(t) }

	// ownerOf names an item's recorded owner for a log line.
	ownerOf := func(t archiveTarget) string {
		if t.ownerEmail.Valid {
			return t.ownerEmail.String
		}
		return "(unknown)"
	}

	// markDeleteMe prefixes a folder that delete cannot delete itself — one
	// bound for the collection folder, or an externally-owned one being handed
	// back to its owner — with "(deleteme) ", reporting whether it may go on.
	//
	// The rename comes first and a failure stops the item, rather than being
	// waved through as cosmetic: the label is the whole reason the pile is
	// tractable to whoever ends up deleting it, and for a folder handed back to
	// an external owner there is no second chance — the access we had usually
	// leaves with the archive folder it was inherited from. A stopped item keeps
	// its row and its place, so the next run retries it; deleteMeName is
	// idempotent, so a rename that did land is not applied twice.
	markDeleteMe := func(ctx context.Context, t archiveTarget, f *drive.File) bool {
		name := deleteMeName(f.Name)
		if name == f.Name {
			return true
		}
		if err := rec.renameFile(ctx, svc, limiter, t.driveID, name); err != nil {
			if ctx.Err() == nil {
				stats.fail("ERROR renaming folder %q (%s) to %q: %v", t.name, t.driveID, name, err)
			}
			return false
		}
		detailf("OK renamed folder %q (%s) to %q", t.name, t.driveID, name)
		return true
	}

	// skipExternal leaves an externally-owned item alone (no --remove-unowned)
	// and flags the row, so a later run — and the review UI — can see why it is
	// still there.
	skipExternal := func(t archiveTarget) itemOutcome {
		if dryRun {
			log.Printf("WOULD skip %s %q (%s): externally owned by %s", t.typ, t.name, t.driveID, ownerOf(t))
		} else {
			detailf("SKIP %q (%s): externally owned by %s", t.name, t.driveID, ownerOf(t))
			if err := markDeleteSkipped(db, t.driveID); err != nil {
				log.Printf("WARN flagging skipped %q (%s): %v", t.name, t.driveID, err)
			}
		}
		stats.skip()
		return outcomeSkipped
	}

	// removeExternal hands an externally-owned item back to its owner, from live
	// state its caller has already read and vetted: a folder renamed first, then
	// the item removed from its folder — Drive relocates it to its owner's My
	// Drive, sharing intact — and then every direct permission we can drop
	// dropped, the running account's own last.
	removeExternal := func(ctx context.Context, t archiveTarget, f *drive.File) itemOutcome {
		if t.typ == typeFolder && !markDeleteMe(ctx, t, f) {
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

	// handleExternal skips (and flags) an externally-owned item, or with
	// --remove-unowned removes it from its folder and unshares it.
	//
	// The recorded owner only decides which items get here; what actually
	// happens to one is decided by the owner Drive reports now. A snapshot can
	// name an owner the item no longer has — or never had: the "(new) <name>"
	// shortcuts reclaim-folders leaves inside a folder it replaced belong to
	// the running account, yet have been crawled as owned by the account owning
	// the folder around them. Removing the only parent of an item we own is not
	// something Drive allows (it answers 400 badRequest, since that would
	// orphan it), and it is not what we want either: an item of ours in the
	// archive is there to be deleted. So a live re-read reroutes it, the mirror
	// of what phase 1 does when the snapshot claims an item we do not own.
	handleExternal := func(ctx context.Context, t archiveTarget) itemOutcome {
		if !removeUnowned {
			return skipExternal(t)
		}
		if dryRun {
			renamed := ""
			if t.typ == typeFolder {
				renamed = fmt.Sprintf(", renamed %q first", deleteMeName(t.name))
			}
			log.Printf("WOULD remove %s %q (%s), externally owned by %s, from its folder%s (it lands in the owner's My Drive) and drop its direct permissions",
				t.typ, t.name, t.driveID, ownerOf(t), renamed)
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
		switch liveOwnerClass(f, me, cfg.InternalDomains) {
		case ownerMine:
			log.Printf("NOTE %q (%s) is owned by this account, not %s (the snapshot was wrong); deleting it instead of removing it from its folder",
				t.name, t.driveID, ownerOf(t))
			return deleteOwned(ctx, t)
		case ownerInternal:
			log.Printf("NOTE %q (%s) is owned by %s, not %s (the snapshot was wrong); handling it as an internally-owned item instead of removing it from its folder",
				t.name, t.driveID, liveOwnerName(f), ownerOf(t))
			t.ownerEmail = nullString(f.Owners[0].EmailAddress)
			return rerouteInternal(t, f)
		}
		return removeExternal(ctx, t, f)
	}

	// collectFolderNow parks one emptied folder in the collection folder under
	// the archive root, from live state its caller has already read and vetted.
	// This is the end of the line for a folder owned by another internal
	// account: we cannot delete it (only its owner can) and we cannot make the
	// org own it (Drive moves no folder into a shared drive), so the most this
	// command can do is put it, labelled, where an IT admin will take ownership
	// of it and delete it. It leaves the archive tree's replica structure, which
	// is what lets the replica around it be pruned, and its row goes with it:
	// what happens to the folder next happens outside this tool.
	collectFolderNow := func(ctx context.Context, t archiveTarget, f *drive.File) itemOutcome {
		dest, err := ensureEmptiedFolders(ctx)
		if err != nil {
			if ctx.Err() == nil {
				stats.fail("ERROR %q (%s): %v", t.name, t.driveID, err)
			}
			return outcomeFailed
		}
		if !markDeleteMe(ctx, t, f) {
			return outcomeFailed
		}
		// Already there from an interrupted run: moving it again would ask Drive
		// to add and remove the same parent at once.
		if !hasParent(f, dest.Id) {
			if err := rec.moveFileVerified(ctx, svc, limiter, t.driveID, dest.Id, strings.Join(f.Parents, ",")); err != nil {
				if ctx.Err() == nil {
					stats.fail("ERROR moving emptied folder %q (%s) into %q: %v", t.name, t.driveID, emptiedFoldersFolderName, err)
				}
				return outcomeFailed
			}
		}
		detailf("OK moved emptied folder %q (%s), owned by %s, into %q as %q", t.name, t.driveID, ownerOf(t), emptiedFoldersFolderName, deleteMeName(f.Name))
		stats.collect()
		return outcomeGone
	}

	// collectFolder is phase 4's entry into that, and re-reads the owner first
	// for the same reason handleExternal does: the snapshot only chose the
	// bucket, and both of the ways it can be wrong need a different route. Ours
	// after all, and the folder is simply deletable; external after all, and
	// --remove-unowned's contract decides, exactly as if the snapshot had said
	// so from the start (the collection folder is for internal owners — an
	// admin has no way to take over an outsider's folder).
	collectFolder := func(ctx context.Context, t archiveTarget) itemOutcome {
		f, err := getFileState(ctx, svc, limiter, t.driveID)
		if isNotFound(err) {
			detailf("OK %q (%s): no longer exists", t.name, t.driveID)
			stats.del()
			return outcomeGone
		}
		if err != nil {
			if ctx.Err() == nil {
				stats.fail("ERROR fetching folder %q (%s): %v", t.name, t.driveID, err)
			}
			return outcomeFailed
		}
		switch liveOwnerClass(f, me, cfg.InternalDomains) {
		case ownerMine:
			log.Printf("NOTE folder %q (%s) is owned by this account, not %s (the snapshot was wrong); deleting it instead of collecting it",
				t.name, t.driveID, ownerOf(t))
			return deleteOwned(ctx, t)
		case ownerExternal:
			log.Printf("NOTE folder %q (%s) is owned by %s, not %s (the snapshot was wrong); treating it as externally owned",
				t.name, t.driveID, liveOwnerName(f), ownerOf(t))
			if len(f.Owners) > 0 {
				t.ownerEmail = nullString(f.Owners[0].EmailAddress)
			}
			if !removeUnowned {
				return skipExternal(t)
			}
			return removeExternal(ctx, t, f)
		}
		return collectFolderNow(ctx, t, f)
	}

	interrupted := func() bool {
		if ctx.Err() == nil {
			return false
		}
		log.Printf("interrupted: %d deleted, %d via %q, %d removed, %d folder(s) collected in %q, %d skipped, %d failed",
			stats.deleted, stats.viaDropoff, deletionPendingFolderName, stats.removed, stats.collected, emptiedFoldersFolderName, stats.skipped, stats.failed)
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
	// owner turns out to have changed are re-routed to the internal queue (see
	// queueInternal), which phase 3 reads once this pool has drained.
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
			return queueInternal(t)
		default:
			stats.fail("ERROR deleting %q (%s): %v", t.name, t.driveID, err)
			return outcomeFailed
		}
	}
	prog1 := newProgress()
	forEachConcurrent(deleteCtx, workers, mineF, func(t archiveTarget) {
		prog1.step("progress: %d/%d owned file(s) deleted", stats.delCount(), len(mineF))
		dropRowIfGone(t, deleteMine(t))
	})
	if err := checkpoint(); err != nil {
		return err
	}

	// Phase 2: externally-owned files, concurrently.
	prog2 := newProgress()
	forEachConcurrent(deleteCtx, workers, externalF, func(t archiveTarget) {
		removed, skipped := stats.externalCounts()
		prog2.step("progress: external file(s): %d removed, %d skipped, of %d", removed, skipped, len(externalF))
		dropRowIfGone(t, handleExternal(deleteCtx, t))
	})
	if err := checkpoint(); err != nil {
		return err
	}

	// Phase 3: internally-owned files, sequentially through Deletion pending.
	prog3 := newProgress()
	for i, t := range internalF {
		if deleteCtx.Err() != nil {
			break
		}
		dropRowIfGone(t, deleteViaDropoff(deleteCtx, t))
		prog3.step("progress: %d/%d internal file(s) via %q", i+1, len(internalF), deletionPendingFolderName)
	}
	if err := checkpoint(); err != nil {
		return err
	}

	// Phase 4: archived folders, sequentially and deepest-first, each gated on
	// a live check that nothing this run did not already delete is left inside
	// (its archived contents went in the phases above). The dropoff queue has
	// drained, so an item that turns out to be internally owned is dealt with
	// here and now — a file through Deletion pending, a folder gathered under
	// the archive root — rather than put onto a queue nobody reads again.
	rerouteInternal = func(t archiveTarget, f *drive.File) itemOutcome {
		if t.typ == typeFolder {
			return collectFolderNow(deleteCtx, t, f)
		}
		return deleteViaDropoff(deleteCtx, t)
	}
	prog4 := newProgress()
	for i, t := range folders {
		if deleteCtx.Err() != nil {
			break
		}
		if dryRun {
			// The handlers below never run in a dry run, so each route says here
			// what it would come to.
			switch routeFor(classifyOwner(t, me, cfg.InternalDomains), t.typ) {
			case routeDelete:
				log.Printf("WOULD delete archived folder %q (%s) once empty", t.name, t.driveID)
				stats.del()
			case routeCollect:
				log.Printf("WOULD rename archived folder %q (%s), owned by %s, to %q once empty and move it into %q under the archive root",
					t.name, t.driveID, ownerOf(t), deleteMeName(t.name), emptiedFoldersFolderName)
				stats.collect()
			default:
				handleExternal(deleteCtx, t) // reports and counts its own dry run
			}
			continue
		}
		// One listing decides both questions: whether anything this run did not
		// already remove is still inside, and — when there is — which items are
		// holding the folder up and whether a plain re-run can clear them (see
		// explainNotEmptyForDelete).
		left, err := remainingContents(deleteCtx, svc, limiter, db, t.driveID, gone)
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
		if len(left) > 0 {
			log.Printf("SKIP folder %q (%s): %s", t.name, t.driveID, explainNotEmptyForDelete(left))
			stats.skipFull()
			continue
		}
		var outcome itemOutcome
		switch routeFor(classifyOwner(t, me, cfg.InternalDomains), t.typ) {
		case routeDelete:
			outcome = deleteOwned(deleteCtx, t)
		case routeCollect:
			outcome = collectFolder(deleteCtx, t)
		default:
			outcome = handleExternal(deleteCtx, t)
		}
		dropRowIfGone(t, outcome)
		prog4.step("progress: %d/%d archived folder(s) processed", i+1, len(folders))
	}
	if err := checkpoint(); err != nil {
		return err
	}

	// Phase 5: prune the replica folders — the "ARCH " shells mirroring the
	// crawl root's structure — that this run emptied out. Deepest-first (a
	// child folder's replica nests inside its parent's) and discounting what we
	// already removed, so a whole stack of nested shells collapses in one run:
	// a pruned replica joins the gone set, which is what lets its parent read
	// as empty on the next iteration even while Drive still lists it.
	//
	// Pruning is safe for archive: it caches these IDs on the original folder
	// (archive_folder_drive_id) and reuses them, so every prune clears that
	// cache and the replica's own row here. Archive re-creates a replica it
	// needs — its resolver verifies a cached ID live and falls through to
	// find-by-name, then create, when the ID is gone (see replicaResolver.ensure).
	prunes, err := replicasToPrune(db, cfg.Archive.Root.ID)
	if err != nil {
		return err
	}
	var inScope map[string]bool
	if subfolder != "" {
		if inScope, err = subtreeDriveIDs(db, subfolder); err != nil {
			return err
		}
	}
	prog5 := newProgress()
	for i, p := range prunes {
		if deleteCtx.Err() != nil {
			break
		}
		if inScope != nil && !inScope[p.replicaID] {
			continue
		}
		if dryRun {
			log.Printf("WOULD prune %s (%s) once empty", p.label, p.replicaID)
			continue
		}
		// Same rule as the item phases: the replica's row and the original's
		// cached pointer to it are only cleared once Drive confirms the replica
		// folder is gone.
		outcome := outcomeFailed
		left, err := remainingContents(deleteCtx, svc, limiter, db, p.replicaID, gone)
		switch {
		case isNotFound(err):
			// Replica already gone; just clear the bookkeeping.
			outcome = outcomeGone
		case err != nil:
			if deleteCtx.Err() == nil {
				log.Printf("WARN checking %s (%s): %v", p.label, p.replicaID, err)
			}
		case len(left) > 0:
			detailf("leaving %s (%s): %s", p.label, p.replicaID, explainNotEmptyForDelete(left))
		default:
			if derr := rec.deleteFile(deleteCtx, svc, limiter, p.replicaID); derr != nil && !isNotFound(derr) {
				if deleteCtx.Err() == nil {
					log.Printf("WARN pruning empty %s (%s): %v", p.label, p.replicaID, derr)
				}
			} else {
				outcome = outcomeGone
			}
		}
		prog5.step("progress: %d/%d replica folder(s) checked", i+1, len(prunes))
		if outcome != outcomeGone {
			continue
		}
		// Its parent's replica may now be an empty shell too, and Drive will
		// keep listing this one inside it for a while yet.
		gone.add(p.replicaID)
		if p.originalID != "" {
			if err := clearArchiveFolder(db, p.originalID); err != nil {
				log.Printf("WARN clearing replica cache of %q (%s): %v", p.name, p.originalID, err)
			}
		}
		if err := deleteNodeRow(db, p.replicaID); err != nil {
			log.Printf("WARN removing database row of pruned replica %s: %v", p.replicaID, err)
		}
		detailf("OK pruned empty %s (%s)", p.label, p.replicaID)
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
	log.Printf("done: %d item(s) %s directly, %d via %q, %d externally-owned removed from their folders, %d internally-owned folder(s) renamed %q and gathered in %q, %d folder(s) not yet empty, %d replica folder(s) pruned, %d failed",
		stats.deleted, verb, stats.viaDropoff, deletionPendingFolderName, stats.removed, stats.collected, deleteMePrefix+"…", emptiedFoldersFolderName, stats.notEmpty, stats.pruned, stats.failed)
	if stats.skipped > 0 {
		fmt.Fprintf(os.Stderr, "%d externally-owned item(s) skipped; re-run with --remove-unowned to remove them from their folders.\n", stats.skipped)
	}
	if stats.failed > 0 {
		return fmt.Errorf("%d item(s) failed; re-run delete to retry", stats.failed)
	}
	return nil
}
