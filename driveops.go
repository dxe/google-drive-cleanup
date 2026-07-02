package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

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
	if f.Name != rc.Name {
		return nil, fmt.Errorf("%s folder name mismatch: config says %q, Drive says %q", section, rc.Name, f.Name)
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

func moveFile(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, addParent, removeParent string) error {
	if err := limiter.Wait(ctx); err != nil {
		return err
	}
	_, err := svc.Files.Update(fileID, nil).
		AddParents(addParent).
		RemoveParents(removeParent).
		SupportsAllDrives(true).
		Fields("id").
		Context(ctx).Do()
	return err
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
