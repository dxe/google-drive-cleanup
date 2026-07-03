package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
)

var unpackCmd = &cobra.Command{
	Use:   "unpack <user>",
	Short: "Restore a dragged Container's contents (and the Stash) to their original locations, then clean up",
	Long: `Finish <user>'s migration after they drag their Container into the shared-drive
dropoff folder (which flips ownership of everything inside to the org).

Each direct child of the Container is moved back to the original parent
recorded in the database (owned items nested deeper ride along with their
folders), then each Stash item is returned the same way. The Container must be
restored first: Stash items are owned by third parties, and a shared drive
cannot hold those, so their destination folders have to be back out of the
shared drive before they can follow.

Items that cannot be placed — not in the database, or their original parent is
gone — are quarantined under <packing-folder>/<user>/Errors/<original parent
id>/ for manual restore instead of blocking cleanup. Once the Stash and
Container are verified empty they are deleted, along with the per-user folder
if nothing (such as a non-empty Errors folder) remains in it.

This command requires the full Drive scope and, to move items out of the shared
drive, manager access on it.

If the user became unavailable and never dragged the Container into the shared
drive, pass --allow-not-moved to abort the migration: files are restored to
their original locations straight from the packing folder so they are usable
again, and pack can be re-run later to retry. Ownership never transferred in
that case, so the database owner columns are left unchanged.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		maxErrors, _ := cmd.Flags().GetInt("max-errors")
		allowNotMoved, _ := cmd.Flags().GetBool("allow-not-moved")
		return runUnpack(dbPath, cfgPath, args[0], dryRun, maxErrors, allowNotMoved)
	},
}

func init() {
	unpackCmd.Flags().Bool("dry-run", false, "report what would move without changing anything (read-only scope)")
	unpackCmd.Flags().Int("max-errors", 5, "abort once more than this many items fail to move")
	unpackCmd.Flags().Bool("allow-not-moved", false, "abort the migration: restore files even if the Container was never dragged into the shared drive (ownership never flipped, so the database owner columns are left unchanged)")
}

func runUnpack(dbPath, cfgPath, account string, dryRun bool, maxErrors int, allowNotMoved bool) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Migration.DropoffFolder.validate("migration.dropoff-folder"); err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	m, err := getUserMigration(db, account)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("no pack recorded for %q; run pack first", account)
	}
	if !m.packedAt.Valid {
		log.Printf("WARN pack for %q never finished cleanly; some owned items may not be in the Container", account)
		if !dryRun && !promptYesNo("Continue with unpack anyway? [y/N] ") {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return nil
		}
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

	dropoff, err := getConfiguredFolder(ctx, svc, cfg.Migration.DropoffFolder, "migration.dropoff-folder")
	if err != nil {
		return err
	}
	if dropoff.DriveId == "" {
		return fmt.Errorf("dropoff folder %s is not in a shared drive", dropoff.Id)
	}

	// The Container must actually have been dragged into the shared drive, and
	// the running account must be able to move items back out of it.
	container, err := svc.Files.Get(m.containerID).
		Fields("id, name, driveId, trashed, capabilities(canMoveItemOutOfDrive)").
		SupportsAllDrives(true).
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("fetching Container %s: %w", m.containerID, err)
	}
	if container.Trashed {
		return fmt.Errorf("Container %s is trashed", m.containerID)
	}
	// containerInSharedDrive is the normal, post-drag state: ownership of
	// everything inside has flipped to the org. When the Container is still in
	// My Drive the drag never happened; --allow-not-moved lets the admin abort
	// the migration and restore files to their original locations anyway (e.g.
	// the user became unavailable), so they stay put until a retry. Ownership
	// never transferred, so those restores must NOT flip the database owner.
	containerInSharedDrive := container.DriveId != ""
	if !containerInSharedDrive {
		if !allowNotMoved {
			return fmt.Errorf("Container %s is still outside the shared drive — has %s dragged it into %q yet? (pass --allow-not-moved to abort the migration and restore files to their original locations instead)", m.containerID, account, dropoff.Name)
		}
		log.Printf("WARN Container %s was never dragged into the shared drive; --allow-not-moved set: aborting the migration and restoring files to their original locations. Ownership did not transfer to the org, so files keep their current owners and the database owner columns are left unchanged.", m.containerID)
	} else {
		if container.DriveId != dropoff.DriveId {
			return fmt.Errorf("Container %s is in shared drive %s, but not the dropoff folder's shared drive %s", m.containerID, container.DriveId, dropoff.DriveId)
		}
		if container.Capabilities != nil && !container.Capabilities.CanMoveItemOutOfDrive {
			return fmt.Errorf("%s cannot move items out of shared drive %s; it needs manager access", me.EmailAddress, container.DriveId)
		}
	}

	stashF, err := svc.Files.Get(m.stashID).
		Fields("id, name, mimeType, driveId").
		SupportsAllDrives(true).
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("fetching Stash %s: %w", m.stashID, err)
	}
	if stashF.MimeType != folderMimeType || stashF.DriveId != "" {
		return fmt.Errorf("Stash %s is not a My-Drive folder anymore; refusing to continue", m.stashID)
	}

	// After the drag the org owns everything, so restored items end up owned by
	// the running account. When aborting an un-dragged migration, ownership is
	// untouched and items return to their original owners.
	ownership := fmt.Sprintf("owned by: %s", me.EmailAddress)
	if !containerInSharedDrive {
		ownership = "left with their current owners (migration aborted; nothing was dragged)"
	}
	if dryRun {
		fmt.Fprintf(os.Stderr, "DRY RUN: no files will be moved. Restored files would be %s.\n", ownership)
	} else {
		fmt.Fprintf(os.Stderr, "Restored files will be %s.\n", ownership)
		if !promptYesNo("Continue? [y/N] ") {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return nil
		}
	}

	limiter := rate.NewLimiter(rate.Limit(3), 3)

	var restored, quarantined, failed int
	fail := func(format string, args ...any) error {
		log.Printf(format, args...)
		failed++
		if failed > maxErrors {
			return fmt.Errorf("aborting after %d failures (--max-errors %d); fix the cause and re-run unpack", failed, maxErrors)
		}
		return nil
	}

	// quarantine moves an item that cannot be placed into
	// <user folder>/Errors/<parentLabel>/, where parentLabel is the Drive ID of
	// the folder it belongs in (or "unknown"), creating folders lazily.
	var errorsFolder *drive.File
	errorsSubs := make(map[string]*drive.File)
	quarantine := func(itemID, fromParent, parentLabel string) error {
		if errorsFolder == nil {
			ef, err := findChildFolder(ctx, svc, limiter, m.userFolderID, errorsFolderName)
			if err != nil {
				return err
			}
			if ef == nil {
				if ef, err = createFolder(ctx, svc, limiter, m.userFolderID, errorsFolderName); err != nil {
					return err
				}
			}
			errorsFolder = ef
		}
		sub := errorsSubs[parentLabel]
		if sub == nil {
			s, err := findChildFolder(ctx, svc, limiter, errorsFolder.Id, parentLabel)
			if err != nil {
				return err
			}
			if s == nil {
				if s, err = createFolder(ctx, svc, limiter, errorsFolder.Id, parentLabel); err != nil {
					return err
				}
			}
			errorsSubs[parentLabel] = s
			sub = s
		}
		return moveFile(ctx, svc, limiter, itemID, sub.Id, fromParent)
	}

	// orphanLabel names the Errors subfolder for an item with no database row:
	// the live parent pack swept it from, if it was recorded, else "unknown".
	orphanLabel := func(itemID string) string {
		if p, err := packOrphanParent(db, account, itemID); err == nil {
			return p
		}
		return "unknown"
	}

	// restoreChildren drains one level of source (the Container or the Stash),
	// moving each child back to its original parent from the database.
	restoreChildren := func(source *drive.File, sourceLabel string, flipOwner bool) error {
		children, err := listChildren(ctx, svc, limiter, source.Id, "nextPageToken, files(id, name)")
		if err != nil {
			return fmt.Errorf("listing %s: %w", sourceLabel, err)
		}
		prog := newProgress()
		for i, c := range children {
			if err := ctx.Err(); err != nil {
				log.Printf("interrupted: %d restored, %d quarantined, %d failed", restored, quarantined, failed)
				return err
			}
			prog.tick("progress: restoring %s: %d/%d", sourceLabel, i, len(children))
			orig, derr := originalParentDriveID(db, c.Id)
			if derr == sql.ErrNoRows {
				label := orphanLabel(c.Id)
				if dryRun {
					log.Printf("WOULD quarantine %q (%s): not in database -> %s/%s", c.Name, c.Id, errorsFolderName, label)
					quarantined++
					continue
				}
				if qerr := quarantine(c.Id, source.Id, label); qerr != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if err := fail("ERROR quarantining %q (%s): %v", c.Name, c.Id, qerr); err != nil {
						return err
					}
					continue
				}
				log.Printf("WARN %q (%s) is not in the database; quarantined under %s/%s for manual restore", c.Name, c.Id, errorsFolderName, label)
				quarantined++
				continue
			}
			if derr != nil {
				return derr
			}
			if dryRun {
				log.Printf("WOULD move %q (%s) -> parent %s", c.Name, c.Id, orig)
				restored++
				continue
			}
			merr := moveFile(ctx, svc, limiter, c.Id, orig, source.Id)
			if merr == nil {
				detailf("OK %q (%s) -> parent %s", c.Name, c.Id, orig)
				if flipOwner {
					// The drag flipped ownership of everything inside the
					// Container to the org, so this child and every item that
					// rode along inside it (an owned subtree moves as one item)
					// is now owned by the running account. Record that for the
					// whole subtree without a per-file Drive lookup; the update
					// only touches rows the DB attributes to the migrating user,
					// leaving nested third-party (stashed) items alone.
					if n, err := updateSubtreeOwner(db, c.Id, account, me.EmailAddress, me.PermissionId, me.DisplayName); err != nil {
						log.Printf("WARN could not update owner in DB for %q (%s) subtree: %v", c.Name, c.Id, err)
					} else {
						detailf("   updated owner for %d DB row(s) under %q (%s)", n, c.Name, c.Id)
					}
				}
				restored++
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isNotFound(merr) {
				// The destination is the problem (original parent deleted or
				// trashed): quarantine so cleanup is not blocked.
				if qerr := quarantine(c.Id, source.Id, orig); qerr != nil {
					if err := fail("ERROR quarantining %q (%s) after missing parent %s: %v", c.Name, c.Id, orig, qerr); err != nil {
						return err
					}
					continue
				}
				log.Printf("WARN original parent %s of %q (%s) is gone; quarantined under %s/%s for manual restore", orig, c.Name, c.Id, errorsFolderName, orig)
				quarantined++
				continue
			}
			if err := fail("ERROR moving %q (%s) to parent %s: %v (if the destination is still inside the shared drive, re-run unpack after the Container restore succeeds)", c.Name, c.Id, orig, merr); err != nil {
				return err
			}
		}
		return nil
	}

	// The Container must be restored before the Stash: Stash items are owned by
	// third parties, and a shared drive cannot hold those, so their destination
	// folders have to be back in the regular tree first.
	if err := restoreChildren(container, "Container", containerInSharedDrive); err != nil {
		return err
	}
	if err := restoreChildren(stashF, "Stash", false); err != nil {
		return err
	}

	// Cleanup: delete the scaffolding only once live listings confirm it is
	// genuinely empty, so nothing that failed to move is silently lost.
	if dryRun {
		log.Printf("NOTE cleanup (deleting the emptied Stash, Container, and per-user folder) is skipped in a dry run")
	} else {
		deleteIfEmpty := func(f *drive.File, label string) error {
			remaining, err := listChildren(ctx, svc, limiter, f.Id, "nextPageToken, files(id)")
			if err != nil {
				return fmt.Errorf("re-checking %s contents: %w", label, err)
			}
			if len(remaining) > 0 {
				log.Printf("WARN %s still has %d item(s); leaving it in place", label, len(remaining))
				return nil
			}
			if err := deleteFile(ctx, svc, limiter, f.Id); err != nil {
				return fmt.Errorf("deleting empty %s: %w", label, err)
			}
			log.Printf("deleted empty %s", label)
			return nil
		}
		if err := deleteIfEmpty(stashF, "Stash"); err != nil {
			return err
		}
		if err := deleteIfEmpty(container, "Container"); err != nil {
			return err
		}
		// The per-user folder goes too, unless something remains in it — e.g. a
		// non-empty Errors folder, or a Stash/Container that could not empty.
		if err := deleteIfEmpty(&drive.File{Id: m.userFolderID}, fmt.Sprintf("per-user folder for %s", account)); err != nil {
			return err
		}
	}

	verb := "restored"
	if dryRun {
		verb = "would restore"
	}
	log.Printf("done: %d item(s) %s, %d quarantined, %d failed", restored, verb, quarantined, failed)
	if quarantined > 0 {
		log.Printf("NOTE quarantined items are under the %s folder; each subfolder is named after the Drive ID of the folder the item belongs in (\"unknown\" if never crawled)", errorsFolderName)
	}
	if failed > 0 {
		return fmt.Errorf("%d item(s) failed; re-run unpack to retry", failed)
	}
	if !dryRun {
		if err := markUnpacked(db, account); err != nil {
			return fmt.Errorf("recording unpack completion: %w", err)
		}
	}
	return nil
}
