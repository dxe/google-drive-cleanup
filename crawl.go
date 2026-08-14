package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

const (
	folderMimeType   = "application/vnd.google-apps.folder"
	shortcutMimeType = "application/vnd.google-apps.shortcut"
	googleAppsPrefix = "application/vnd.google-apps."

	// permissionFields are the Permission sub-fields we persist for folders so a
	// clone's sharing can be recreated later. Reused by the root fetch and list.
	permissionFields = "permissions(id, type, role, emailAddress, domain, displayName, allowFileDiscovery, deleted)"

	listFields = "nextPageToken, files(id, name, mimeType, modifiedTime, owners(emailAddress, displayName, permissionId), parents, shortcutDetails(targetId), capabilities(canEdit, canListChildren), " + permissionFields + ")"
)

// Node types stored in the nodes.type column. These are the only valid values
// for that column (see the CHECK constraint in the schema).
const (
	typeFolder    = "folder"
	typeShortcut  = "shortcut"
	typeGoogleDoc = "google_doc"
	typeBinary    = "binary"
)

func classify(mimeType string) string {
	switch {
	case mimeType == folderMimeType:
		return typeFolder
	case mimeType == shortcutMimeType:
		return typeShortcut
	case strings.HasPrefix(mimeType, googleAppsPrefix):
		return typeGoogleDoc
	default:
		return typeBinary
	}
}

type crawler struct {
	db          *sql.DB
	srv         *drive.Service
	limiter     *rate.Limiter
	concurrency int // how many folders to list in parallel (see --concurrency)

	// transferAccounts is the set of lower-cased ownership-transfer emails from
	// config. The crawl keeps each node's original_owner_* pointed at the most
	// recent owner that is NOT one of these, so once a file lands on a transfer
	// account its original owner freezes at the last real owner.
	transferAccounts map[string]bool
	// manualTransferAccounts is the lower-cased subset of transferAccounts whose
	// transfers are performed by hand. A file owned by one of these gets its
	// manual_transfer_performed flag set (a manual transfer permanently bumps
	// modifiedTime), and its last_modified is read from the most recent revision
	// rather than modifiedTime, since that most recent change was the ownership
	// transfer — which leaves no revision — rather than a content edit.
	manualTransferAccounts map[string]bool

	// archiveRoot is config archive.root when filled in. A configured archive
	// tree is crawled as a second root after the crawl root finishes, so
	// archived files stay in the snapshot (and packable); the review UI and
	// keep-recent exclude the tree from decision marking instead.
	archiveRoot rootConfig

	// visited guards against listing a folder twice (a cycle or a second parent).
	// The concurrent drain does a test-and-set on it from many workers, so it and
	// its mutex must be used together (see markVisited/isVisited).
	visitedMu sync.Mutex
	visited   map[string]bool

	fileCount atomic.Int64 // running count of child rows upserted this run
}

// markVisited records driveID as listed and reports whether this call is the one
// that claimed it — the first worker to reach a folder gets true and lists it;
// any later sighting (a cycle or a second parent) gets false and skips.
func (c *crawler) markVisited(driveID string) bool {
	c.visitedMu.Lock()
	defer c.visitedMu.Unlock()
	if c.visited[driveID] {
		return false
	}
	c.visited[driveID] = true
	return true
}

// isVisited reports whether driveID has already been claimed for listing.
func (c *crawler) isVisited(driveID string) bool {
	c.visitedMu.Lock()
	defer c.visitedMu.Unlock()
	return c.visited[driveID]
}

// unvisitedInQueue counts the distinct folders in queue that still need
// listing, i.e. excluding entries that duplicate an already-visited folder (a
// folder reachable via multiple parents is enqueued once per sighting). This is
// the real work remaining, unlike len(queue), which also counts stale
// duplicates that get discarded as no-ops the moment a worker pops them — the
// source of the queue "collapsing" to 0 in one step near the end of a crawl.
func (c *crawler) unvisitedInQueue(queue []folderRef) int {
	c.visitedMu.Lock()
	defer c.visitedMu.Unlock()
	seen := make(map[string]bool, len(queue))
	n := 0
	for _, f := range queue {
		if c.visited[f.driveID] || seen[f.driveID] {
			continue
		}
		seen[f.driveID] = true
		n++
	}
	return n
}

