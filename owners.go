package main

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var ownersCmd = &cobra.Command{
	Use:   "owners",
	Short: "Print each owner and how many files (non-folders) they own",
	Long: `Print each owner and how many files (non-folders) they own, sorted
descending by count — this drives outreach priority.

With --folder, counts are limited to that Google Drive folder and its
descendants (the folder must be one crawled into the database); without it, the
whole database is counted.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		folderID, _ := cmd.Flags().GetString("folder")
		return runOwners(dbPath, cfgPath, folderID)
	},
}

func init() {
	ownersCmd.Flags().String("folder", "", "Google Drive folder ID to scope the report to (must be crawled into the database)")
}

func runOwners(dbPath, cfgPath, parentID string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if parentID != "" {
		typ, err := nodeTypeByDriveID(db, parentID)
		if err == sql.ErrNoRows {
			return fmt.Errorf("parent folder %s not found in the database; crawl it first", parentID)
		}
		if err != nil {
			return err
		}
		if typ != typeFolder {
			return fmt.Errorf("%s is a %s, not a folder", parentID, typ)
		}
	}

	counts, err := ownersReport(db, parentID)
	if err != nil {
		return err
	}
	fmt.Printf("%10s %10s %10s  %s\n", "FOLDERS", "FILES", "TOTAL", "OWNER")
	for _, oc := range counts {
		if cfg.Owners.IgnoreInternalDomains && isInternalEmail(oc.email, cfg.InternalDomains) {
			continue
		}
		fmt.Printf("%10d %10d %10d  %s\n", oc.folderCount, oc.fileCount, oc.total, ownerLabel(oc))
	}
	return nil
}

// isInternalEmail reports whether email is non-null and ends with "@" followed
// by one of the internal domains (case-insensitive).
func isInternalEmail(email sql.NullString, internalDomains []string) bool {
	if !email.Valid {
		return false
	}
	at := strings.LastIndex(email.String, "@")
	if at < 0 {
		return false
	}
	domain := strings.ToLower(email.String[at+1:])
	for _, d := range internalDomains {
		if domain == strings.ToLower(d) {
			return true
		}
	}
	return false
}

func ownerLabel(oc ownerCount) string {
	switch {
	case oc.email.Valid:
		if oc.displayName.Valid {
			return fmt.Sprintf("%s (%s)", oc.email.String, oc.displayName.String)
		}
		return oc.email.String
	case oc.ownerID.Valid:
		// No email on these rows; the stable Drive user id is all we have, so
		// include the display name to make it human-readable.
		if oc.displayName.Valid {
			return fmt.Sprintf("id:%s (%s)", oc.ownerID.String, oc.displayName.String)
		}
		return "id:" + oc.ownerID.String
	default:
		return "(unknown)"
	}
}

type ownerCount struct {
	email       sql.NullString
	ownerID     sql.NullString
	displayName sql.NullString
	folderCount int64
	fileCount   int64
	total       int64
}

// ownersReport counts nodes per owner, split into folders and files, grouped by
// owner_email, falling back to owner_id when the email is missing, and to a
// single "(unknown)" bucket when both are NULL. Rows are ordered by file count
// descending.
//
// If parentDriveID is non-empty, only the folder with that Drive ID and its
// descendants are counted (walking parent_id downwards); an empty string counts
// the whole database.
func ownersReport(db *sql.DB, parentDriveID string) ([]ownerCount, error) {
	scope := "nodes"
	var args []any
	if parentDriveID != "" {
		// Restrict to the folder and everything beneath it. The recursive CTE
		// seeds on the folder's row and walks parent_id downwards; we then count
		// over just those rows instead of the whole table.
		scope = `(
			WITH RECURSIVE subtree(id) AS (
				SELECT id FROM nodes WHERE drive_id = ?
				UNION ALL
				SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
			)
			SELECT nodes.* FROM nodes JOIN subtree ON nodes.id = subtree.id
		)`
		args = append(args, parentDriveID)
	}
	rows, err := db.Query(fmt.Sprintf(`
		SELECT MAX(owner_email) AS email, MAX(owner_id) AS oid, MAX(owner_display_name),
			SUM(type = '%[1]s') AS folders,
			SUM(type <> '%[1]s') AS files,
			COUNT(*) AS total
		FROM %[2]s
		GROUP BY COALESCE(owner_email, owner_id, '(unknown)')
		ORDER BY files DESC, total DESC, (email IS NULL AND oid IS NULL), COALESCE(email, oid)`, typeFolder, scope), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var counts []ownerCount
	for rows.Next() {
		var oc ownerCount
		if err := rows.Scan(&oc.email, &oc.ownerID, &oc.displayName, &oc.folderCount, &oc.fileCount, &oc.total); err != nil {
			return nil, err
		}
		counts = append(counts, oc)
	}
	return counts, rows.Err()
}
