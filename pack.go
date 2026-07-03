package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
)

// Per-user folder layout inside the packing folder. The Container must be
// created manually by the admin's PERSONAL Gmail account — only personal
// accounts can transfer ownership to other personal accounts, and the user
// must own the Container to drag it into the shared drive. The CLI creates
// everything else.
const (
	stashFolderName  = "Stash"
	errorsFolderName = "Errors"
)

// containerFolderName is the required name of a user's Container folder. It is
// scoped by account so multiple users' Containers are distinguishable once
// dragged into the same shared drive.
func containerFolderName(account string) string {
	return account + "-Container"
}

var packCmd = &cobra.Command{
	Use:   "pack <user>",
	Short: "Move everything <user> owns into their Container folder, ready for a single drag into the shared drive",
	Long: `Gather everything <user> (email or owner id) owns into one Container folder
under <packing-folder>/<user>/, so the user can transfer it all to the org with
a single drag of the Container into the shared-drive dropoff folder.

Owned subtrees move intact: only "owned roots" (owned items whose parent is not
owned by the user) are moved into the Container; owned items nested below them
ride along. A recursive sweep then moves every item inside the Container tree
that the user does NOT own into a flat Stash folder next to the Container —
a shared drive cannot hold third-party-owned items, so leaving them in place
would block the drag. unpack later returns everything to the original parents
recorded in the database.

The Container itself must already exist: create it inside the per-user folder
with the admin's PERSONAL Gmail account (only personal accounts can transfer
ownership to other personal accounts), so its ownership can be transferred to
the user before they drag it. On a first run pack creates the per-user folder
and Stash, then stops with instructions to create the Container.

This command requires the full Drive scope. If the cached token.json only has
read-only access, the tool re-runs consent automatically to obtain it.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		maxErrors, _ := cmd.Flags().GetInt("max-errors")
		return runPack(dbPath, cfgPath, args[0], dryRun, maxErrors)
	},
}

func init() {
	packCmd.Flags().Bool("dry-run", false, "report what would move without changing anything (read-only scope)")
	packCmd.Flags().Int("max-errors", 5, "abort once more than this many items fail to move")
}

func runPack(dbPath, cfgPath, account string, dryRun bool, maxErrors int) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Migration.PackingFolder.validate("migration.packing-folder"); err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
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
	// The snapshot must describe the tree the config now points at: pack moves
	// live files based on it, so a config root that no longer matches what was
	// crawled means every original-parent decision could be wrong. Refuse and
	// make the operator re-crawl. (unpack deliberately skips this check — it
	// finishes an in-flight migration from recorded per-user state, not the root.)
	if crawlRoot != cfg.Crawl.Root.ID {
		return fmt.Errorf("crawl root in config (%s, %q) does not match the root in the database (%s); crawl.root.id changed since the last crawl — re-run `drive-cleanup crawl` to rebuild the snapshot before packing",
			cfg.Crawl.Root.ID, cfg.Crawl.Root.Name, crawlRoot)
	}

	owned, err := nodesOwnedBy(db, account)
	if err != nil {
		return err
	}
	if len(owned) == 0 {
		fmt.Fprintf(os.Stderr, "Nothing owned by %q in the database; nothing to pack.\n", account)
		return nil
	}
	roots, err := ownedRoots(db, account)
	if err != nil {
		return err
	}
	for _, n := range owned {
		if n.driveID == crawlRoot {
			log.Printf("WARN the crawl root itself is owned by %q; it is never moved", account)
		}
	}
	extraParents, err := extraParentNodeIDs(db)
	if err != nil {
		return err
	}

	if pending, err := countPendingFolders(db); err != nil {
		return err
	} else if pending > 0 {
		log.Printf("WARN the crawl is incomplete (%d folder(s) not fully listed); the database may be missing items. Re-run crawl first for a complete pack.", pending)
	}

	// Edit-access pre-check: the same scan check-edit-access reports. If any
	// crawled node is not editable, the run may fail to move some items, so
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
		if !dryRun && !promptYesNo("Continue with pack anyway? [y/N] ") {
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
	if strings.EqualFold(me.EmailAddress, account) || me.PermissionId == account {
		return fmt.Errorf("pack is running as %s, the account being migrated; run it as an org account with edit access instead", me.EmailAddress)
	}

	packing, err := getConfiguredFolder(ctx, svc, cfg.Migration.PackingFolder, "migration.packing-folder")
	if err != nil {
		return err
	}
	if packing.DriveId != "" {
		return fmt.Errorf("packing folder %s is in a shared drive (%s); it must be a regular My-Drive folder so Stashes can hold third-party-owned files", packing.Id, packing.DriveId)
	}
	if inside, err := folderInsideRoot(ctx, svc, packing, crawlRoot); err != nil {
		return fmt.Errorf("checking the packing folder against the crawl root: %w", err)
	} else if inside {
		return fmt.Errorf("packing folder %s (%q) is inside the crawl root %s; move it outside so re-crawls don't ingest mid-migration scaffolding", packing.Id, packing.Name, crawlRoot)
	}
	dropoff, err := getConfiguredFolder(ctx, svc, cfg.Migration.DropoffFolder, "migration.dropoff-folder")
	if err != nil {
		return err
	}
	if dropoff.DriveId == "" {
		return fmt.Errorf("dropoff folder %s is not in a shared drive; dragging the Container there would not transfer ownership to the org", dropoff.Id)
	}

	limiter := rate.NewLimiter(rate.Limit(3), 3)

	// Resolve (or create) the per-user scaffolding: <packing>/<account>/ with
	// Stash inside. Find-before-create keeps re-runs and crash recovery safe.
	userFolder, err := findChildFolder(ctx, svc, limiter, packing.Id, account)
	if err != nil {
		return fmt.Errorf("looking up the per-user folder: %w", err)
	}
	if userFolder == nil {
		if dryRun {
			log.Printf("WOULD create per-user folder %q under %q", account, packing.Name)
		} else if userFolder, err = createFolder(ctx, svc, limiter, packing.Id, account); err != nil {
			return fmt.Errorf("creating the per-user folder: %w", err)
		}
	}
	var stashF, containerF *drive.File
	if userFolder != nil {
		stashF, err = findChildFolder(ctx, svc, limiter, userFolder.Id, stashFolderName)
		if err != nil {
			return fmt.Errorf("looking up the Stash folder: %w", err)
		}
		if stashF == nil {
			if dryRun {
				log.Printf("WOULD create %q under %s/%s", stashFolderName, packing.Name, account)
			} else if stashF, err = createFolder(ctx, svc, limiter, userFolder.Id, stashFolderName); err != nil {
				return fmt.Errorf("creating the Stash folder: %w", err)
			}
		}
		containerF, err = findChildFolder(ctx, svc, limiter, userFolder.Id, containerFolderName(account))
		if err != nil {
			return fmt.Errorf("looking up the Container folder: %w", err)
		}
	}
	containerInstructions := fmt.Sprintf(
		"create a folder named %q inside %s/%s with the admin's PERSONAL Gmail account (only personal accounts can transfer ownership to other personal accounts; the packing folder must be shared with that account as editor), then re-run pack",
		containerFolderName(account), packing.Name, account)
	if containerF == nil && !dryRun {
		return fmt.Errorf("no %q folder yet: %s", containerFolderName(account), containerInstructions)
	}

	if !dryRun {
		// The Container must not have been dragged already, and ideally is (or
		// will be) owned by the user — the drag requires it.
		cState, err := getFileState(ctx, svc, limiter, containerF.Id)
		if err != nil {
			return fmt.Errorf("fetching Container %s: %w", containerF.Id, err)
		}
		if cState.DriveId != "" {
			return fmt.Errorf("Container %s is already in a shared drive (%s); it looks like it was already dragged to the dropoff folder — run unpack instead", containerF.Id, cState.DriveId)
		}
		if ownedByAccount(cState, account) {
			fmt.Fprintf(os.Stderr, "Container ownership: already owned by %s.\n", account)
		} else {
			owner := "(unknown)"
			if len(cState.Owners) > 0 {
				owner = cState.Owners[0].EmailAddress
			}
			fmt.Fprintf(os.Stderr, "Container ownership: currently %s — transfer it to %s (invite + accept, via the Drive UI) before the drag.\n", owner, account)
		}
		if err := upsertUserMigration(db, account, userFolder.Id, containerF.Id, stashF.Id); err != nil {
			return fmt.Errorf("recording the migration: %w", err)
		}
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "DRY RUN: no changes will be made. Would pack %d item(s) owned by %q (%d owned subtree root(s)).\n", len(owned), account, len(roots))
	} else {
		fmt.Fprintf(os.Stderr, "About to pack %d item(s) owned by %q (%d owned subtree root(s)) into %s/%s/%s.\n",
			len(owned), account, len(roots), packing.Name, account, containerFolderName(account))
		if !promptYesNo("Continue? [y/N] ") {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return nil
		}
	}

	var movedToContainer, sweptToStash, alreadyPacked, skipped, failed int
	// fail logs one item failure and aborts the run once the error budget is
	// spent — a burst of failures means something systemic (wrong account,
	// wrong config, revoked access), not per-item drift.
	fail := func(format string, args ...any) error {
		log.Printf(format, args...)
		failed++
		if failed > maxErrors {
			return fmt.Errorf("aborting after %d failures (--max-errors %d); fix the cause and re-run pack", failed, maxErrors)
		}
		return nil
	}
	warnExtraParents := func(id, name string) {
		if extraParents[id] {
			log.Printf("WARN %q (%s) has extra parents recorded in the database; the round trip keeps only the traversal parent (see the extra_parents table)", name, id)
		}
	}

	// Phase A: move each owned root into the Container. Moves are optimistic —
	// one files.update per item, removing the parent the crawl recorded. If
	// that parent is stale the update fails loudly (Drive items have a single
	// parent), and only then do we fetch the item's live state to diagnose.
	prog := newProgress()
	for _, r := range roots {
		if err := ctx.Err(); err != nil {
			log.Printf("interrupted: %d moved to Container, %d swept to Stash, %d failed", movedToContainer, sweptToStash, failed)
			return err
		}
		prog.tick("progress: %d/%d moved to Container", movedToContainer, len(roots))
		warnExtraParents(r.driveID, r.name)
		if dryRun {
			log.Printf("WOULD move %s %q (%s) from %s into the Container", r.typ, r.name, r.driveID, r.parentDriveID)
			movedToContainer++
			continue
		}
		err := moveFile(ctx, svc, limiter, r.driveID, containerF.Id, r.parentDriveID)
		if err == nil {
			detailf("OK %s %q (%s) -> Container", r.typ, r.name, r.driveID)
			movedToContainer++
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Diagnose this one item: gone, trashed, already packed, ownership
		// changed, or just a stale parent to retry from the live one.
		f, gerr := getFileState(ctx, svc, limiter, r.driveID)
		switch {
		case isNotFound(gerr):
			log.Printf("SKIP %q (%s): no longer exists", r.name, r.driveID)
			skipped++
		case gerr != nil:
			if err := fail("ERROR %q (%s): move failed (%v) and live lookup failed (%v)", r.name, r.driveID, err, gerr); err != nil {
				return err
			}
		case f.Trashed:
			log.Printf("SKIP %q (%s): trashed since the crawl", r.name, r.driveID)
			skipped++
		case hasParent(f, containerF.Id):
			detailf("OK %s %q (%s): already in the Container", r.typ, r.name, r.driveID)
			alreadyPacked++
		case !ownedByAccount(f, account):
			log.Printf("SKIP %q (%s): no longer owned by %s; leaving it in place", r.name, r.driveID, account)
			skipped++
		default:
			if merr := moveFile(ctx, svc, limiter, r.driveID, containerF.Id, strings.Join(f.Parents, ",")); merr != nil {
				if err := fail("ERROR moving %q (%s) into the Container: %v", r.name, r.driveID, merr); err != nil {
					return err
				}
			} else {
				// Its recorded parent was stale — expected, since packing moves
				// roots out from under one another. Retrying from the live parent
				// (above) is the normal path, so this is not worth a warning.
				detailf("OK %s %q (%s) -> Container (from live parent %v)", r.typ, r.name, r.driveID, f.Parents)
				movedToContainer++
			}
		}
	}

	// Phase B: sweep the live Container tree. Everything the user does not own
	// goes to the flat Stash — a shared drive cannot hold third-party-owned
	// items, so anything left would block the drag. Working from live listings
	// (rather than the database) also catches items created or re-owned after
	// the crawl; the sweep doubles as the pre-drag ownership verification.
	// seen collects every user-owned item observed in the tree for Phase C.
	seen := make(map[string]bool)
	if dryRun {
		preview, err := unownedChildrenOfOwned(db, account)
		if err != nil {
			return err
		}
		for _, s := range preview {
			log.Printf("WOULD move %q (%s) out of owned folder %s into the Stash", s.name, s.driveID, s.parentDriveID)
			sweptToStash++
		}
		log.Printf("NOTE the real sweep works from live Drive listings, so it also catches items the crawl never saw")
	} else {
		queue := []string{containerF.Id}
		for len(queue) > 0 {
			if err := ctx.Err(); err != nil {
				log.Printf("interrupted: %d moved to Container, %d swept to Stash, %d failed", movedToContainer, sweptToStash, failed)
				return err
			}
			folderID := queue[0]
			queue = queue[1:]
			children, err := listChildren(ctx, svc, limiter, folderID,
				"nextPageToken, files(id, name, mimeType, owners(emailAddress, permissionId))")
			if err != nil {
				return fmt.Errorf("sweeping Container tree (listing %s): %w", folderID, err)
			}
			for _, c := range children {
				if ownedByAccount(c, account) {
					seen[c.Id] = true
					if c.MimeType == folderMimeType {
						queue = append(queue, c.Id)
					}
					continue
				}
				if _, derr := nodeTypeByDriveID(db, c.Id); derr == sql.ErrNoRows {
					log.Printf("WARN not in database: %q (%s), swept from %s — if unpack cannot place it, it lands in %s/%s for manual restore",
						c.Name, c.Id, folderID, errorsFolderName, folderID)
					if err := recordPackOrphan(db, account, c.Id, folderID); err != nil {
						return fmt.Errorf("recording orphan %s: %w", c.Id, err)
					}
				} else if derr != nil {
					return derr
				}
				warnExtraParents(c.Id, c.Name)
				if merr := moveFile(ctx, svc, limiter, c.Id, stashF.Id, folderID); merr != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if err := fail("ERROR sweeping %q (%s) into the Stash: %v", c.Name, c.Id, merr); err != nil {
						return err
					}
					continue
				}
				detailf("OK swept %q (%s) -> Stash", c.Name, c.Id)
				sweptToStash++
			}
		}
	}

	// Phase C: stragglers — items the database says the user owns that never
	// showed up inside the Container tree (usually items that moved since the
	// crawl, so their owned ancestor no longer carries them). Costs no API
	// calls unless something is actually missing.
	if !dryRun {
		for _, n := range owned {
			if seen[n.driveID] || n.driveID == crawlRoot {
				continue
			}
			if err := ctx.Err(); err != nil {
				log.Printf("interrupted: %d moved to Container, %d swept to Stash, %d failed", movedToContainer, sweptToStash, failed)
				return err
			}
			f, gerr := getFileState(ctx, svc, limiter, n.driveID)
			switch {
			case isNotFound(gerr):
				detailf("SKIP straggler check %q (%s): no longer exists", n.name, n.driveID)
			case gerr != nil:
				if err := fail("ERROR straggler check %q (%s): %v", n.name, n.driveID, gerr); err != nil {
					return err
				}
			case f.Trashed, hasParent(f, containerF.Id), hasParent(f, stashF.Id):
				// Trashed, or already handled this run (Phase A skips land here too).
			case !ownedByAccount(f, account):
				// Ownership changed since the crawl; nothing of the user's to move.
			default:
				if merr := moveFile(ctx, svc, limiter, n.driveID, containerF.Id, strings.Join(f.Parents, ",")); merr != nil {
					if err := fail("ERROR moving straggler %q (%s) into the Container: %v", n.name, n.driveID, merr); err != nil {
						return err
					}
				} else {
					log.Printf("WARN straggler %q (%s) was outside the packed tree (live parent %v); moved flat into the Container", n.name, n.driveID, f.Parents)
					movedToContainer++
					if f.MimeType == folderMimeType {
						log.Printf("WARN straggler %q (%s) is a folder; re-run pack to sweep any third-party items inside it", n.name, n.driveID)
					}
				}
			}
		}
	}

	verb := "moved"
	if dryRun {
		verb = "would move"
	}
	log.Printf("done: %d item(s) %s to Container (%d already there), %d swept to Stash, %d skipped, %d failed",
		movedToContainer, verb, alreadyPacked, sweptToStash, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d item(s) failed; re-run pack to retry", failed)
	}
	if !dryRun {
		if err := markPacked(db, account); err != nil {
			return fmt.Errorf("recording pack completion: %w", err)
		}
		fmt.Fprintf(os.Stderr, `Pack complete. Next steps:
  1. Transfer ownership of the Container to %[1]s via the Drive UI (invite + accept).
  2. Ask %[1]s to drag the Container into the %[2]q folder (one drag; this flips ownership of everything inside to the org).
  3. Run: drive-cleanup unpack %[1]s
`, account, dropoff.Name)
	}
	return nil
}
