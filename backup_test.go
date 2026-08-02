package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRunBackup covers the whole subcommand against a real database file: the
// snapshot must be a usable copy, and the log must gain one line per run —
// including for a backup taken without a description.
func TestRunBackup(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "drive.db")

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	mustUpsert(t, db, node{driveID: "abc", name: "Notes", typ: typeBinary, mimeType: "application/pdf"})
	// Leave the database open (and its WAL unchecked-pointed) so the copy has
	// to pick up committed data that a naive file copy could miss.
	defer db.Close()

	if err := runBackup(dbPath, "before deleting things"); err != nil {
		t.Fatal(err)
	}
	// Back-to-back backups land in the same minute; the second must still get
	// its own file rather than failing or overwriting the first.
	if err := runBackup(dbPath, ""); err != nil {
		t.Fatal(err)
	}

	// The log is the record of what happened, so drive the checks from it:
	// one line per backup, "<filename> - <description>", blank description
	// included.
	backupDir := filepath.Join(dir, backupDirName)
	logBytes, err := os.ReadFile(filepath.Join(backupDir, logFileName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(logBytes), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 log lines, got %q", string(logBytes))
	}
	linePattern := regexp.MustCompile(`^(drive-\d{8}-\d{4}(-\d+)?\.db) - (.*)$`)
	var names []string
	for i, want := range []string{"before deleting things", ""} {
		m := linePattern.FindStringSubmatch(lines[i])
		if m == nil {
			t.Fatalf("log line %d = %q, want \"<drive-yyyymmdd-hhmm.db> - %s\"", i, lines[i], want)
		}
		if m[3] != want {
			t.Errorf("log line %d description = %q, want %q", i, m[3], want)
		}
		names = append(names, m[1])
	}
	if names[0] == names[1] {
		t.Fatalf("both backups used the same file name %q", names[0])
	}

	// Every logged name must be a real database file holding the row above.
	for _, name := range names {
		copyDB, err := sql.Open("sqlite", filepath.Join(backupDir, name))
		if err != nil {
			t.Fatal(err)
		}
		var got string
		err = copyDB.QueryRow(`SELECT name FROM nodes WHERE drive_id = 'abc'`).Scan(&got)
		copyDB.Close()
		if err != nil {
			t.Fatalf("reading backup %s: %v", name, err)
		}
		if got != "Notes" {
			t.Errorf("backup %s row = %q, want %q", name, got, "Notes")
		}
	}
}

func TestRunBackupMissingDatabase(t *testing.T) {
	if err := runBackup(filepath.Join(t.TempDir(), "nope.db"), ""); err == nil {
		t.Fatal("expected an error for a database that does not exist")
	}
}
