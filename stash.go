package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

// stashShortcutName is the name of the placeholder shortcut stash push leaves
// in an emptied folder (pointing at the folder's stash subfolder) and stash pop
// later removes.
const stashShortcutName = "Contents temporarily moved"

var stashCmd = &cobra.Command{
	Use:   "stash",
	Short: "Park a user's folder contents in a My-Drive stash so the folders can transit a shared drive, then restore them",
	Long: `Park (push) and restore (pop) the contents of a user's folders through a
My-Drive stash folder.

Background: to transfer ownership of a user's items to the org, the user drags
them into a shared-drive staging folder (the drag flips ownership), then
restore-locations moves them back to their original parents. Loose files round-
trip cleanly. A folder the user owns, however, usually contains files owned by
OTHER accounts — and a shared drive cannot hold items the dragging user does not
own, so the folder's move is blocked or those files are orphaned.

stash push empties each of the user's folders into a stash folder (a regular
My-Drive folder that CAN hold third-party-owned files) so the now-empty folder
can transit the shared drive. stash pop refills the folders afterwards.`,
}

var stashPushCmd = &cobra.Command{
	Use:   "push <user>",
	Short: "Move the contents of every folder owned by <user> into the stash folder",
	Long: `For every folder owned by <user> (email or owner id) in the database, move all
of its files into a per-folder subfolder of the configured stash folder, named
after the original folder's Drive ID. The original folder's sharing is recreated
on the stash subfolder (with sendNotificationEmail=false), and a shortcut named
"` + stashShortcutName + `" pointing at the stash subfolder is left
behind in the now-empty original folder.

Before moving anything, this command runs the check-edit-access scan and, if any
crawled node is not editable by the running account, prints the count and asks
for confirmation. It also verifies the stash folder's name matches config, that
it lives inside the crawl root, and that it is not in a shared drive.

Run this BEFORE asking the user to drag their folders into the staging folder.
Because the stash folder is inside the crawl root, the user's own parked files
still appear in their "owner:me" Drive search, so the user moves them to staging
along with their (now-empty) folders like any other loose file.

This command requires the full Drive scope. If you previously authenticated with
drive.readonly, delete token.json and re-run to re-consent.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		return runStashPush(dbPath, cfgPath, args[0])
	},
}

var stashPopCmd = &cobra.Command{
	Use:   "pop",
	Short: "Move stashed files back to their original folders and delete the stash subfolders",
	Long: `Drain the configured stash folder: for each subfolder (named after an original
folder's Drive ID), move its files back into that original folder, then — once
the subfolder is empty — remove the "` + stashShortcutName + `"
shortcut from the original folder and delete the now-empty stash subfolder.

pop takes no user argument: it drains the whole stash. That is safe because each
subfolder is keyed by a globally-unique original-folder ID, so contents always
return to the right place regardless of which user they came from.

Run this AFTER restore-locations has moved the (now org-owned) folders back into
the regular tree — externally-owned files can only be returned once the folder
is back out of the shared drive.

This command requires the full Drive scope.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfgPath, _ := cmd.Flags().GetString("config")
		return runStashPop(cfgPath)
	},
}

func init() {
	stashPushCmd.Flags().String("db", "drive.db", "path to the SQLite database")
	stashPushCmd.Flags().String("config", "config.json", "path to the config JSON")
	stashPopCmd.Flags().String("config", "config.json", "path to the config JSON")
	stashCmd.AddCommand(stashPushCmd, stashPopCmd)
}

// cancelOnSignal returns a context cancelled on the first SIGINT/SIGTERM, so a
// stash run stops cleanly between Drive calls. A second signal kills the process
// the default way.
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

