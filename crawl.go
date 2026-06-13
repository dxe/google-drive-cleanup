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

	listFields = "nextPageToken, files(id, name, mimeType, owners(emailAddress, displayName, permissionId), parents, shortcutDetails(targetId), capabilities(canEdit, canListChildren), " + permissionFields + ")"
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
	db        *sql.DB
	srv       *drive.Service
	limiter   *rate.Limiter
	visited   map[string]bool // Drive IDs of folders already listed this run (cycle guard)
	fileCount int64           // running count of child rows upserted this run
}

var crawlCmd = &cobra.Command{
	Use:   "crawl",
	Short: "Recursively crawl the configured root folder into the database",
	Long: `Crawl (or resume a previous crawl of) the configured root folder into the
database. Ctrl-C stops cleanly between writes; just re-run to resume.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		refresh, _ := cmd.Flags().GetBool("refresh")
		return runCrawl(dbPath, cfgPath, refresh)
	},
}

func init() {
	crawlCmd.Flags().Bool("refresh", false, "reset children_done on all folders to force a full re-crawl")
}

func runCrawl(dbPath, cfgPath string, refresh bool) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if refresh {
		if err := resetChildrenDone(db); err != nil {
			return err
		}
		log.Print("refresh: reset children_done=0 on all folders")
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
		db:      db,
		srv:     srv,
		limiter: rate.NewLimiter(rate.Limit(3), 3), // Drive sustains only a few req/sec
		visited: make(map[string]bool),
	}

	if err := c.validateAndInsertRoot(ctx, cfg.Crawl.Root); err != nil {
		return err
	}

	// The work queue is derived from the db, not just live traversal: every
	// folder with children_done=0 still needs its children listed. Folders
	// discovered mid-crawl are also appended in memory, but since they are
	// inserted with children_done=0 before their parent is marked done, an
	// interrupted run picks them up here on resume.
	queue, err := pendingFolders(db)
	if err != nil {
		return err
	}
	log.Printf("starting crawl: %d folders pending", len(queue))

	var failed, processed int
	for len(queue) > 0 {
		if ctx.Err() != nil {
			break
		}
		f := queue[0]
		queue = queue[1:]
		if c.visited[f.driveID] {
			continue
		}
		c.visited[f.driveID] = true
		detailf("folder %q (%s): listing children [%d folders queued, %d files so far]",
			f.name, f.driveID, len(queue), c.fileCount)
		subs, err := c.listFolder(ctx, f)
		queue = append(queue, subs...)
		processed++
		// Without --verbose the per-folder line above is suppressed; emit a
		// periodic heartbeat so a long crawl still shows it is making progress.
		if !verbose && processed%3 == 0 {
			log.Printf("progress: %d folders listed, %d queued, %d files so far", processed, len(queue), c.fileCount)
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			failed++
			log.Printf("error: folder %q (%s): %v (children_done stays 0; next run retries it)",
				f.name, f.driveID, err)
		}
	}

	remaining, cerr := countPendingFolders(db)
	if cerr != nil {
		return cerr
	}
	switch {
	case ctx.Err() != nil:
		log.Printf("interrupted: %d folders remaining (children_done=0) — re-run `crawl` to resume", remaining)
	case failed > 0:
		return fmt.Errorf("crawl finished with %d folder errors; %d folders remaining — re-run to retry", failed, remaining)
	default:
		log.Printf("crawl complete: %d folders remaining (children_done=0), %d files seen this run", remaining, c.fileCount)
	}
	return nil
}

// validateAndInsertRoot fetches the configured root from Drive, verifies it
// is a folder whose name exactly matches the config (a guard against crawling
// the wrong folder), and upserts it with parent_id = NULL.
func (c *crawler) validateAndInsertRoot(ctx context.Context, cfg rootConfig) error {
	var f *drive.File
	err := c.withRetry(ctx, "files.get "+cfg.ID, func() error {
		var err error
		f, err = c.srv.Files.Get(cfg.ID).
			Fields("id, name, mimeType, owners(emailAddress, displayName, permissionId), capabilities(canEdit, canListChildren), " + permissionFields).
			SupportsAllDrives(true).
			Context(ctx).Do()
		return err
	})
	if err != nil {
		return fmt.Errorf("fetching root folder %s: %w", cfg.ID, err)
	}
	if f.MimeType != folderMimeType {
		return fmt.Errorf("root %s is not a folder (mimeType %q)", cfg.ID, f.MimeType)
	}
	if f.Name != cfg.Name {
		return fmt.Errorf("root name mismatch for %s: config expects %q but Drive has %q — refusing to crawl (is the id pointing at the right folder?)",
			cfg.ID, cfg.Name, f.Name)
	}

	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	email, ownerID, display := ownerOf(f)
	_, _, _, _, err = upsertNode(tx, node{
		driveID:      f.Id,
		name:         f.Name,
		typ:          typeFolder,
		mimeType:     f.MimeType,
		ownerEmail:   email,
		ownerID:      ownerID,
		ownerDisplay: display,
		canEdit:      canEditOf(f),
		// parentID stays NULL: this is the crawl root
	})
	if err != nil {
		return fmt.Errorf("inserting root: %w", err)
	}
	if err := replacePermissions(tx, f.Id, permissionsOf(f)); err != nil {
		return fmt.Errorf("recording root permissions: %w", err)
	}
	return tx.Commit()
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
		subs, err := c.commitPage(f, fl.Files, lastPage)
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
func (c *crawler) commitPage(parent folderRef, files []*drive.File, last bool) ([]folderRef, error) {
	tx, err := c.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var subfolders []folderRef
	for _, file := range files {
		email, ownerID, display := ownerOf(file)
		n := node{
			driveID:      file.Id,
			name:         file.Name,
			typ:          classify(file.MimeType),
			mimeType:     file.MimeType,
			ownerEmail:   email,
			ownerID:      ownerID,
			ownerDisplay: display,
			canEdit:      canEditOf(file),
			// Record the folder we discovered it through, not files.parents[0]:
			// parent_id must always point at a row we actually crawled.
			parentID: sql.NullInt64{Int64: parent.rowID, Valid: true},
		}
		if file.ShortcutDetails != nil {
			n.shortcutTarget = nullString(file.ShortcutDetails.TargetId)
		}
		rowID, existed, prevParent, prevDone, err := upsertNode(tx, n)
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

		// Multi-parent / re-discovery: parent_id keeps the first-discovered
		// parent (the upsert never overwrites a non-NULL one); log every
		// other sighting and persist it to extra_parents for manual review.
		if existed && prevParent.Valid && prevParent.Int64 != parent.rowID {
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
		if n.typ == typeFolder && !prevDone && !c.visited[file.Id] {
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
	c.fileCount += int64(len(files))
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
	const maxAttempts = 8
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
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
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return false
	}
	switch {
	case gerr.Code == 429, gerr.Code >= 500:
		return true
	case gerr.Code == 403:
		for _, e := range gerr.Errors {
			if e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded" {
				return true
			}
		}
	}
	return false
}
