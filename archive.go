package main

// The archive command soft-deletes everything marked delete in the review UI:
// files move into a configured archive folder (config archive.root) whose
// inside mirrors the crawl root's folder structure as "ARCH "-prefixed replica
// folders, so an archived file remains findable by its original location and
// name. Individually-added permissions on archived files are replaced with the
// running account's own, so archived content stops being shared. A file owned by
// another INTERNAL account first takes a detour through an "Archival pending"
// folder in the dropoff shared drive, which transfers its ownership to the org
// so the original owner's access becomes removable too. Delete-marked folders
// that are empty on Drive (their contents archived or gone) are archived too,
// descendants before ancestors. The restore command reverses one item; the
// delete command permanently deletes archived items.

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

// archivalPendingFolderName is the working folder created inside the dropoff
// shared drive for internally-owned files: moving a file there transfers its
// ownership to the org, so the previous owner's access becomes an ordinary
// grant instead of un-revokable ownership. Removed again when empty, like the
// delete command's "Deletion pending".
const archivalPendingFolderName = "Archival pending"

// replicaName returns the archive replica folder name for an original folder
// name: prefixed and rune-safely truncated.
func replicaName(original string) string {
	return prefixedReplicaName(archReplicaPrefix, original)
}

// prefixedReplicaName prefixes an original folder's name and truncates the
// result to maxReplicaNameRunes without splitting a rune. Shared by the archive
// tree's "ARCH " replicas and the externals tree's "(ext) " ones, so a long name
// is cut the same way in both.
func prefixedReplicaName(prefix, original string) string {
	r := []rune(prefix + original)
	if len(r) > maxReplicaNameRunes {
		r = r[:maxReplicaNameRunes]
	}
	return string(r)
}

// folderBlocker is one live child that keeps a delete-marked folder from
// emptying, paired with what the database knows about it.
type folderBlocker struct {
	name     string
	driveID  string
	mimeType string
	known    bool   // the database has a row for this child
	decision string // that row's decision: delete, keep, or undecided
	archived bool   // already moved into the archive tree
	skipped  bool   // delete_skipped: a delete run left it alone (externally owned)
}

