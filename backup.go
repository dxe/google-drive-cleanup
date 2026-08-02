package main

// The backup subcommand: snapshot the SQLite database into db-backups/ with a
// timestamped name and note it in db-backups/log.txt, so the ad-hoc "copy
// drive.db before doing something scary" habit produces consistently named
// files with a record of why each one was taken.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// backupDirName is the directory (alongside the database) backups are written
// to, and logFileName the running record of what each backup was for.
const (
	backupDirName = "db-backups"
	logFileName   = "log.txt"
)

var backupCmd = &cobra.Command{
	Use:   "backup [description]",
	Short: "Copy the database into db-backups/ with a timestamped name",
	Long: `Write a snapshot of the database to db-backups/<name>-<yyyymmdd-hhmm>.db
(local time), next to the database itself, and append a line to
db-backups/log.txt in the form "<filename> - <description>".

The optional description is free text — everything after the subcommand is used,
so it needs no quoting. A backup with no description is still logged.

The copy is taken with SQLite's VACUUM INTO, so it is a consistent single-file
snapshot that includes anything still sitting in the write-ahead log; no
separate -wal/-shm files are needed to restore it. Unlike every other
subcommand, backup does not apply pending schema migrations first: it captures
the database exactly as it is on disk.`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		return runBackup(dbPath, strings.TrimSpace(strings.Join(args, " ")))
	},
}

func runBackup(dbPath, description string) error {
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("database %s: %w", dbPath, err)
	}

	dir := filepath.Join(filepath.Dir(dbPath), backupDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	name, err := backupName(dir, dbPath, time.Now())
	if err != nil {
		return err
	}
	dest := filepath.Join(dir, name)

	if err := vacuumInto(dbPath, dest); err != nil {
		return err
	}

	if err := appendBackupLog(filepath.Join(dir, logFileName), name, description); err != nil {
		// The backup itself is on disk and usable; the log is bookkeeping.
		return fmt.Errorf("backup written to %s, but logging it failed: %w", dest, err)
	}

	size := ""
	if fi, err := os.Stat(dest); err == nil {
		size = fmt.Sprintf(" (%.1f MB)", float64(fi.Size())/(1<<20))
	}
	fmt.Fprintf(os.Stderr, "Backed up %s to %s%s\n", dbPath, dest, size)
	return nil
}

// backupName picks an unused file name in dir for a backup of dbPath taken at
// t: "<stem>-<yyyymmdd-hhmm>.db", with a "-2", "-3", … suffix if a backup was
// already taken in the same minute (VACUUM INTO refuses to overwrite).
func backupName(dir, dbPath string, t time.Time) (string, error) {
	base := filepath.Base(dbPath)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	prefix := fmt.Sprintf("%s-%s", stem, t.Format("20060102-1504"))
	for i := 1; i <= 100; i++ {
		name := prefix + ".db"
		if i > 1 {
			name = fmt.Sprintf("%s-%d.db", prefix, i)
		}
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			return name, nil
		}
	}
	return "", fmt.Errorf("too many backups already taken in %s at %s", dir, prefix)
}

// vacuumInto writes a consistent snapshot of the database at src to dest.
// It deliberately bypasses openDB: a backup must not migrate the schema of the
// database it is about to copy.
func vacuumInto(src, dest string) error {
	db, err := sql.Open("sqlite", src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout = 10000"); err != nil {
		return fmt.Errorf("PRAGMA busy_timeout: %w", err)
	}
	if _, err := db.Exec("VACUUM INTO ?", dest); err != nil {
		return fmt.Errorf("copying %s to %s: %w", src, dest, err)
	}
	return nil
}

// appendBackupLog adds one "<filename> - <description>" line to the log,
// creating it if needed.
func appendBackupLog(path, name, description string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// Keep the separator even when there is no description, so the format is
	// uniform and a description can be filled in later by hand.
	if _, err := fmt.Fprintf(f, "%s - %s\n", name, description); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
