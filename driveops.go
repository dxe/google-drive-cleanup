package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

// errorBudget bounds how many item failures a concurrent move phase tolerates
// before giving up. fail logs one failure and, once more than maxErrors items
// have failed, records an abort error and cancels the shared context so
// in-flight workers stop and later phases short-circuit — a burst of failures
// means something systemic (wrong account, wrong config, revoked access), not
// per-item drift. fail is safe to call from many worker goroutines; the summary
// fields (aborted, err, failed) may be read directly once the workers have
// drained (a WaitGroup provides the happens-before).
type errorBudget struct {
	mu        sync.Mutex
	cmd       string // "pack" or "unpack", named in the abort message
	maxErrors int
	cancel    context.CancelFunc
	failed    int
	aborted   bool
	err       error
}

func (b *errorBudget) fail(format string, args ...any) {
	log.Printf(format, args...)
	b.mu.Lock()
	b.failed++
	over := b.failed > b.maxErrors && !b.aborted
	if over {
		b.aborted = true
		b.err = fmt.Errorf("aborting after %d failures (--max-errors %d); fix the cause and re-run %s", b.failed, b.maxErrors, b.cmd)
	}
	b.mu.Unlock()
	if over {
		b.cancel()
	}
}

func (b *errorBudget) failedCount() int { b.mu.Lock(); defer b.mu.Unlock(); return b.failed }

// forEachConcurrent runs work on each item using up to `concurrency` goroutines,
// all sharing the caller's rate limiter (so the aggregate request rate stays
// capped no matter how many workers run). It stops launching new work as soon as
// ctx is cancelled — the SIGINT context or a move phase that has spent its error
// budget — and waits for in-flight work to drain before returning. work must
// mutate shared state only through synchronized helpers (see moveStats).
func forEachConcurrent[T any](ctx context.Context, concurrency int, items []T, work func(T)) {
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, it := range items {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(it T) {
			defer wg.Done()
			defer func() { <-sem }()
			work(it)
		}(it)
	}
	wg.Wait()
}

// cancelOnSignal returns a context cancelled on the first SIGINT/SIGTERM, so a
// run stops cleanly between Drive calls. A second signal kills the process the
// default way.
func cancelOnSignal() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		signal.Stop(sigCh)
		cancel()
	}()
	return ctx, cancel
}

func promptYesNo(prompt string) bool {
	fmt.Fprint(os.Stderr, prompt)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	return strings.ToLower(strings.TrimSpace(scanner.Text())) == "y"
}

// getConfiguredFolder fetches the folder a config section points at and
// verifies it really is a folder whose live name matches the configured name —
// the guard against a stale or mistyped id. section is the dotted config path
// used in error messages.
func getConfiguredFolder(ctx context.Context, svc *drive.Service, rc rootConfig, section string) (*drive.File, error) {
	f, err := svc.Files.Get(rc.ID).
		Fields("id, name, mimeType, driveId, parents").
		SupportsAllDrives(true).
		Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("fetching %s folder %s: %w", section, rc.ID, err)
	}
	if f.MimeType != folderMimeType {
		return nil, fmt.Errorf("%s folder %s is not a folder (mimeType %q)", section, rc.ID, f.MimeType)
	}
	// The name verified against the config is normally the folder's own name.
	// But the root folder of a shared drive (id == driveId) is generically named
	// "Drive"; its meaningful name lives on the Drive resource. Look that up and
	// verify — and adopt — the shared drive's name, so callers that print f.Name
	// show the drive's real name rather than "Drive".
	name := f.Name
	if f.DriveId != "" && f.Id == f.DriveId {
		d, err := svc.Drives.Get(f.DriveId).Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("fetching shared drive %s for %s: %w", f.DriveId, section, err)
		}
		name = d.Name
		f.Name = d.Name
	}
	if name != rc.Name {
		return nil, fmt.Errorf("%s folder name mismatch: config says %q, Drive says %q", section, rc.Name, name)
	}
	return f, nil
}

// folderInsideRoot reports whether f is a descendant of crawlRootID, walking
// parents upward (following the first parent at each level). Depth-capped so a
// corrupt parent chain cannot loop forever.
func folderInsideRoot(ctx context.Context, svc *drive.Service, f *drive.File, crawlRootID string) (bool, error) {
	cur := f
	for depth := 0; depth < 100; depth++ {
		if len(cur.Parents) == 0 {
			return false, nil // reached a My-Drive top level without hitting the crawl root
		}
		for _, p := range cur.Parents {
			if p == crawlRootID {
				return true, nil
			}
		}
		parent, err := svc.Files.Get(cur.Parents[0]).
			Fields("id, parents").
			SupportsAllDrives(true).
			Context(ctx).Do()
		if err != nil {
			return false, err
		}
		cur = parent
	}
	return false, nil
}

// listChildren returns every non-trashed child of parentID, paginated. fields is
// the full Drive fields selector, e.g. "nextPageToken, files(id, name)".
func listChildren(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, parentID, fields string) ([]*drive.File, error) {
	var out []*drive.File
	pageToken := ""
	for {
		if err := limiter.Wait(ctx); err != nil {
			return nil, err
		}
		call := svc.Files.List().
			Q(fmt.Sprintf("'%s' in parents and trashed = false", parentID)).
			Fields(googleapi.Field(fields)).
			SupportsAllDrives(true).IncludeItemsFromAllDrives(true).
			PageSize(1000).
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		list, err := call.Do()
		if err != nil {
			return nil, err
		}
		out = append(out, list.Files...)
		if list.NextPageToken == "" {
			return out, nil
		}
		pageToken = list.NextPageToken
	}
}