var crawlCmd = &cobra.Command{
	Use:   "crawl",
	Short: "Recursively crawl the configured root folder into the database",
	Long: `Crawl (or resume a previous crawl of) the configured root folder into the
database. Ctrl-C stops cleanly between writes; just re-run to resume.

--refresh re-lists every folder but keeps the existing snapshot rows; --wipe
deletes the previous snapshot outright (as also happens automatically when the
configured root changes) and crawls from scratch.

--folder re-indexes only the subtree rooted at the given Drive folder ID,
pruning stale rows under it alone. The folder must already exist in the snapshot
(which guarantees it is a descendant of the crawl root).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		refresh, _ := cmd.Flags().GetBool("refresh")
		wipe, _ := cmd.Flags().GetBool("wipe")
		subfolder, _ := cmd.Flags().GetString("folder")
		concurrency, _ := cmd.Flags().GetInt("concurrency")
		return runCrawl(dbPath, cfgPath, refresh, wipe, subfolder, concurrency)
	},
}

// defaultCrawlConcurrency is how many folders the crawl lists in parallel by
// default. Each files.list carries hundreds of ms of latency, so a single worker
// only reaches a few folders per second regardless of the rate limit; a handful
// of workers keeps enough requests in flight to reach the shared limiter's
// ceiling. The limiter (not the worker count) is the quota safety cap, so this
// stays modest.
const defaultCrawlConcurrency = 8

func init() {
	crawlCmd.Flags().Bool("refresh", false, "reset children_done on all folders to force a full re-crawl")
	crawlCmd.Flags().Bool("wipe", false, "discard the previous crawl snapshot entirely and crawl from scratch")
	crawlCmd.Flags().String("folder", "", "Drive folder ID (already crawled) to re-index in place, pruning stale rows under that subfolder only")
	crawlCmd.Flags().Int("concurrency", defaultCrawlConcurrency, "how many folders to list in parallel (all still share the global rate limiter)")
}

func runCrawl(dbPath, cfgPath string, refresh, wipe bool, subfolder string, concurrency int) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// A scoped re-index (--folder) touches only one subtree, so the whole-tree
	// session flags do not apply to it.
	if subfolder != "" && (refresh || wipe) {
		return fmt.Errorf("--folder cannot be combined with --refresh or --wipe")
	}

	// Cancel the context on the first SIGINT/SIGTERM so the crawl stops
	// cleanly between API pages/folders; transactions in flight always run to
	// completion because they don't use this context. A second signal kills
	// the process the default way.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received %s — finishing current write, then exiting (signal again to force quit)", sig)
		signal.Stop(sigCh)
		cancel()
	}()

	srv, err := newDriveService(ctx, drive.DriveReadonlyScope)
	if err != nil {
		return err
	}

	c := &crawler{
		db:  db,
		srv: srv,
		// Folders are listed concurrently (see --concurrency), but every call still
		// passes through this one shared limiter, so it — not the worker count — is
		// the quota safety cap. 20/sec is far under Drive's per-user ceiling
		// (~12k/min); backoff-on-429/403 self-throttles if we ever overshoot.
		limiter:                rate.NewLimiter(rate.Limit(20), 20),
		concurrency:            concurrency,
		visited:                make(map[string]bool),
		transferAccounts:       transferAccountSet(cfg.Migration.OwnershipTransferAccounts),
		manualTransferAccounts: transferAccountSet(cfg.Migration.ManualOwnershipTransferAccounts),
	}
	if cfg.Archive.Root.configured() {
		if cfg.Archive.Root.ID == cfg.Crawl.Root.ID {
			return fmt.Errorf("archive.root.id equals crawl.root.id (%s); the archive folder must be a separate folder outside the crawl root", cfg.Crawl.Root.ID)
		}
		c.archiveRoot = cfg.Archive.Root
	}

	log.Printf("recording last-content-change times in last_modified (%d manual ownership-transfer account(s) configured)", len(c.manualTransferAccounts))

	// --folder re-indexes one already-crawled subtree in place; it shares the
	// Drive client and signal handling above but none of the whole-tree session
	// bookkeeping below.
	if subfolder != "" {
		return c.runScopedCrawl(ctx, subfolder)
	}

	if refresh {
		if err := resetChildrenDone(db, ""); err != nil {
			return err
		}
		log.Print("refresh: reset children_done=0 on all folders")
	}

	// Establish the crawl session. sessionStart is the cutoff for the stale-row
	// sweep run once the crawl completes: any node not re-observed since then no
	// longer exists under the root and is removed. It is persisted so an
	// interrupted crawl resumes the same session rather than resetting the
	// cutoff (which would delete rows written before the interruption). A fresh
	// database, a --refresh, or a change of the configured root each begin a new
	// session; a plain resume keeps the recorded one.
	storedRoot, sessionStart, haveMeta, err := getCrawlMeta(db)
	if err != nil {
		return err
	}
	if wipe {
		log.Print("--wipe: discarding the previous crawl snapshot")
		if err := wipeCrawlSnapshot(db); err != nil {
			return err
		}
		haveMeta = false
	}
	if haveMeta && storedRoot != cfg.Crawl.Root.ID {
		log.Printf("crawl root changed since the last crawl (%s -> %s); discarding the previous snapshot",
			storedRoot, cfg.Crawl.Root.ID)
		if err := wipeCrawlSnapshot(db); err != nil {
			return err
		}
		haveMeta = false
	}
	// A resume continues the recorded session over the existing snapshot;
	// anything that starts a new session (fresh db, --wipe, --refresh, root
	// change) is a new crawl.
	resuming := haveMeta && !refresh
	if !resuming {
		sessionStart = now()
		if err := setCrawlMeta(db, cfg.Crawl.Root.ID, sessionStart); err != nil {
			return err
		}
	}

	if err := c.validateAndInsertRoot(ctx, cfg.Crawl.Root); err != nil {
		return err
	}

	// The work queue is derived from the db, not just live traversal: every
	// folder with children_done=0 still needs its children listed. Folders
	// discovered mid-crawl are also appended in memory, but since they are
	// inserted with children_done=0 before their parent is marked done, an
	// interrupted run picks them up here on resume. With an archive tree
	// configured this first phase is scoped to the crawl root's subtree; the
	// archive phase below then drains everything still pending (the archive
	// tree plus any detached stragglers).
	crawlScope := ""
	if c.archiveRoot.configured() {
		crawlScope = cfg.Crawl.Root.ID
	}
	queue, err := pendingFolders(db, crawlScope)
	if err != nil {
		return err
	}
	if resuming {
		log.Printf("resuming previous crawl: %d folders pending", len(queue))
	} else {
		log.Printf("starting new crawl: %d folders pending", len(queue))
	}

	failed := c.drainQueue(ctx, queue)

	// The archive tree is crawled after the crawl root so archived files stay
	// in the snapshot: their owners keep refreshing (pack needs that for
	// ownership transfer ahead of a delete) and stale-row pruning sees them
	// re-observed. Skipped cleanly on interruption; a re-run picks it up.
	if c.archiveRoot.configured() && ctx.Err() == nil {
		if err := c.validateAndInsertRoot(ctx, c.archiveRoot); err != nil {
			return fmt.Errorf("archive tree: %w", err)
		}
		archiveQueue, err := pendingFolders(db, "")
		if err != nil {
			return err
		}
		log.Printf("crawling archive tree %q (%s): %d folders pending", c.archiveRoot.Name, c.archiveRoot.ID, len(archiveQueue))
		failed += c.drainQueue(ctx, archiveQueue)
	}

	remaining, cerr := countPendingFolders(db, "")
	if cerr != nil {
		return cerr
	}
	switch {
	case ctx.Err() != nil:
		log.Printf("interrupted: %d folders remaining (children_done=0) — re-run `crawl` to resume", remaining)
	case failed > 0:
		return fmt.Errorf("crawl finished with %d folder errors; %d folders remaining — re-run to retry", failed, remaining)
	default:
		// The crawl finished cleanly, so every live node was re-observed this
		// session: drop the rows that were not (deleted from Drive since the
		// previous crawl) so the snapshot reflects the tree as it is now.
		if remaining == 0 {
			removed, err := deleteStaleNodes(db, sessionStart)
			if err != nil {
				return fmt.Errorf("removing stale rows from the previous crawl: %w", err)
			}
			if removed > 0 {
				log.Printf("pruned %d node(s) no longer under the root (backed up to pruned_nodes)", removed)
			}
		}
		log.Printf("crawl complete: %d folders remaining (children_done=0), %d files seen this run", remaining, c.fileCount.Load())
	}
	return nil
}

// runScopedCrawl re-indexes only the subtree rooted at subfolder, an already
// crawled folder, and prunes stale rows under it alone. Requiring the folder to
// already exist in the snapshot guarantees it is a descendant of the crawl root.
// The whole-tree session bookkeeping (crawl_meta, wipe/root-change handling) is
// deliberately left untouched: a local cutoff timestamp scopes the stale sweep
// to this subtree.
func (c *crawler) runScopedCrawl(ctx context.Context, subfolder string) error {
	typ, err := nodeTypeByDriveID(c.db, subfolder)
	if err == sql.ErrNoRows {
		return fmt.Errorf("subfolder %s not found in the database; it must be a folder already crawled under the crawl root", subfolder)
	}
	if err != nil {
		return err
	}
	if typ != typeFolder {
		return fmt.Errorf("subfolder %s is a %s, not a folder", subfolder, typ)
	}
	rel, err := subtreeRelativePath(c.db, subfolder)
	if err != nil {
		return err
	}
	if rel == "" {
		// The subfolder is the crawl root itself; name it for the log line.
		ref, err := folderRefByDriveID(c.db, subfolder)
		if err != nil {
			return err
		}
		rel = ref.name
	}

	// Capture the cutoff before re-observing anything: every node re-observed
	// this run gets a fresh crawled_at (>= cutoff), so descendants still bearing
	// an older timestamp afterwards no longer exist in Drive.
	cutoff := now()

	// Re-fetch and upsert the subfolder itself. Unlike its descendants it is
	// never re-observed through a parent listing (its parent is outside the
	// scope), so without this its crawled_at would stay stale and the prune
	// below would delete the very folder being re-indexed — the same reason a
	// full crawl re-inserts the root up front. A 404 here means the folder was
	// removed from Drive; report it rather than silently pruning the subtree.
	f, err := c.fetchFolder(ctx, subfolder)
	if err != nil {
		return fmt.Errorf("fetching subfolder %s: %w", subfolder, err)
	}
	if err := c.upsertFolder(ctx, f); err != nil {
		return err
	}

	// resetChildrenDone on the subtree forces the walk to re-list every folder
	// under it rather than treat already-done folders as complete.
	if err := resetChildrenDone(c.db, subfolder); err != nil {
		return err
	}
	log.Printf("re-indexing subfolder %q (%s)", rel, subfolder)

	// Seed the queue with every folder in the subtree (all now children_done=0),
	// not just the subfolder: listing each directly means a folder deleted from
	// Drive still gets a (now empty) listing, is marked done, and can be pruned.
	queue, err := pendingFolders(c.db, subfolder)
	if err != nil {
		return err
	}
	failed := c.drainQueue(ctx, queue)

	remaining, cerr := countPendingFolders(c.db, subfolder)
	if cerr != nil {
		return cerr
	}
	switch {
	case ctx.Err() != nil:
		log.Printf("interrupted: %d folders remaining (children_done=0) under %q — re-run `crawl --folder %s` to resume", remaining, rel, subfolder)
	case failed > 0:
		return fmt.Errorf("re-index finished with %d folder errors; %d folders remaining under %q — re-run to retry", failed, remaining, rel)
	default:
		if remaining == 0 {
			removed, err := deleteStaleNodesUnder(c.db, subfolder, cutoff)
			if err != nil {
				return fmt.Errorf("removing stale rows under %q: %w", rel, err)
			}
			if removed > 0 {
				log.Printf("pruned %d node(s) no longer under %q (backed up to pruned_nodes)", removed, rel)
			}
		}
		log.Printf("re-index complete: %d folders remaining (children_done=0) under %q, %d files seen this run", remaining, rel, c.fileCount.Load())
	}
	return nil
}

// drainQueue lists folders breadth-first until the queue empties or ctx is
// cancelled. Up to c.concurrency workers share one queue: each lists a folder,
// then pushes the subfolders it discovers back onto the queue for any worker to
// pick up. The crawl is done when the queue is empty and no worker is still
// listing. It returns the number of folders whose listing failed (their
// children_done stays 0 so a later run retries them).
//
// visited (test-and-set via markVisited) guarantees each folder is listed once
// even when reached through multiple parents, and fileCount is atomic; database
// writes serialize on the single SQLite connection (see openDB). Listing happens
// off-lock, so the mutex is only ever held for cheap bookkeeping — never for I/O.
func (c *crawler) drainQueue(ctx context.Context, seed []folderRef) int {
	workers := c.concurrency
	if workers < 1 {
		workers = 1
	}
	var (
		mu        sync.Mutex
		cond      = sync.NewCond(&mu)
		queue     = append([]folderRef(nil), seed...)
		active    int // workers currently listing a folder (may still add to queue)
		failed    int
		processed int
	)

	worker := func() {
		for {
			mu.Lock()
			// Wait for work while some other worker might still enqueue more. Once
			// the queue is empty and nobody is listing, the crawl is done; a
			// cancelled context ends it too.
			for len(queue) == 0 && active > 0 && ctx.Err() == nil {
				cond.Wait()
			}
			if ctx.Err() != nil || (len(queue) == 0 && active == 0) {
				mu.Unlock()
				cond.Broadcast() // let the other idle workers observe the same end state
				return
			}
			f := queue[0]
			queue = queue[1:]
			active++
			queued := len(queue)
			mu.Unlock()

			// A later sighting of an already-claimed folder (cycle / second parent)
			// is a no-op; release the active slot and let the loop re-evaluate.
			if !c.markVisited(f.driveID) {
				mu.Lock()
				active--
				mu.Unlock()
				cond.Broadcast()
				continue
			}

			detailf("folder %q (%s): listing children [%d folders queued, %d files so far]",
				f.name, f.driveID, queued, c.fileCount.Load())
			subs, err := c.listFolder(ctx, f)

			mu.Lock()
			active--
			queue = append(queue, subs...)
			processed++
			// Without --verbose the per-folder line above is suppressed; emit a
			// periodic heartbeat so a long crawl still shows it is making progress.
			if !verbose && processed%3 == 0 {
				log.Printf("progress: %d folders listed, %d queued, %d files so far", processed, c.unvisitedInQueue(queue), c.fileCount.Load())
			}
			// A cancelled listing returns ctx.Err(); that is an interruption, not a
			// folder error, so don't count it (children_done stays 0 regardless).
			if err != nil && ctx.Err() == nil {
				failed++
				log.Printf("error: folder %q (%s): %v (children_done stays 0; next run retries it)",
					f.name, f.driveID, err)
			}
			mu.Unlock()
			cond.Broadcast()
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); worker() }()
	}
	wg.Wait()
	return failed
}

// fetchFolder gets a single folder from Drive with the fields we persist, and
// verifies it is still a folder.
func (c *crawler) fetchFolder(ctx context.Context, id string) (*drive.File, error) {
	var f *drive.File
	err := c.withRetry(ctx, "files.get "+id, func() error {
		var err error
		f, err = c.srv.Files.Get(id).
			Fields("id, name, mimeType, modifiedTime, owners(emailAddress, displayName, permissionId), capabilities(canEdit, canListChildren), " + permissionFields).
			SupportsAllDrives(true).
			Context(ctx).Do()
		return err
	})
	if err != nil {
		return nil, err
	}
	if f.MimeType != folderMimeType {
		return nil, fmt.Errorf("%s is not a folder (mimeType %q)", id, f.MimeType)
	}
	return f, nil
}

// upsertFolder records a folder fetched directly (not via a parent listing) and
// its permissions, refreshing its crawled_at. It passes setParent=false so the
// upsert preserves whatever parent the row already has (NULL for the crawl root;
// the existing parent for a scoped re-index root, whose real parent is outside
// the scope and so never re-observed through a listing this run).
func (c *crawler) upsertFolder(ctx context.Context, f *drive.File) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	email, ownerID, display := ownerOf(f)
	if _, _, _, _, err := upsertNode(tx, node{
		driveID:               f.Id,
		name:                  f.Name,
		typ:                   typeFolder,
		mimeType:              f.MimeType,
		ownerEmail:            email,
		ownerID:               ownerID,
		ownerDisplay:          display,
		canEdit:               canEditOf(f),
		// Folders have no revisions, so the manual-transfer flag is irrelevant here.
		lastModified:          nullString(c.lastModifiedFor(ctx, f, false)),
		ownerIsTransfer:       c.emailIsTransferAccount(email),
		ownerIsManualTransfer: c.emailIsManualTransferAccount(email),
	}, false); err != nil {
		return fmt.Errorf("inserting %s: %w", f.Id, err)
	}
	if err := replacePermissions(tx, f.Id, permissionsOf(f)); err != nil {
		return fmt.Errorf("recording permissions for %s: %w", f.Id, err)
	}
	return tx.Commit()
}

// validateAndInsertRoot fetches the configured root from Drive, verifies it
// is a folder whose name exactly matches the config (a guard against crawling
// the wrong folder), and upserts it with parent_id = NULL.
func (c *crawler) validateAndInsertRoot(ctx context.Context, cfg rootConfig) error {
	f, err := c.fetchFolder(ctx, cfg.ID)
	if err != nil {
		return fmt.Errorf("fetching root folder %s: %w", cfg.ID, err)
	}
	if f.Name != cfg.Name {
		return fmt.Errorf("root name mismatch for %s: config expects %q but Drive has %q — refusing to crawl (is the id pointing at the right folder?)",
			cfg.ID, cfg.Name, f.Name)
	}
	return c.upsertFolder(ctx, f)
}

// listFolder lists every child of f, committing each page of children in one
// transaction. The transaction for the final page (no nextPageToken) also
// flips f's children_done to 1, so the database never shows a folder as done
// while any of its child rows are missing. Returns newly discovered
// subfolders that still need listing.
func (c *crawler) listFolder(ctx context.Context, f folderRef) ([]folderRef, error) {
	var subfolders []folderRef
	pageToken := ""
	for {
		if err := ctx.Err(); err != nil {
			return subfolders, err // interrupted between pages; children_done stays 0
		}
		var fl *drive.FileList
		err := c.withRetry(ctx, "files.list "+f.driveID, func() error {
			call := c.srv.Files.List().
				Q(fmt.Sprintf("'%s' in parents and trashed = false", f.driveID)).
				PageSize(1000).
				Fields(listFields).
				SupportsAllDrives(true).
				IncludeItemsFromAllDrives(true).
				Context(ctx)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			var err error
			fl, err = call.Do()
			return err
		})
		if err != nil {
			return subfolders, err
		}
		lastPage := fl.NextPageToken == ""
		subs, err := c.commitPage(ctx, f, fl.Files, lastPage)
		subfolders = append(subfolders, subs...)
		if err != nil {
			return subfolders, err
		}
		if lastPage {
			return subfolders, nil
		}
		pageToken = fl.NextPageToken
	}
}

// commitPage upserts one page of children of parent in a single transaction;
// when last is true the same transaction marks the parent children_done=1.
func (c *crawler) commitPage(ctx context.Context, parent folderRef, files []*drive.File, last bool) ([]folderRef, error) {
	// Resolve each file's last-content-change time before opening the
	// transaction: for manually-transferred files this makes Revisions API calls,
	// and network I/O must never happen while holding the single SQLite write
	// connection. A file counts as manually transferred when its persisted
	// manual_transfer_performed flag is already set (from a past crawl, even if it
	// has since moved to a non-manual account) or its current owner is a manual-
	// ownership-transfer account — the same value the flag takes after this crawl's
	// MAX(...) upsert below.
	driveIDs := make([]string, len(files))
	for i, file := range files {
		driveIDs[i] = file.Id
	}
	existingManual, err := manualTransferPerformedByDriveID(c.db, driveIDs)
	if err != nil {
		return nil, fmt.Errorf("reading manual_transfer_performed flags: %w", err)
	}
	lastMod := make(map[string]string, len(files))
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err // interrupted; children_done stays 0 so a rerun retries
		}
		email, _, _ := ownerOf(file)
		manualTransfer := existingManual[file.Id] || c.emailIsManualTransferAccount(email)
		lastMod[file.Id] = c.lastModifiedFor(ctx, file, manualTransfer)
	}

	tx, err := c.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var subfolders []folderRef
	for _, file := range files {
		// The archive folder is supposed to live outside the crawl root (inside
		// it, the archive inherits the crawl root's sharing, and the archive
		// command refuses to run). It is still crawled either way — as a child
		// here or as the second root — so just flag the misplacement.
		if c.archiveRoot.configured() && file.Id == c.archiveRoot.ID {
			log.Printf("WARN archive root %q (%s) found inside the crawl root (under %q); move it outside — the archive command will refuse to run until it is",
				file.Name, file.Id, parent.name)
		}
		email, ownerID, display := ownerOf(file)
		n := node{
			driveID:               file.Id,
			name:                  file.Name,
			typ:                   classify(file.MimeType),
			mimeType:              file.MimeType,
			ownerEmail:            email,
			ownerID:               ownerID,
			ownerDisplay:          display,
			canEdit:               canEditOf(file),
			lastModified:          nullString(lastMod[file.Id]),
			ownerIsTransfer:       c.emailIsTransferAccount(email),
			ownerIsManualTransfer: c.emailIsManualTransferAccount(email),
			// Record the folder we discovered it through, not files.parents[0]:
			// parent_id must always point at a row we actually crawled.
			parentID: sql.NullInt64{Int64: parent.rowID, Valid: true},
		}
		if file.ShortcutDetails != nil {
			n.shortcutTarget = nullString(file.ShortcutDetails.TargetId)
		}
		// A node Drive reports under a single parent is definitively a child of
		// the folder we just listed it under, so reparent it there (setParent):
		// a node that moved between crawls follows the move. A multi-parent node
		// instead keeps its first-discovered parent, and the extras are recorded
		// below.
		rowID, existed, prevParent, prevDone, err := upsertNode(tx, n, len(file.Parents) <= 1)
		if err != nil {
			return nil, fmt.Errorf("upserting %s (%q): %w", file.Id, file.Name, err)
		}

		// Record folder sharing so it can be recreated on a clone later. Only
		// folders are tracked; files inherit their clone folder's permissions.
		if n.typ == typeFolder {
			if err := replacePermissions(tx, file.Id, permissionsOf(file)); err != nil {
				return nil, fmt.Errorf("recording permissions for %s (%q): %w", file.Id, file.Name, err)
			}
		}

		// Multi-parent bookkeeping: a node Drive reports under more than one
		// parent keeps its first-discovered parent in parent_id; log every other
		// sighting and persist it to extra_parents for manual review. A
		// single-parent node reached via a folder other than its stored parent
		// has simply moved and was already reparented above — nothing to record.
		if len(file.Parents) > 1 && existed && prevParent.Valid && prevParent.Int64 != parent.rowID {
			log.Printf("warning: %s (%q) reached again via folder %q (%s); keeping first-discovered parent (row %d), recording extra parent",
				file.Id, file.Name, parent.name, parent.driveID, prevParent.Int64)
			if err := recordExtraParent(tx, file.Id, parent.driveID); err != nil {
				return nil, err
			}
		}
		if len(file.Parents) > 1 {
			known, err := knownDriveIDs(tx, file.Parents)
			if err != nil {
				return nil, err
			}
			log.Printf("warning: multi-parent node %s (%q): parents [%s] (already in crawl db: %s)",
				file.Id, file.Name, strings.Join(file.Parents, ", "), joinKnown(file.Parents, known))
			for _, p := range file.Parents {
				if p != parent.driveID && known[p] {
					if err := recordExtraParent(tx, file.Id, p); err != nil {
						return nil, err
					}
				}
			}
		}

		// Recurse into folders only — shortcuts are recorded, never followed.
		// Skip folders already fully listed (resume) or already visited this
		// run (cycle / second parent).
		if n.typ == typeFolder && !prevDone && !c.isVisited(file.Id) {
			subfolders = append(subfolders, folderRef{rowID: rowID, driveID: file.Id, name: file.Name})
		}
	}
	if last {
		if err := markChildrenDone(tx, parent.rowID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	c.fileCount.Add(int64(len(files)))
	return subfolders, nil
}

func ownerOf(f *drive.File) (email, ownerID, display sql.NullString) {
	if len(f.Owners) == 0 || f.Owners[0] == nil {
		return // shared-drive items have no owner; the API may also omit it
	}
	o := f.Owners[0]
	return nullString(o.EmailAddress), nullString(o.PermissionId), nullString(o.DisplayName)
}

// canEditOf reports whether the crawling account can edit f. When the
// capabilities object is absent the account has no usable access, so this
// returns false. Edit access on a folder implies the ability to list its
// children, so an editable folder whose children cannot be listed is an
// impossible state we refuse to record and panic on instead.
func canEditOf(f *drive.File) bool {
	if f.Capabilities == nil {
		return false
	}
	canEdit := f.Capabilities.CanEdit
	if canEdit && f.MimeType == folderMimeType && !f.Capabilities.CanListChildren {
		panic(fmt.Sprintf("folder %q (%s) is editable but its children cannot be listed", f.Name, f.Id))
	}
	return canEdit
}

// permissionsOf maps the drive.Permission entries on f to our permission model.
// The list is empty for items whose permissions the crawling account cannot
// read; we store what Drive returns.
func permissionsOf(f *drive.File) []permission {
	perms := make([]permission, 0, len(f.Permissions))
	for _, p := range f.Permissions {
		if p == nil {
			continue
		}
		perms = append(perms, permission{
			permissionID:       p.Id,
			typ:                p.Type,
			role:               p.Role,
			emailAddress:       nullString(p.EmailAddress),
			domain:             nullString(p.Domain),
			displayName:        nullString(p.DisplayName),
			allowFileDiscovery: sql.NullBool{Bool: p.AllowFileDiscovery, Valid: p.Type == "domain" || p.Type == "anyone"},
			deleted:            p.Deleted,
		})
	}
	return perms
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// transferAccountSet lower-cases the configured ownership-transfer emails into a
// lookup set (email comparison is case-insensitive). Returns an empty, non-nil
// set when none are configured.
func transferAccountSet(emails []string) map[string]bool {
	set := make(map[string]bool, len(emails))
	for _, e := range emails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			set[e] = true
		}
	}
	return set
}

// emailIsTransferAccount reports whether email is one of the configured
// ownership-transfer accounts (case-insensitive). An invalid/empty email is
// never a transfer account.
func (c *crawler) emailIsTransferAccount(email sql.NullString) bool {
	return email.Valid && c.transferAccounts[strings.ToLower(email.String)]
}

// emailIsManualTransferAccount reports whether email is one of the configured
// manual-ownership-transfer accounts (case-insensitive).
func (c *crawler) emailIsManualTransferAccount(email sql.NullString) bool {
	return email.Valid && c.manualTransferAccounts[strings.ToLower(email.String)]
}

// lastModifiedFor returns the value to store in nodes.last_modified for f.
// Normally this is Drive's top-level modifiedTime, but when manualTransferPerformed
// is set that modifiedTime is unreliable: a manual ownership transfer permanently
// bumped it even though the transfer was not a content edit. Ownership transfers
// do not create a Drive revision, so the most recent revision is the last real
// content edit — we consult the Revisions API and prefer it. manualTransferPerformed
// reflects the persisted manual_transfer_performed flag (set once a file is seen
// under a manual-ownership-transfer account and never cleared, since the bump is
// permanent) OR'd with the current owner being such an account, so it does not
// depend on who owns the file now. It falls back to modifiedTime when the file
// has no revisions or its history cannot be read (logged, not fatal — a single
// unreadable file must not stall a crawl).
func (c *crawler) lastModifiedFor(ctx context.Context, f *drive.File, manualTransferPerformed bool) string {
	// Revisions exist only for actual files; folders and shortcuts have none, so
	// don't waste an API call on them.
	if typ := classify(f.MimeType); manualTransferPerformed && (typ == typeGoogleDoc || typ == typeBinary) {
		t, err := c.lastRevisionTime(ctx, f.Id)
		switch {
		case err == nil && t != "":
			return t
		case err != nil && ctx.Err() == nil:
			log.Printf("warning: %s (%q): could not read revision history (%v); using modifiedTime %s",
				f.Id, f.Name, err, f.ModifiedTime)
		}
	}
	return f.ModifiedTime
}

// lastRevisionTime returns the modifiedTime of the file's most recent revision,
// or "" when it has no revisions. A revision is created only by a content change
// (a new upload for binary files, an autosaved or named version for Google-
// native files); metadata-only events such as an ownership transfer bump the
// top-level modifiedTime without adding a revision. The newest revision is
// therefore the last real content edit, whereas modifiedTime may reflect a
// later transfer.
func (c *crawler) lastRevisionTime(ctx context.Context, fileID string) (string, error) {
	var revisions []*drive.Revision
	pageToken := ""
	for {
		var list *drive.RevisionList
		err := c.withRetry(ctx, "revisions.list "+fileID, func() error {
			call := c.srv.Revisions.List(fileID).
				Fields("nextPageToken, revisions(id, modifiedTime)").
				PageSize(1000).
				Context(ctx)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			var err error
			list, err = call.Do()
			return err
		})
		if err != nil {
			return "", err
		}
		revisions = append(revisions, list.Revisions...)
		if list.NextPageToken == "" {
			break
		}
		pageToken = list.NextPageToken
	}
	if len(revisions) == 0 {
		return "", nil
	}
	// Drive returns revisions oldest-first, so the last entry is the most recent
	// content edit — the value we want, since the ownership transfer that bumped
	// modifiedTime did not itself create a revision.
	return revisions[len(revisions)-1].ModifiedTime, nil
}

func joinKnown(ids []string, known map[string]bool) string {
	var in []string
	for _, id := range ids {
		if known[id] {
			in = append(in, id)
		}
	}
	if len(in) == 0 {
		return "none"
	}
	return strings.Join(in, ", ")
}

// withRetry runs fn through the rate limiter, retrying with exponential
// backoff plus jitter on 403 rateLimitExceeded, 429 and 5xx responses.
// Retries go back through the limiter too — they never bypass it.
func (c *crawler) withRetry(ctx context.Context, op string, fn func() error) error {
	return withRetry(ctx, c.limiter, op, fn)
}

// withRetry runs fn through the given rate limiter, retrying with exponential
// backoff plus jitter on the errors retryable reports transient. Retries go
// back through the limiter too — they never bypass it. Used by the crawl and by
// the pack/unpack write operations alike.
func withRetry(ctx context.Context, limiter *rate.Limiter, op string, fn func() error) error {
	const maxAttempts = 8
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		if err := limiter.Wait(ctx); err != nil {
			return err
		}
		err := fn()
		if err == nil {
			return nil
		}
		if !retryable(err) || attempt == maxAttempts {
			return err
		}
		sleep := backoff/2 + rand.N(backoff) // jitter in [backoff/2, 1.5*backoff)
		log.Printf("%s: attempt %d/%d failed (%v); retrying in %s", op, attempt, maxAttempts, err, sleep.Round(time.Millisecond))
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func retryable(err error) bool {
	// A move that reported success but did not attach the requested parent:
	// confirmParent has re-issued it and wants withRetry to back off and verify
	// again. See confirmParent.
	if errors.Is(err, errParentNotConfirmed) {
		return true
	}
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return false
	}
	switch {
	case gerr.Code == 429, gerr.Code >= 500:
		return true
	case gerr.Code == 409:
		// Transient conflict. Seen on unpack's revokePermission: removing the
		// migrating user's Manager (organizer) membership from the dropoff shared
		// drive right after a burst of writes on it — reparenting every restored
		// item out of the drive, then deleting the Container and Stash. Drive
		// reconciles shared-drive ACLs/membership asynchronously, so the member
		// removal collides with that in-flight backend churn. Google's guidance
		// for 409 on Drive is to retry with backoff; a genuine, non-transient
		// conflict just exhausts the retries and surfaces as before.
		return true
	case gerr.Code == 403:
		for _, e := range gerr.Errors {
			switch e.Reason {
			case "rateLimitExceeded", "userRateLimitExceeded":
				return true
			case "fileWriterTeamDriveMoveInDisabled":
				// Raised by unpack when moving an item into a destination that is
				// (still) inside a shared drive. Right after the Container restore
				// moves a folder subtree out of the shared drive, Drive's backend is
				// eventually consistent: for a short window it still treats the just-
				// moved destination as residing in the shared drive and rejects the
				// move-in. Retrying lets that settle. If the destination is genuinely
				// still in the shared drive (e.g. its owned-root ancestor failed to
				// restore), the retries exhaust and it fails as before.
				return true
			}
		}
	}
	return false
}