// folderBlockers lists what Drive still reports inside folderID and joins each
// child to its database row, so a folder that will not empty can say which
// items are holding it up. Shared by archive's and delete's emptiness gates.
func folderBlockers(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, db *sql.DB, folderID string) ([]folderBlocker, error) {
	children, err := listChildren(ctx, svc, limiter, folderID, "nextPageToken, files(id, name, mimeType)")
	if err != nil {
		return nil, err
	}
	out := make([]folderBlocker, 0, len(children))
	for _, c := range children {
		b := folderBlocker{name: c.Name, driveID: c.Id, mimeType: c.MimeType}
		var (
			orig    sql.NullString
			skipped int
		)
		err := db.QueryRow(`SELECT decision, original_parent_drive_id, delete_skipped FROM nodes WHERE drive_id = ?`, c.Id).
			Scan(&b.decision, &orig, &skipped)
		switch err {
		case nil:
			b.known, b.archived, b.skipped = true, orig.Valid, skipped != 0
		case sql.ErrNoRows:
			// Not crawled yet; b.known stays false.
		default:
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// blockerNames renders up to maxNamed blockers as `"name" (id)`, with a count
// of the rest, for a single log line.
func blockerNames(blockers []folderBlocker) string {
	const maxNamed = 3
	named := blockers
	if len(named) > maxNamed {
		named = named[:maxNamed]
	}
	parts := make([]string, 0, len(named))
	for _, b := range named {
		parts = append(parts, fmt.Sprintf("%q (%s)", b.name, b.driveID))
	}
	s := strings.Join(parts, ", ")
	if rest := len(blockers) - len(named); rest > 0 {
		s += fmt.Sprintf(", and %d more", rest)
	}
	return s
}

// explainNotEmpty turns a non-empty folder's live contents into one actionable
// sentence. The causes need different responses, and the generic advice to
// re-run only clears the first:
//
//   - children marked delete that are not archived yet: transient. They failed
//     earlier this run, or Drive's listing lags a move that already happened, so
//     a re-run really does pick them up.
//   - children marked keep, or still undecided: permanent. Archive only moves
//     what is marked delete, so the folder can never empty on its own and no
//     number of re-runs changes that — the items have to be re-decided in
//     review, or the folder left as it is.
//   - children the database has never seen: added since the last crawl, so they
//     have no decision at all yet; crawl, then decide them.
//
// Telling someone to "re-run archive later" when every blocker is permanent
// sends them round a loop that cannot finish, which is the case this exists to
// call out.
// stuck is the set of folder Drive IDs this run already reported as permanently
// blocked. Phase C walks deepest-first, so a child folder's verdict is always
// known before its parent's — and a parent whose only blocker is such a child
// is just as stuck, however the child itself is marked. Without that chaining a
// parent would be told to "re-run archive", which is exactly the dead end this
// function exists to stop reporting.
//
// The second return value says whether the folder is permanently blocked, so
// the caller can add it to stuck for the ancestors still to come.
func explainNotEmpty(blockers []folderBlocker, stuck map[string]bool) (string, bool) {
	if len(blockers) == 0 {
		// The emptiness probe said "not empty" and this listing found nothing:
		// Drive's listing is eventually consistent, so it is the lag case.
		return "not empty on Drive, but a second listing found no children (Drive's listing lags recent moves) — re-run archive later", false
	}
	var pending, blocked, undecided, kept, unknown []folderBlocker
	for _, b := range blockers {
		switch {
		case !b.known:
			unknown = append(unknown, b)
		case stuck[b.driveID]:
			blocked = append(blocked, b)
		case b.decision == decisionDelete:
			pending = append(pending, b)
		case b.decision == decisionKeep:
			kept = append(kept, b)
		default:
			undecided = append(undecided, b)
		}
	}
	var counts []string
	if len(pending) > 0 {
		counts = append(counts, fmt.Sprintf("%d marked delete but not archived yet", len(pending)))
	}
	if len(blocked) > 0 {
		counts = append(counts, fmt.Sprintf("%d subfolder(s) this run could not empty either", len(blocked)))
	}
	if len(kept) > 0 {
		counts = append(counts, fmt.Sprintf("%d marked keep", len(kept)))
	}
	if len(undecided) > 0 {
		counts = append(counts, fmt.Sprintf("%d undecided", len(undecided)))
	}
	if len(unknown) > 0 {
		counts = append(counts, fmt.Sprintf("%d not in the database", len(unknown)))
	}

	msg := fmt.Sprintf("not empty on Drive: %d item(s) inside (%s) — %s",
		len(blockers), strings.Join(counts, ", "), blockerNames(blockers))
	if len(blocked)+len(kept)+len(undecided)+len(unknown) == 0 {
		return msg + "; re-run archive to pick them up (Drive listings can also lag a just-moved item)", false
	}

	var fix []string
	if len(kept)+len(undecided) > 0 {
		fix = append(fix, fmt.Sprintf("mark %s delete in `drive-cleanup review` (or leave this folder unarchived)",
			blockerNames(append(append([]folderBlocker{}, kept...), undecided...))))
	}
	if len(unknown) > 0 {
		fix = append(fix, fmt.Sprintf("run `drive-cleanup crawl` so %s get a decision", blockerNames(unknown)))
	}
	if len(blocked) > 0 {
		fix = append(fix, fmt.Sprintf("clear what is blocking %s first (reported above)", blockerNames(blocked)))
	}
	return msg + ". Re-running archive cannot empty this folder — archive only moves items marked delete. To finish it: " +
		strings.Join(fix, "; "), true
}

// explainNotEmptyForDelete is explainNotEmpty's counterpart for the delete
// command, whose blockers mean something different. Inside an archive folder
// the items that will not go are the ones delete already declined:
// externally-owned items it skipped (which only --remove-unowned clears), and
// anything that is not marked delete at all. Callers pass what
// remainingContents kept, so the list is never empty — items this run deleted
// but Drive still lists are gone from it before it gets here.
func explainNotEmptyForDelete(blockers []folderBlocker) string {
	var pending, skipped, other, unknown []folderBlocker
	for _, b := range blockers {
		switch {
		case !b.known:
			unknown = append(unknown, b)
		case b.decision != decisionDelete:
			other = append(other, b)
		case b.skipped:
			skipped = append(skipped, b)
		default:
			pending = append(pending, b)
		}
	}
	var counts []string
	if len(pending) > 0 {
		counts = append(counts, fmt.Sprintf("%d marked delete but not deleted yet", len(pending)))
	}
	if len(skipped) > 0 {
		counts = append(counts, fmt.Sprintf("%d skipped as externally owned", len(skipped)))
	}
	if len(other) > 0 {
		counts = append(counts, fmt.Sprintf("%d not marked delete", len(other)))
	}
	if len(unknown) > 0 {
		counts = append(counts, fmt.Sprintf("%d not in the database", len(unknown)))
	}

	msg := fmt.Sprintf("not empty on Drive: %d item(s) inside (%s) — %s",
		len(blockers), strings.Join(counts, ", "), blockerNames(blockers))
	if len(skipped)+len(other)+len(unknown) == 0 {
		return msg + "; re-run delete to pick them up (Drive listings can also lag a just-deleted item)"
	}

	var fix []string
	if len(skipped) > 0 {
		fix = append(fix, fmt.Sprintf("re-run with --remove-unowned to hand %s back to their owners", blockerNames(skipped)))
	}
	if len(other) > 0 {
		fix = append(fix, fmt.Sprintf("mark %s delete in `drive-cleanup review` and re-run archive", blockerNames(other)))
	}
	if len(unknown) > 0 {
		fix = append(fix, fmt.Sprintf("run `drive-cleanup crawl` so %s get a decision", blockerNames(unknown)))
	}
	return msg + ". Re-running delete on its own cannot empty this folder. To finish it: " + strings.Join(fix, "; ")
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

A file owned by another internal account (config internal-domains) is first
moved into an "Archival pending" folder inside the dropoff shared drive
(migration.dropoff-folder), which transfers its ownership to the org; once Drive
has applied that transfer — it is asynchronous, so archive polls until it lands
(and gives up on a file only after the whole batch has stopped progressing) — the
file moves on into the archive, owned by the running account, and its previous
owner's access is revoked with the rest of its individual permissions. The
folder is removed again when it ends up empty. This needs the Google Workspace
privilege "Move any file or folder into shared drives" and manager access on
the dropoff shared drive (see the README). Externally-owned files are archived
as before: their ownership cannot be taken over, so they keep it.

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
	// handed counts internally-owned files handed to the org by moving them into
	// the archival-pending folder, i.e. the ownership transfers this run started.
	handed int
}

func (s *archiveStats) move()         { s.mu.Lock(); s.moved++; s.mu.Unlock() }
func (s *archiveStats) alreadyThere() { s.mu.Lock(); s.already++; s.mu.Unlock() }
func (s *archiveStats) skip()         { s.mu.Lock(); s.skipped++; s.mu.Unlock() }
func (s *archiveStats) hand()         { s.mu.Lock(); s.handed++; s.mu.Unlock() }

func (s *archiveStats) movedCount() int  { s.mu.Lock(); defer s.mu.Unlock(); return s.moved }
func (s *archiveStats) handedCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.handed }

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
	// The dropoff folder is only needed for internally-owned files, but it is
	// validated eagerly — as in delete — so a broken config fails before
	// anything moves.
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

	// Internally-owned files are archived by way of the dropoff shared drive (it
	// is what makes their ownership transferable, see phase B), so it is resolved
	// and checked up front even when this run turns out to have none.
	dropoff, err := getConfiguredFolder(ctx, svc, cfg.Migration.DropoffFolder, "migration.dropoff-folder")
	if err != nil {
		return err
	}
	if dropoff.DriveId == "" {
		return fmt.Errorf("dropoff folder %s is not in a shared drive; moving internally-owned files there would not transfer their ownership to the org", dropoff.Id)
	}

	// Files owned by another account in the org take the ownership-transfer
	// detour; the rest (this account's own, and third parties' — whose ownership
	// cannot be taken over) move straight into the archive.
	var directF, internalF []archiveTarget
	for _, t := range files {
		if classifyOwner(t, me, cfg.InternalDomains) == ownerInternal {
			internalF = append(internalF, t)
		} else {
			directF = append(directF, t)
		}
	}

	// All Drive calls below share this one limiter; it — not the worker count —
	// is the quota safety cap (see pack).
	limiter := rate.NewLimiter(rate.Limit(20), 20)

	scopeNote := ""
	if subfolder != "" {
		scopeNote = fmt.Sprintf(" under subfolder %q", subfolderPath)
	}
	// internalNote spells out the ownership-transfer detour when this run has
	// files that need it.
	internalNote := ""
	if len(internalF) > 0 {
		internalNote = fmt.Sprintf("\n%d of those file(s) are owned by other internal accounts: each goes through %q in the dropoff shared drive %q first, so its ownership transfers to the org and it reaches the archive owned by %s.",
			len(internalF), archivalPendingFolderName, dropoff.Name, me.EmailAddress)
	}
	if dryRun {
		fmt.Fprintf(os.Stderr, "DRY RUN: no changes will be made. Would archive %d file(s) and %d empty folder(s) marked delete%s into %q.%s\n",
			len(files), len(folders), scopeNote, archiveFolder.Name, internalNote)
	} else {
		fmt.Fprintf(os.Stderr, "About to archive %d file(s) and up to %d folder(s) marked delete%s into %q (%s), replacing archived files' individually-added permissions.%s\n",
			len(files), len(folders), scopeNote, archiveFolder.Name, archiveFolder.Id, internalNote)
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

	// archivalPending lazily finds or creates the working folder in the dropoff
	// shared drive, verifying up front that the account can move items back out
	// of that drive again — without manager access every file handed to the org
	// would strand in there. Created only if an internally-owned file needs it.
	var archivalPending *drive.File
	ensureArchivalPending := func(ctx context.Context) (*drive.File, error) {
		if archivalPending != nil {
			return archivalPending, nil
		}
		f, err := findChildFolder(ctx, svc, limiter, dropoff.Id, archivalPendingFolderName)
		if err != nil {
			return nil, fmt.Errorf("looking up %q in %q: %w", archivalPendingFolderName, dropoff.Name, err)
		}
		if f == nil {
			if f, err = rec.createFolder(ctx, svc, limiter, dropoff.Id, archivalPendingFolderName); err != nil {
				return nil, fmt.Errorf("creating %q in %q: %w", archivalPendingFolderName, dropoff.Name, err)
			}
		}
		state, err := svc.Files.Get(f.Id).
			Fields("id, capabilities(canMoveItemOutOfDrive)").
			SupportsAllDrives(true).Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("fetching %q (%s): %w", archivalPendingFolderName, f.Id, err)
		}
		if state.Capabilities != nil && !state.Capabilities.CanMoveItemOutOfDrive {
			return nil, fmt.Errorf("%s cannot move items out of the shared drive %s holding %q; it needs manager access there to archive internally-owned files",
				me.EmailAddress, dropoff.DriveId, dropoff.Name)
		}
		archivalPending = f
		return f, nil
	}

	// archiveOne moves one item into its parent's replica and records the
	// archival. Moves are optimistic (one files.update, removing fromParent —
	// normally the recorded parent, the archival-pending folder for a file coming
	// back out of the shared drive); a failure is diagnosed from the item's live
	// state, pack-style. Reports whether the item ended up archived (moved or
	// already there).
	archiveOne := func(t archiveTarget, fromParent string, outOfSharedDrive bool) bool {
		parent := t.parentDriveID.String
		replica, ok := resolver.verified[parent]
		if !ok { // resolved up front; missing means the pre-phase was interrupted
			return false
		}
		// A move out of the shared drive is parent-verified: that is the one move
		// Drive has been seen to report as successful while silently dropping the
		// item at the mover's My Drive root (see moveFileVerified).
		move := rec.moveFile
		if outOfSharedDrive {
			move = rec.moveFileVerified
		}
		err := move(moveCtx, svc, limiter, t.driveID, replica.driveID, fromParent)
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
			if merr := move(moveCtx, svc, limiter, t.driveID, replica.driveID, strings.Join(f.Parents, ",")); merr != nil {
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

	// interrupted reports the tallies of a run cut short by Ctrl-C.
	interrupted := func() {
		log.Printf("interrupted: %d archived, %d already archived, %d handed to the org, %d skipped, %d failed",
			stats.moved, stats.already, stats.handed, stats.skipped, stats.failed)
	}

	// Phase A: files this account owns, plus externally-owned ones whose
	// ownership cannot be taken over — straight into the archive, concurrently.
	// Permission replacement happens per file right after its move, so an
	// interrupted run leaves no moved-but-shared stragglers beyond the items in
	// flight.
	forEachConcurrent(moveCtx, workers, directF, func(t archiveTarget) {
		prog.tick("progress: %d/%d file(s) archived", stats.movedCount(), len(directF))
		if dryRun {
			log.Printf("WOULD move %s %q (%s) into %q", t.typ, t.name, t.driveID, destination(t))
			if t.canEdit {
				log.Printf("WOULD replace individually-added permissions on %q with %s", t.name, me.EmailAddress)
			}
			stats.move()
			return
		}
		if !archiveOne(t, t.parentDriveID.String, false) {
			return
		}
		if t.canEdit {
			replaceDirectPermissions(moveCtx, svc, limiter, rec, t.driveID, t.name, me)
		}
	})
	if ctx.Err() != nil {
		interrupted()
		return ctx.Err()
	}
	if stats.aborted {
		return stats.err
	}

	// Phase B: files owned by other internal accounts. Such an owner cannot be
	// unshared — an owner permission is not revokable — so the file detours
	// through the archival-pending folder in the dropoff shared drive: the move
	// in transfers ownership to the org, the move back out into the archive makes
	// the running account the owner, and the previous owner is left holding an
	// ordinary grant that replaceDirectPermissions revokes with the rest.
	if len(internalF) > 0 && moveCtx.Err() == nil {
		if dryRun {
			for _, t := range internalF {
				log.Printf("WOULD move %s %q (%s), owned by %s, into %q in the dropoff shared drive %q to transfer its ownership to the org, wait for the transfer, then move it into %q owned by %s",
					t.typ, t.name, t.driveID, t.ownerEmail.String, archivalPendingFolderName, dropoff.Name, destination(t), me.EmailAddress)
				log.Printf("WOULD replace individually-added permissions on %q with %s", t.name, me.EmailAddress)
				stats.hand()
				stats.move()
			}
		} else if pending, perr := ensureArchivalPending(moveCtx); perr != nil {
			stats.fail("ERROR preparing %q: %v", archivalPendingFolderName, perr)
		} else {
			// B1: hand each file to the org by moving it into the pending folder.
			// handed collects what got there and still needs its transfer to land;
			// ready collects files an interrupted run already carried past it.
			var listMu sync.Mutex
			var handed, ready []archiveTarget
			addHanded := func(t archiveTarget) {
				listMu.Lock()
				handed = append(handed, t)
				listMu.Unlock()
				stats.hand()
			}
			addReady := func(t archiveTarget) { listMu.Lock(); ready = append(ready, t); listMu.Unlock() }
			forEachConcurrent(moveCtx, workers, internalF, func(t archiveTarget) {
				prog.tick("progress: %d/%d internally-owned file(s) handed to the org", stats.handedCount(), len(internalF))
				err := rec.moveFileVerified(moveCtx, svc, limiter, t.driveID, pending.Id, t.parentDriveID.String)
				if err == nil {
					detailf("OK %q (%s) -> %q (ownership transferring to the org)", t.name, t.driveID, archivalPendingFolderName)
					addHanded(t)
					return
				}
				if moveCtx.Err() != nil {
					return
				}
				f, gerr := getFileState(moveCtx, svc, limiter, t.driveID)
				if moveCtx.Err() != nil {
					return
				}
				replica, haveReplica := resolver.verified[t.parentDriveID.String]
				switch {
				case isNotFound(gerr):
					log.Printf("SKIP %q (%s): no longer exists", t.name, t.driveID)
					stats.skip()
				case gerr != nil:
					stats.fail("ERROR %q (%s): move into %q failed (%v) and live lookup failed (%v)", t.name, t.driveID, archivalPendingFolderName, err, gerr)
				case f.Trashed:
					log.Printf("SKIP %q (%s): trashed since the crawl", t.name, t.driveID)
					stats.skip()
				case hasParent(f, pending.Id):
					// A previous run already handed it over; wait for its transfer.
					detailf("OK %q (%s): already in %q", t.name, t.driveID, archivalPendingFolderName)
					addHanded(t)
				case haveReplica && hasParent(f, replica.driveID):
					// A previous run got it all the way into the archive but was cut
					// short before recording it; B3 finishes the bookkeeping.
					addReady(t)
				default:
					// The recorded parent is stale (the file moved since the crawl);
					// retry from its live parents.
					if merr := rec.moveFileVerified(moveCtx, svc, limiter, t.driveID, pending.Id, strings.Join(f.Parents, ",")); merr != nil {
						if moveCtx.Err() == nil {
							stats.fail("ERROR moving %q (%s) into %q: %v (this needs the Workspace privilege \"Move any file or folder into shared drives\")",
								t.name, t.driveID, archivalPendingFolderName, merr)
						}
						return
					}
					detailf("OK %q (%s) -> %q (from live parent %v)", t.name, t.driveID, archivalPendingFolderName, f.Parents)
					addHanded(t)
				}
			})

			// B2: wait for Drive to apply the ownership transfer, which it does
			// asynchronously and file by file — the same lag unpack waits out after
			// a Container drag. Moving a file back out too early would return it to
			// My Drive still owned by its original owner, whose access could then
			// not be removed. A file the shared drive holds reports no owner at all,
			// so one listing of the pending folder answers this for the whole batch
			// (thousands of files at a time, far too many to ask about one by one).
			// The listing can lag, but only toward the OLD owner — it cannot report
			// a transfer that has not happened — so a file it reports as ownerless
			// really is transferred; a file it does not mention yet is asked about
			// directly, which is also how one that vanished gets noticed.
			//
			// The timeout is a no-progress window, not a total: a large batch can
			// legitimately take many rounds to trickle through, and each round that
			// transfers something is a reason to keep waiting.
			for lastProgress := time.Now(); len(handed) > 0 && moveCtx.Err() == nil; {
				children, lerr := listChildren(moveCtx, svc, limiter, pending.Id, "nextPageToken, files(id, owners(emailAddress))")
				if lerr != nil && moveCtx.Err() == nil {
					log.Printf("WARN listing %q: %v; checking each file individually instead", archivalPendingFolderName, lerr)
				}
				stillOwnedByID := make(map[string]bool, len(children))
				for _, c := range children {
					stillOwnedByID[c.Id] = len(c.Owners) > 0
				}
				var stillOwned []archiveTarget
				progressed := 0
				for _, t := range handed {
					if moveCtx.Err() != nil {
						stillOwned = append(stillOwned, t)
						continue
					}
					if hasOwner, listed := stillOwnedByID[t.driveID]; listed {
						if hasOwner {
							stillOwned = append(stillOwned, t)
						} else {
							ready = append(ready, t)
							progressed++
						}
						continue
					}
					// Not in the listing yet; ask about the file itself.
					f, gerr := getFileState(moveCtx, svc, limiter, t.driveID)
					switch {
					case moveCtx.Err() != nil:
						stillOwned = append(stillOwned, t)
					case isNotFound(gerr):
						log.Printf("SKIP %q (%s): no longer exists", t.name, t.driveID)
						stats.skip()
					case gerr != nil:
						stats.fail("ERROR checking the ownership transfer of %q (%s): %v", t.name, t.driveID, gerr)
					case f.Trashed:
						log.Printf("SKIP %q (%s): trashed since it was moved into %q", t.name, t.driveID, archivalPendingFolderName)
						stats.skip()
					case len(f.Owners) == 0:
						ready = append(ready, t)
						progressed++
					default:
						stillOwned = append(stillOwned, t)
					}
				}
				handed = stillOwned
				if len(handed) == 0 || moveCtx.Err() != nil {
					break
				}
				if progressed > 0 {
					lastProgress = time.Now()
				} else if time.Since(lastProgress) > flipWaitTimeout {
					break
				}
				log.Printf("%d file(s) in %q are still owned by their original owner; the ownership transfer is still propagating — rechecking in %v",
					len(handed), archivalPendingFolderName, flipPollInterval)
				select {
				case <-moveCtx.Done():
				case <-time.After(flipPollInterval):
				}
			}

			// B3: move each transferred file out of the shared drive into its
			// archive replica — that move makes the running account the owner — and
			// unshare it. The snapshot's owner columns follow the transfer; the
			// original_owner_* columns stay frozen at what the crawl discovered.
			base := stats.movedCount()
			forEachConcurrent(moveCtx, workers, ready, func(t archiveTarget) {
				prog.tick("progress: %d/%d transferred file(s) archived", stats.movedCount()-base, len(ready))
				if !archiveOne(t, pending.Id, true) {
					return
				}
				if err := setNodeOwner(db, t.driveID, me.EmailAddress, me.PermissionId, me.DisplayName); err != nil {
					log.Printf("WARN recording %s as the new owner of %q (%s): %v", me.EmailAddress, t.name, t.driveID, err)
				}
				// The account owns the file now, so its permissions are ours to
				// replace whatever the crawl recorded in can_edit.
				replaceDirectPermissions(moveCtx, svc, limiter, rec, t.driveID, t.name, me)
			})

			// Whatever never transferred stays in the pending folder; a re-run finds
			// it there and carries on (its row is still unarchived).
			for _, t := range handed {
				stats.fail("ERROR %q (%s) is still owned by %s after %v without progress in %q; its ownership transfer has not landed and the file is stranded there — re-run archive to finish it",
					t.name, t.driveID, t.ownerEmail.String, flipWaitTimeout, archivalPendingFolderName)
			}
		}
	}
	if ctx.Err() != nil {
		interrupted()
		return ctx.Err()
	}
	if stats.aborted {
		return stats.err
	}

	// Phase C: folders, sequentially and deepest-first — a folder is archived
	// only once a live check shows it empty, which requires its own contents
	// (deeper in the list) to have been archived first. Folders keep their
	// permissions.
	// Folders this run reported as permanently blocked, so an ancestor whose
	// only blocker is one of them is told the same rather than to re-run.
	stuckFolders := map[string]bool{}
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
			// Say which children are holding it up and whether a re-run can
			// actually clear them; "not empty, try again later" is a loop with
			// no exit when the blockers are items nothing will ever move.
			blockers, berr := folderBlockers(moveCtx, svc, limiter, db, t.driveID)
			if berr != nil {
				if moveCtx.Err() != nil {
					break
				}
				log.Printf("SKIP folder %q (%s): not empty on Drive, and listing its contents to say why failed: %v", t.name, t.driveID, berr)
			} else {
				why, permanent := explainNotEmpty(blockers, stuckFolders)
				log.Printf("SKIP folder %q (%s): %s", t.name, t.driveID, why)
				if permanent {
					stuckFolders[t.driveID] = true
				}
			}
			stats.skip()
			continue
		}
		archiveOne(t, t.parentDriveID.String, false)
	}
	if ctx.Err() != nil {
		interrupted()
		return ctx.Err()
	}
	if stats.aborted {
		return stats.err
	}

	// Remove the archival-pending folder again once it was used and a live
	// listing confirms nothing is stranded in it. Skipped when the run aborted
	// above: then something probably is.
	if archivalPending != nil && moveCtx.Err() == nil {
		if empty, err := folderIsEmpty(moveCtx, svc, limiter, archivalPending.Id); err != nil {
			log.Printf("WARN checking %q: %v", archivalPendingFolderName, err)
		} else if !empty {
			log.Printf("%q is not empty (stranded files, or Drive's listing lags recent moves); leaving it", archivalPendingFolderName)
		} else if err := rec.deleteFile(moveCtx, svc, limiter, archivalPending.Id); err != nil {
			log.Printf("WARN removing empty %q folder: %v", archivalPendingFolderName, err)
		} else {
			detailf("OK removed empty %q folder", archivalPendingFolderName)
		}
	}

	verb := "archived"
	if dryRun {
		verb = "would be archived"
	}
	log.Printf("done: %d item(s) %s (%d already archived, %d via %q for ownership transfer), %d skipped, %d failed",
		stats.moved, verb, stats.already, stats.handed, archivalPendingFolderName, stats.skipped, stats.failed)
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