func runStashPush(dbPath, cfgPath, account string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	stash := cfg.Stash.Folder
	if stash.ID == "" || stash.Name == "" {
		return fmt.Errorf("%s must set stash.folder.id and stash.folder.name", cfgPath)
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	folders, err := foldersOwnedBy(db, account)
	if err != nil {
		return err
	}
	if len(folders) == 0 {
		fmt.Fprintf(os.Stderr, "No folders owned by %q in the database; nothing to stash.\n", account)
		return nil
	}

	// Edit-access pre-check: the same scan check-edit-access reports. If any
	// crawled node is not editable, the run may fail to move some files, so
	// confirm before proceeding.
	uneditable, err := nodesLackingEditAccess(db)
	if err != nil {
		return err
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
		fmt.Fprintf(os.Stderr, "WARNING: %d crawled item(s) are not editable by the crawling account: %d folder(s), %d file(s).\n",
			len(uneditable), folderCount, fileCount)
		fmt.Fprintln(os.Stderr, "Moving those items will fail. Run check-edit-access for the full list.")
		if !promptYesNo("Continue with stash push anyway? [y/N] ") {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return nil
		}
	}

	ctx, cancel := cancelOnSignal()
	defer cancel()

	svc, err := newDriveService(ctx, drive.DriveScope)
	if err != nil {
		return err
	}

	about, err := svc.About.Get().Fields("user").Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("fetching current user info: %w", err)
	}
	me := about.User

	// Validate the stash folder before touching anything: right name, inside the
	// crawl root, and not in a shared drive.
	crawlRoot, err := crawlRootDriveID(db)
	if err != nil {
		return fmt.Errorf("fetching crawl root: %w", err)
	}
	if err := validateStashFolder(ctx, svc, stash, crawlRoot); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "About to stash the contents of %d folder(s) owned by %q into %q.\n", len(folders), account, stash.Name)
	fmt.Fprintf(os.Stderr, "Stash subfolders and shortcuts will be created by: %s.\n", me.EmailAddress)
	if !promptYesNo("Continue? [y/N] ") {
		fmt.Fprintln(os.Stderr, "Aborted.")
		return nil
	}

	limiter := rate.NewLimiter(rate.Limit(3), 3)

	var processed, filesMoved, failed int
	for _, f := range folders {
		if err := ctx.Err(); err != nil {
			log.Printf("interrupted: %d folder(s) processed, %d file(s) moved, %d failed", processed, filesMoved, failed)
			return err
		}

		// Find an existing stash subfolder first so we can recognise (and skip)
		// our own marker shortcut when listing, and resume a partial push.
		sub, err := findChildFolder(ctx, svc, limiter, stash.ID, f.driveID)
		if err != nil {
			log.Printf("ERROR folder %q (%s): looking up stash subfolder: %v", f.name, f.driveID, err)
			failed++
			continue
		}

		children, err := listChildren(ctx, svc, limiter, f.driveID,
			"nextPageToken, files(id, name, mimeType, shortcutDetails(targetId))")
		if err != nil {
			log.Printf("ERROR folder %q (%s): listing children: %v", f.name, f.driveID, err)
			failed++
			continue
		}

		var movable []*drive.File
		for _, c := range children {
			if c.MimeType == folderMimeType {
				continue // subfolders stay; folders owned by the user are processed on their own
			}
			if sub != nil && c.MimeType == shortcutMimeType && c.ShortcutDetails != nil && c.ShortcutDetails.TargetId == sub.Id {
				continue // our own "Contents temporarily moved" marker (resume)
			}
			movable = append(movable, c)
		}

		// Nothing to move and no existing stash subfolder: this folder has no
		// files to park, so leave it untouched (no empty subfolder, no shortcut).
		if len(movable) == 0 && sub == nil {
			continue
		}

		if sub == nil {
			sub, err = createFolder(ctx, svc, limiter, stash.ID, f.driveID)
			if err != nil {
				log.Printf("ERROR folder %q (%s): creating stash subfolder: %v", f.name, f.driveID, err)
				failed++
				continue
			}
			perms, err := folderPermissionsFor(db, f.driveID)
			if err != nil {
				log.Printf("WARN folder %q (%s): reading permissions: %v", f.name, f.driveID, err)
			} else {
				applyFolderPermissions(ctx, svc, limiter, perms, sub.Id, me.EmailAddress)
			}
		}

		for _, c := range movable {
			if err := ctx.Err(); err != nil {
				log.Printf("interrupted: %d folder(s) processed, %d file(s) moved, %d failed", processed, filesMoved, failed)
				return err
			}
			if err := moveFile(ctx, svc, limiter, c.Id, sub.Id, f.driveID); err != nil {
				log.Printf("ERROR moving %s (%s) into stash: %v", c.Name, c.Id, err)
				failed++
				continue
			}
			filesMoved++
		}

		if err := ensureMovedShortcut(ctx, svc, limiter, f.driveID, sub.Id); err != nil {
			log.Printf("WARN folder %q (%s): creating placeholder shortcut: %v", f.name, f.driveID, err)
		}
		log.Printf("OK folder %q (%s): %d file(s) -> stash %s", f.name, f.driveID, len(movable), sub.Id)
		processed++
	}

	log.Printf("done: %d folder(s) processed, %d file(s) moved, %d failed", processed, filesMoved, failed)
	if failed > 0 {
		return fmt.Errorf("%d item(s) failed; re-run stash push to retry", failed)
	}
	return nil
}

