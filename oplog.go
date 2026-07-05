package main

import (
	"context"
	"database/sql"
	"log"

	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
)

// opLog records every Google Drive write into the drive_ops table, tagged with
// the migration (account) and command (pack/unpack) that issued it. Its methods
// mirror the free functions in driveops.go one-for-one and simply wrap them:
// call sites gain logging by prefixing the call with the recorder, and the
// underlying Drive logic stays in driveops.go. Logging is best-effort -- a
// failed insert is logged and swallowed so the audit log can never abort a
// migration. Only real writes reach here; dry-run paths never call these.
type opLog struct {
	db      *sql.DB
	account string // the migration, matching user_migrations.account
	command string // "pack" or "unpack"
}

// record writes one drive_ops row. err distinguishes ok from error; startedAt
// is captured by the caller just before the Drive call so the row brackets the
// operation's real duration.
func (l *opLog) record(operation, itemID, itemName, fromParent, toParent, detail, startedAt string, err error) {
	status := "ok"
	var errMsg any
	if err != nil {
		status = "error"
		errMsg = err.Error()
	}
	if _, dberr := l.db.Exec(`
		INSERT INTO drive_ops
			(account, command, operation, item_id, item_name, from_parent, to_parent, detail, status, error, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.account, l.command, operation,
		nullIfEmpty(itemID), nullIfEmpty(itemName), nullIfEmpty(fromParent), nullIfEmpty(toParent), nullIfEmpty(detail),
		status, errMsg, startedAt, now(),
	); dberr != nil {
		// The audit log must never break a migration; note it and move on.
		log.Printf("WARN failed to record %s of %s in drive_ops: %v", operation, itemID, dberr)
	}
}

// nullIfEmpty maps "" to a SQL NULL so empty columns read as NULL rather than
// blank strings, keeping the log easy to query.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (l *opLog) createFolder(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, parentID, name string) (*drive.File, error) {
	started := now()
	f, err := createFolder(ctx, svc, limiter, parentID, name)
	id := ""
	if f != nil {
		id = f.Id
	}
	l.record("create_folder", id, name, "", parentID, "", started, err)
	return f, err
}

func (l *opLog) moveFile(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, addParent, removeParent string) error {
	started := now()
	err := moveFile(ctx, svc, limiter, fileID, addParent, removeParent)
	l.record("move", fileID, "", removeParent, addParent, "", started, err)
	return err
}

func (l *opLog) moveFileVerified(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, addParent, removeParent string) error {
	started := now()
	err := moveFileVerified(ctx, svc, limiter, fileID, addParent, removeParent)
	l.record("move_verified", fileID, "", removeParent, addParent, "", started, err)
	return err
}

func (l *opLog) deleteFile(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID string) error {
	started := now()
	err := deleteFile(ctx, svc, limiter, fileID)
	l.record("delete", fileID, "", "", "", "", started, err)
	return err
}

func (l *opLog) grantPermission(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, email, role string) error {
	started := now()
	err := grantPermission(ctx, svc, limiter, fileID, email, role)
	l.record("grant_permission", fileID, "", "", "", email+" "+role, started, err)
	return err
}

func (l *opLog) revokePermission(ctx context.Context, svc *drive.Service, limiter *rate.Limiter, fileID, permissionID string) error {
	started := now()
	err := revokePermission(ctx, svc, limiter, fileID, permissionID)
	l.record("revoke_permission", fileID, "", "", "", "permission "+permissionID, started, err)
	return err
}
