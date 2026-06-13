package main

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
)

var restoreCmd = &cobra.Command{
	Use:   "restore-locations",
	Short: "Move files from the staging folder back to their original locations",
	Long: `Scans the configured staging-folder one level deep, looks up each file's
original parent in the database, and moves it there. Files not found in the
database are skipped with a warning.

Owners drag their files into the staging folder (a shared drive) to transfer
ownership to the org account. Once that transfer is done, run this command to
move each file back to the folder it lived in before the transfer.

This command requires the full Drive scope. If the cached token.json only has
read-only access (e.g. from crawl/owners), the tool re-runs consent
automatically to obtain it.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		return runRestoreLocations(dbPath, cfgPath, dryRun)
	},
}

func init() {
	restoreCmd.Flags().Bool("dry-run", false, "report what would move without changing anything (read-only scope)")
}

func runRestoreLocations(dbPath, cfgPath string, dryRun bool) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	staging := cfg.RestoreLocations.StagingFolder
	if err := staging.validate("restore-locations.staging-folder"); err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		signal.Stop(sigCh)
		cancel()
	}()

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

	if dryRun {
		fmt.Fprintf(os.Stderr, "DRY RUN: no files will be moved. Restored files would be owned by: %s.\n", me.EmailAddress)
	} else {
		fmt.Fprintf(os.Stderr, "Restored files will be owned by: %s. Continue? [y/N] ", me.EmailAddress)
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		if strings.ToLower(strings.TrimSpace(scanner.Text())) != "y" {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return nil
		}
	}

	// Verify the staging folder name matches config, guarding against a stale id.
	folder, err := svc.Files.Get(staging.ID).
		Fields("id, name, mimeType").
		SupportsAllDrives(true).
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("fetching staging folder %s: %w", staging.ID, err)
	}
	if folder.Name != staging.Name {
		return fmt.Errorf("staging folder name mismatch: config says %q, Drive says %q", staging.Name, folder.Name)
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	limiter := rate.NewLimiter(rate.Limit(3), 3)

	var moved, skipped, failed int
	pageToken := ""
	for {
		if err := ctx.Err(); err != nil {
			log.Printf("interrupted: %d moved, %d skipped, %d failed", moved, skipped, failed)
			return err
		}

		if err := limiter.Wait(ctx); err != nil {
			return err
		}
		call := svc.Files.List().
			Q(fmt.Sprintf("'%s' in parents and trashed = false", staging.ID)).
			Fields("nextPageToken, files(id, name)").
			SupportsAllDrives(true).IncludeItemsFromAllDrives(true).
			PageSize(100)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		list, err := call.Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("listing staging folder: %w", err)
		}

		for _, file := range list.Files {
			if err := ctx.Err(); err != nil {
				log.Printf("interrupted: %d moved, %d skipped, %d failed", moved, skipped, failed)
				return err
			}

			parentDriveID, err := originalParentDriveID(db, file.Id)
			if err == sql.ErrNoRows {
				detailf("SKIP %s (%s): not found in database", file.Name, file.Id)
				skipped++
				continue
			}
			if err != nil {
				log.Printf("ERROR %s (%s): database lookup: %v", file.Name, file.Id, err)
				failed++
				continue
			}

			if dryRun {
				log.Printf("WOULD move %s (%s) -> parent %s", file.Name, file.Id, parentDriveID)
				moved++
				continue
			}

			if err := limiter.Wait(ctx); err != nil {
				return err
			}
			_, err = svc.Files.Update(file.Id, nil).
				AddParents(parentDriveID).
				RemoveParents(staging.ID).
				SupportsAllDrives(true).
				Fields("id").
				Context(ctx).Do()
			if err != nil {
				log.Printf("ERROR moving %s (%s) to parent %s: %v", file.Name, file.Id, parentDriveID, err)
				failed++
				continue
			}
			detailf("OK %s (%s) -> parent %s", file.Name, file.Id, parentDriveID)
			if err := updateNodeOwner(db, file.Id, me.EmailAddress, me.PermissionId, me.DisplayName); err != nil {
				log.Printf("WARN could not update owner in DB for %s (%s): %v", file.Name, file.Id, err)
			}
			moved++
		}

		pageToken = list.NextPageToken
		if pageToken == "" {
			break
		}
	}

	verb := "moved"
	if dryRun {
		verb = "would move"
	}
	log.Printf("done: %d %s, %d skipped (not in DB), %d failed", moved, verb, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d file(s) failed to move", failed)
	}
	return nil
}