func runStashPop(cfgPath string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	stash := cfg.Stash.Folder
	if stash.ID == "" || stash.Name == "" {
		return fmt.Errorf("%s must set stash.folder.id and stash.folder.name", cfgPath)
	}

	ctx, cancel := cancelOnSignal()
	defer cancel()

	svc, err := newDriveService(ctx, drive.DriveScope)
	if err != nil {
		return err
	}

	// Guard against a stale id: verify the stash folder really is that folder.
	folder, err := svc.Files.Get(stash.ID).
		Fields("id, name, mimeType").
		SupportsAllDrives(true).
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("fetching stash folder %s: %w", stash.ID, err)
	}
	if folder.MimeType != folderMimeType {
		return fmt.Errorf("stash folder %s is not a folder (mimeType %q)", stash.ID, folder.MimeType)
	}
	if folder.Name != stash.Name {
		return fmt.Errorf("stash folder name mismatch: config says %q, Drive says %q", stash.Name, folder.Name)
	}

	limiter := rate.NewLimiter(rate.Limit(3), 3)

	subs, err := listChildren(ctx, svc, limiter, stash.ID, "nextPageToken, files(id, name, mimeType)")
	if err != nil {
		return fmt.Errorf("listing stash folder: %w", err)
	}
	var stashSubs []*drive.File
	for _, s := range subs {
		if s.MimeType == folderMimeType {
			stashSubs = append(stashSubs, s)
		}
	}
	if len(stashSubs) == 0 {
		fmt.Fprintln(os.Stderr, "Stash folder has no subfolders; nothing to pop.")
		return nil
	}

	fmt.Fprintf(os.Stderr, "About to move the contents of %d stash subfolder(s) back to their original folders and delete the subfolders.\n", len(stashSubs))
	if !promptYesNo("Continue? [y/N] ") {
		fmt.Fprintln(os.Stderr, "Aborted.")
		return nil
	}

	var restored, filesMoved, failed int
	for _, sub := range stashSubs {
		if err := ctx.Err(); err != nil {
			log.Printf("interrupted: %d subfolder(s) restored, %d file(s) moved, %d failed", restored, filesMoved, failed)
			return err
		}
		origID := sub.Name // stash subfolders are named after the original folder's Drive ID

		contents, err := listChildren(ctx, svc, limiter, sub.Id, "nextPageToken, files(id, name)")
		if err != nil {
			log.Printf("ERROR stash subfolder %s (-> %s): listing: %v", sub.Id, origID, err)
			failed++
			continue
		}
		var moveFailed bool
		for _, c := range contents {
			if err := ctx.Err(); err != nil {
				log.Printf("interrupted: %d subfolder(s) restored, %d file(s) moved, %d failed", restored, filesMoved, failed)
				return err
			}
			if err := moveFile(ctx, svc, limiter, c.Id, origID, sub.Id); err != nil {
				log.Printf("ERROR moving %s (%s) back to %s: %v", c.Name, c.Id, origID, err)
				failed++
				moveFailed = true
				continue
			}
			filesMoved++
		}

		// Only tear down the subfolder + shortcut once it is genuinely empty, so
		// nothing that failed to move is silently lost.
		remaining, err := listChildren(ctx, svc, limiter, sub.Id, "nextPageToken, files(id)")
		if err != nil {
			log.Printf("ERROR stash subfolder %s: re-checking contents: %v", sub.Id, err)
			failed++
			continue
		}
		if len(remaining) > 0 || moveFailed {
			log.Printf("WARN stash subfolder %s (-> %s) still has %d item(s); leaving it and its shortcut in place", sub.Id, origID, len(remaining))
			continue
		}

		if err := removeMovedShortcut(ctx, svc, limiter, origID, sub.Id); err != nil {
			log.Printf("WARN removing placeholder shortcut from %s: %v", origID, err)
		}
		if err := deleteFile(ctx, svc, limiter, sub.Id); err != nil {
			log.Printf("ERROR deleting empty stash subfolder %s: %v", sub.Id, err)
			failed++
			continue
		}
		log.Printf("OK stash subfolder %s -> %s: %d file(s) restored, subfolder deleted", sub.Id, origID, len(contents))
		restored++
	}

	log.Printf("done: %d subfolder(s) restored, %d file(s) moved, %d failed", restored, filesMoved, failed)
	if failed > 0 {
		return fmt.Errorf("%d item(s) failed; re-run stash pop to retry", failed)
	}
	return nil
}

