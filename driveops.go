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

// escapeDriveQuery escapes a string for embedding in a files.list q clause:
// backslashes and single quotes get a leading backslash, per the Drive API's
// query grammar.
func escapeDriveQuery(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\'`)
}

// findChildFolder returns the (first) non-trashed subfolder of parentID named
// name, or nil if none exists.
func findChildFolder(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, parentID, name string) (*drive.File, error) {
	if err := limiter.Wait(ctx); err != nil {
		return nil, err
	}
	list, err := svc.Files.List().
		Q(fmt.Sprintf("'%s' in parents and name = '%s' and mimeType = '%s' and trashed = false", parentID, escapeDriveQuery(name), folderMimeType)).
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

// errParentNotConfirmed signals that a move reported success but a follow-up read
// showed the requested parent was not attached. confirmParent re-issues the move
// and returns this so withRetry backs off and re-verifies; retryable treats it as
// transient. If it survives every retry attempt it surfaces to the caller as a
// real failure, so a silently misplaced item fails loudly instead of vanishing.
var errParentNotConfirmed = errors.New("move reported success but the requested parent was not attached")

// confirmParent verifies that addParent is among fileID's live parents after a
// move, repairing it if not. Drive can return success on a files.update that
// removed the old parent — and, for a move out of a shared drive, flipped
// ownership to the mover — yet did NOT attach the requested new parent: the item
// silently lands at the mover's My Drive root, no error returned. This was seen
// during unpack when a file was restored into a destination folder that was
// itself being moved out of the shared drive concurrently. A plain files.get is
// strongly consistent for an item's own parents (unlike the files.list the sweep
// and restore walks use), so it reliably detects the drop; the repair re-moves
// the item from whatever parents it currently has, retrying with backoff until
// addParent sticks or the attempts run out.
func confirmParent(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, addParent string) error {
	return withRetry(ctx, limiter, "confirm parent of "+fileID, func() error {
		f, err := svc.Files.Get(fileID).
			Fields("id, parents").
			SupportsAllDrives(true).
			Context(ctx).Do()
		if err != nil {
			return err
		}
		if hasParent(f, addParent) {
			return nil
		}
		log.Printf("WARN move of %s reported success but parent %s did not attach (item is under %v); re-moving", fileID, addParent, f.Parents)
		if err := limiter.Wait(ctx); err != nil {
			return err
		}
		if _, err := svc.Files.Update(fileID, nil).
			AddParents(addParent).
			RemoveParents(strings.Join(f.Parents, ",")).
			SupportsAllDrives(true).
			Fields("id").
			Context(ctx).Do(); err != nil {
			return err
		}
		return errParentNotConfirmed
	})
}

// moveFileVerified reparents an item like moveFile, then confirms the new parent
// actually attached (see confirmParent). unpack uses it for restores and
// quarantines: those move items out of the shared drive, the one operation
// observed to occasionally report success while silently dropping the item at the
// mover's My Drive root. pack's moves stay within My Drive, so they use plain
// moveFile.
func moveFileVerified(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, addParent, removeParent string) error {
	if err := moveFile(ctx, svc, limiter, fileID, addParent, removeParent); err != nil {
		return err
	}
	return confirmParent(ctx, svc, limiter, fileID, addParent)
}

// listPermissions returns every permission on fileID, paginated.
func listPermissions(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID string) ([]*drive.Permission, error) {
	var out []*drive.Permission
	pageToken := ""
	for {
		if err := limiter.Wait(ctx); err != nil {
			return nil, err
		}
		call := svc.Permissions.List(fileID).
			// The full set the crawl records (see permissionFields): reclaim-folders needs
			// domain and allowFileDiscovery to recreate a grant on its replacement
			// folder, and the extra fields cost nothing to the revoke-only callers.
			Fields("nextPageToken, permissions(id, type, emailAddress, domain, displayName, role, allowFileDiscovery, deleted)").
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
		out = append(out, list.Permissions...)
		if list.NextPageToken == "" {
			return out, nil
		}
		pageToken = list.NextPageToken
	}
}

// findUserPermission returns email's existing permission on fileID
// (case-insensitive), or nil if none. Used to make granting idempotent so a
// re-run does not re-notify the user.
func findUserPermission(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, email string) (*drive.Permission, error) {
	perms, err := listPermissions(ctx, svc, limiter, fileID)
	if err != nil {
		return nil, err
	}
	for _, p := range perms {
		if p.Type == "user" && strings.EqualFold(p.EmailAddress, email) {
			return p, nil
		}
	}
	return nil, nil
}

// grantPermission grants email the given role on fileID. role uses the Drive
// API name — "organizer" is "Manager" in the Drive UI. A notification email is
// sent so the user gets a link to the folder.
func grantPermission(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, email, role string) error {
	return withRetry(ctx, limiter, "permissions.create "+email, func() error {
		_, err := svc.Permissions.Create(fileID, &drive.Permission{
			Type:         "user",
			Role:         role,
			EmailAddress: email,
		}).SupportsAllDrives(true).SendNotificationEmail(false).Context(ctx).Do()
		return err
	})
}

// copyPermission recreates one of a folder's grants on another item, returning
// the permission it created. Notification email is suppressed for the user and
// group grants that would otherwise trigger one; the domain and anyone grants
// never notify, and the Drive API rejects the parameter on them, so it is not
// sent there.
//
// Roles are copied verbatim. An "owner" grant cannot be recreated (ownership is
// not transferable this way) and must be filtered out by the caller.
func copyPermission(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID string, p *drive.Permission) (*drive.Permission, error) {
	want := &drive.Permission{
		Type:         p.Type,
		Role:         p.Role,
		EmailAddress: p.EmailAddress,
		Domain:       p.Domain,
	}
	if p.Type == "domain" || p.Type == "anyone" {
		// allowFileDiscovery is meaningful only here, and false must go on the
		// wire rather than being dropped as a zero value.
		want.AllowFileDiscovery = p.AllowFileDiscovery
		want.ForceSendFields = []string{"AllowFileDiscovery"}
	}
	var out *drive.Permission
	err := withRetry(ctx, limiter, "permissions.create copy "+permissionKey(p), func() error {
		call := svc.Permissions.Create(fileID, want).
			SupportsAllDrives(true).
			Fields("id, type, role, emailAddress, domain, displayName, allowFileDiscovery, deleted").
			Context(ctx)
		if p.Type == "user" || p.Type == "group" {
			call = call.SendNotificationEmail(false)
		}
		created, err := call.Do()
		out = created
		return err
	})
	return out, err
}

// permissionKey identifies who a grant is for, ignoring its role and id, so the
// same grantee can be recognised across two items. Emails and domains are
// lower-cased; an "anyone" grant is keyed by its type alone.
func permissionKey(p *drive.Permission) string {
	switch p.Type {
	case "user", "group":
		return p.Type + ":" + strings.ToLower(p.EmailAddress)
	case "domain":
		return "domain:" + strings.ToLower(p.Domain)
	default:
		return p.Type
	}
}

// revokePermission removes the permission with permissionID from fileID. Used
// by unpack to drop the migrating user's access once the round trip is done.
func revokePermission(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, permissionID string) error {
	return withRetry(ctx, limiter, "permissions.delete "+permissionID, func() error {
		return svc.Permissions.Delete(fileID, permissionID).SupportsAllDrives(true).Context(ctx).Do()
	})
}

func deleteFile(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID string) error {
	return withRetry(ctx, limiter, "files.delete "+fileID, func() error {
		return svc.Files.Delete(fileID).SupportsAllDrives(true).Context(ctx).Do()
	})
}

// removeFromParent detaches fileID from removeParents (comma-separated parent
// IDs) WITHOUT adding a new parent: Drive relocates the item to its owner's My
// Drive, permissions intact. Used by delete --remove-unowned to hand an
// externally-owned file back to its owner.
func removeFromParent(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, removeParents string) error {
	return withRetry(ctx, limiter, "files.update remove-parent "+fileID, func() error {
		_, err := svc.Files.Update(fileID, nil).
			RemoveParents(removeParents).
			SupportsAllDrives(true).
			Fields("id").
			Context(ctx).Do()
		return err
	})
}

// folderIsEmpty reports whether folderID has no non-trashed children (a single
// files.list page of size 1). NOTE files.list is eventually consistent: a
// just-emptied folder may briefly still report a child. Callers treat "not
// empty" as skip-and-retry-later, never as an error.
func folderIsEmpty(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, folderID string) (bool, error) {
	if err := limiter.Wait(ctx); err != nil {
		return false, err
	}
	list, err := svc.Files.List().
		Q(fmt.Sprintf("'%s' in parents and trashed = false", folderID)).
		Fields("files(id)").
		SupportsAllDrives(true).IncludeItemsFromAllDrives(true).
		PageSize(1).
		Context(ctx).Do()
	if err != nil {
		return false, err
	}
	return len(list.Files) == 0, nil
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

// insideAncestor reports whether an item whose live parents are `parents` sits
// anywhere inside ancestorID, following the live parent chain upward.
func insideAncestor(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, parents []string, ancestorID string) (bool, error) {
	const maxHops = 200
	visited := make(map[string]bool)
	queue := append([]string(nil), parents...)
	hops := 0
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if p == ancestorID {
			return true, nil
		}
		if visited[p] || hops >= maxHops {
			continue
		}
		visited[p] = true
		hops++
		f, err := getFileState(ctx, svc, limiter, p)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return false, err
		}
		queue = append(queue, f.Parents...)
	}
	return false, nil
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

// renameFile sets a new name on an item, retrying transient failures. Used by
// reclaim-folders to prefix a folder it is replacing with "(old) ".
func renameFile(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, name string) error {
	return withRetry(ctx, limiter, "files.update rename "+fileID, func() error {
		_, err := svc.Files.Update(fileID, &drive.File{Name: name}).
			SupportsAllDrives(true).
			Fields("id").
			Context(ctx).Do()
		return err
	})
}

// createShortcut creates a Drive shortcut to targetID inside parentID. The
// shortcut is a new file owned by the running account, so it can be placed in a
// folder owned by somebody else as long as the account may add children there.
func createShortcut(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, parentID, name, targetID string) (*drive.File, error) {
	var out *drive.File
	err := withRetry(ctx, limiter, "files.create shortcut "+name, func() error {
		f, err := svc.Files.Create(&drive.File{
			Name:            name,
			MimeType:        shortcutMimeType,
			Parents:         []string{parentID},
			ShortcutDetails: &drive.FileShortcutDetails{TargetId: targetID},
		}).SupportsAllDrives(true).Fields("id, name, shortcutDetails(targetId)").Context(ctx).Do()
		out = f
		return err
	})
	return out, err
}

// findChildrenNamed returns every non-trashed child of parentID named name.
// mimeType, when non-empty, restricts the match to that type. fields is the
// inner Drive fields selector, e.g. "id, name, owners(emailAddress)".
//
// Unlike findChildFolder it hands back every match so the caller can pick by a
// property Drive's query grammar cannot express (reclaim-folders picks the
// candidate owned by the running account, and the shortcut pointing at a target).
// One page of up to 100: an exact-name match in one folder never needs more.
func findChildrenNamed(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, parentID, name, mimeType, fields string) ([]*drive.File, error) {
	q := fmt.Sprintf("'%s' in parents and name = '%s' and trashed = false", parentID, escapeDriveQuery(name))
	if mimeType != "" {
		q += fmt.Sprintf(" and mimeType = '%s'", mimeType)
	}
	if err := limiter.Wait(ctx); err != nil {
		return nil, err
	}
	list, err := svc.Files.List().
		Q(q).
		Fields(googleapi.Field("files(" + fields + ")")).
		SupportsAllDrives(true).IncludeItemsFromAllDrives(true).
		PageSize(100).
		Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return list.Files, nil
}