// findChildFolder returns the (first) non-trashed subfolder of parentID named
// name, or nil if none exists. name must not contain single quotes.
func findChildFolder(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, parentID, name string) (*drive.File, error) {
	if err := limiter.Wait(ctx); err != nil {
		return nil, err
	}
	list, err := svc.Files.List().
		Q(fmt.Sprintf("'%s' in parents and name = '%s' and mimeType = '%s' and trashed = false", parentID, name, folderMimeType)).
		Fields("files(id, name)").
		SupportsAllDrives(true).IncludeItemsFromAllDrives(true).
		PageSize(10).
		Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	if len(list.Files) == 0 {
		return nil, nil
	}
	return list.Files[0], nil
}

func createFolder(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, parentID, name string) (*drive.File, error) {
	if err := limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return svc.Files.Create(&drive.File{
		Name:     name,
		MimeType: folderMimeType,
		Parents:  []string{parentID},
	}).SupportsAllDrives(true).Fields("id, name").Context(ctx).Do()
}

// moveFile reparents an item, retrying transient failures with backoff. Notably
// this includes the eventual-consistency 403 unpack can hit right after moving a
// destination folder out of a shared drive (see retryable); the pack/unpack
// write path has no other retry, so it lives here.
func moveFile(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, addParent, removeParent string) error {
	return withRetry(ctx, limiter, "files.update move "+fileID, func() error {
		_, err := svc.Files.Update(fileID, nil).
			AddParents(addParent).
			RemoveParents(removeParent).
			SupportsAllDrives(true).
			Fields("id").
			Context(ctx).Do()
		return err
	})
}

// findUserPermission returns email's existing permission on fileID
// (case-insensitive), or nil if none. Used to make granting idempotent so a
// re-run does not re-notify the user.
func findUserPermission(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, email string) (*drive.Permission, error) {
	pageToken := ""
	for {
		if err := limiter.Wait(ctx); err != nil {
			return nil, err
		}
		call := svc.Permissions.List(fileID).
			Fields("nextPageToken, permissions(id, type, emailAddress, role)").
			SupportsAllDrives(true).
			PageSize(100).
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		list, err := call.Do()
		if err != nil {
			return nil, err
		}
		for _, p := range list.Permissions {
			if p.Type == "user" && strings.EqualFold(p.EmailAddress, email) {
				return p, nil
			}
		}
		if list.NextPageToken == "" {
			return nil, nil
		}
		pageToken = list.NextPageToken
	}
}

// grantPermission grants email the given role on fileID. role uses the Drive
// API name — "organizer" is "Manager" in the Drive UI. A notification email is
// sent so the user gets a link to the folder.
func grantPermission(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, email, role string) error {
	if err := limiter.Wait(ctx); err != nil {
		return err
	}
	_, err := svc.Permissions.Create(fileID, &drive.Permission{
		Type:         "user",
		Role:         role,
		EmailAddress: email,
	}).SupportsAllDrives(true).SendNotificationEmail(false).Context(ctx).Do()
	return err
}

// revokePermission removes the permission with permissionID from fileID. Used
// by unpack to drop the migrating user's access once the round trip is done.
func revokePermission(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, permissionID string) error {
	if err := limiter.Wait(ctx); err != nil {
		return err
	}
	return svc.Permissions.Delete(fileID, permissionID).SupportsAllDrives(true).Context(ctx).Do()
}

func deleteFile(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID string) error {
	if err := limiter.Wait(ctx); err != nil {
		return err
	}
	return svc.Files.Delete(fileID).SupportsAllDrives(true).Context(ctx).Do()
}

// getFileState fetches the live parents, owner, and trashed flag of one item.
// pack and unpack move optimistically (one files.update per item, no pre-read)
// and call this only to diagnose a failed move or an expected-but-missing item.
func getFileState(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID string) (*drive.File, error) {
	if err := limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return svc.Files.Get(fileID).
		Fields("id, name, parents, trashed, driveId, owners(emailAddress, permissionId)").
		SupportsAllDrives(true).
		Context(ctx).Do()
}

// hasParent reports whether parentID is among the file's live parents.
func hasParent(f *drive.File, parentID string) bool {
	for _, p := range f.Parents {
		if p == parentID {
			return true
		}
	}
	return false
}

// ownedByAccount reports whether the file's listed owner matches account
// (email, case-insensitive, or Drive permission id). Shared-drive items have
// no owners and never match.
func ownedByAccount(f *drive.File, account string) bool {
	for _, o := range f.Owners {
		if strings.EqualFold(o.EmailAddress, account) || o.PermissionId == account {
			return true
		}
	}
	return false
}

// isNotFound reports whether err is a Drive 404 — for a files.update move this
// means the item or, more often, the destination parent no longer exists.
func isNotFound(err error) bool {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == 404
	}
	return false
}