// validateStashFolder verifies the configured stash folder is the right folder
// (name match), is not in a shared drive, and lives inside the crawl root.
func validateStashFolder(ctx context.Context, svc *drive.Service, stash rootConfig, crawlRootID string) error {
	f, err := svc.Files.Get(stash.ID).
		Fields("id, name, mimeType, driveId, parents").
		SupportsAllDrives(true).
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("fetching stash folder %s: %w", stash.ID, err)
	}
	if f.MimeType != folderMimeType {
		return fmt.Errorf("stash folder %s is not a folder (mimeType %q)", stash.ID, f.MimeType)
	}
	if f.Name != stash.Name {
		return fmt.Errorf("stash folder name mismatch: config says %q, Drive says %q", stash.Name, f.Name)
	}
	if f.DriveId != "" {
		return fmt.Errorf("stash folder %s is in a shared drive (%s); it must be a regular My-Drive folder so it can hold third-party-owned files", stash.ID, f.DriveId)
	}
	inside, err := folderInsideRoot(ctx, svc, f, crawlRootID)
	if err != nil {
		return fmt.Errorf("checking stash folder is inside the crawl root: %w", err)
	}
	if !inside {
		return fmt.Errorf("stash folder %s (%q) is not inside the crawl root %s; place it under the crawl root so owners' parked files surface in their Drive search", stash.ID, stash.Name, crawlRootID)
	}
	return nil
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
// name, or nil if none exists. name is a Drive ID here (no quoting needed).
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

// applyFolderPermissions recreates perms on targetID (a freshly-created stash
// subfolder). It skips owner grants (the stash subfolder is owned by the running
// account), deleted grants, the running account's own grant, and any grant with
// missing required fields. Failures are logged and skipped — recreating one
// grant is best-effort and must not abort the whole push.
func applyFolderPermissions(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, perms []permission, targetID, myEmail string) {
	for _, p := range perms {
		if p.deleted || p.role == "owner" {
			continue
		}
		perm := &drive.Permission{Type: p.typ, Role: p.role}
		switch p.typ {
		case "user", "group":
			if !p.emailAddress.Valid {
				continue
			}
			if strings.EqualFold(p.emailAddress.String, myEmail) {
				continue // we already have access; recreating it is pointless/erroring
			}
			perm.EmailAddress = p.emailAddress.String
		case "domain":
			if !p.domain.Valid {
				continue
			}
			perm.Domain = p.domain.String
			perm.AllowFileDiscovery = p.allowFileDiscovery.Bool
			perm.ForceSendFields = []string{"AllowFileDiscovery"}
		case "anyone":
			perm.AllowFileDiscovery = p.allowFileDiscovery.Bool
			perm.ForceSendFields = []string{"AllowFileDiscovery"}
		default:
			continue
		}
		if err := limiter.Wait(ctx); err != nil {
			return
		}
		if _, err := svc.Permissions.Create(targetID, perm).
			SendNotificationEmail(false).
			SupportsAllDrives(true).
			Fields("id").
			Context(ctx).Do(); err != nil {
			log.Printf("WARN could not recreate %s/%s permission on stash subfolder %s: %v", p.typ, p.role, targetID, err)
		}
	}
}

// ensureMovedShortcut creates the placeholder shortcut in parentID pointing at
// targetID, unless one already pointing there exists (idempotent for resume).
func ensureMovedShortcut(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, parentID, targetID string) error {
	if err := limiter.Wait(ctx); err != nil {
		return err
	}
	list, err := svc.Files.List().
		Q(fmt.Sprintf("'%s' in parents and name = '%s' and mimeType = '%s' and trashed = false", parentID, stashShortcutName, shortcutMimeType)).
		Fields("files(id, shortcutDetails(targetId))").
		SupportsAllDrives(true).IncludeItemsFromAllDrives(true).
		PageSize(100).
		Context(ctx).Do()
	if err != nil {
		return err
	}
	for _, f := range list.Files {
		if f.ShortcutDetails != nil && f.ShortcutDetails.TargetId == targetID {
			return nil // already present
		}
	}
	if err := limiter.Wait(ctx); err != nil {
		return err
	}
	_, err = svc.Files.Create(&drive.File{
		Name:            stashShortcutName,
		MimeType:        shortcutMimeType,
		Parents:         []string{parentID},
		ShortcutDetails: &drive.FileShortcutDetails{TargetId: targetID},
	}).SupportsAllDrives(true).Fields("id").Context(ctx).Do()
	return err
}

// removeMovedShortcut deletes any placeholder shortcut in parentID that points
// at targetID. A missing shortcut is not an error.
func removeMovedShortcut(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, parentID, targetID string) error {
	if err := limiter.Wait(ctx); err != nil {
		return err
	}
	list, err := svc.Files.List().
		Q(fmt.Sprintf("'%s' in parents and name = '%s' and mimeType = '%s' and trashed = false", parentID, stashShortcutName, shortcutMimeType)).
		Fields("files(id, shortcutDetails(targetId))").
		SupportsAllDrives(true).IncludeItemsFromAllDrives(true).
		PageSize(100).
		Context(ctx).Do()
	if err != nil {
		return err
	}
	for _, f := range list.Files {
		if f.ShortcutDetails == nil || f.ShortcutDetails.TargetId != targetID {
			continue
		}
		if err := deleteFile(ctx, svc, limiter, f.Id); err != nil {
			return err
		}
	}
	return nil
}
