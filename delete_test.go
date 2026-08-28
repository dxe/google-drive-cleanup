package main

import (
	"database/sql"
	"strings"
	"testing"
)

// replicaTree builds the archive side of buildArchiveTree: replicas of A and B
// (B's nested inside A's, as on Drive) with A's cached on its original folder
// and B's orphaned — the state delete leaves behind when it deletes folder B
// itself, taking the row that pointed at B's replica with it.
func replicaTree(t *testing.T, db *sql.DB) (archID int64) {
	t.Helper()
	_, archID = buildArchiveTree(t, db)
	replicaA, err := upsertReplicaRow(db, "archA", "ARCH A", sql.NullInt64{Int64: archID, Valid: true}, "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertReplicaRow(db, "archB", "ARCH B", sql.NullInt64{Int64: replicaA, Valid: true}, "me@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := setArchiveFolder(db, "A", "archA"); err != nil {
		t.Fatal(err)
	}
	if err := deleteNodeRow(db, "B"); err != nil {
		t.Fatal(err)
	}
	return archID
}

func TestOrphanedReplicaFolders(t *testing.T) {
	db := testDB(t)
	replicaTree(t, db)

	orphans, err := orphanedReplicaFolders(db, "arch")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range orphans {
		got = append(got, o.driveID)
	}
	// Only archB: archA is still pointed at by folder A, and the archive root
	// itself has no parent row.
	if strings.Join(got, ",") != "archB" {
		t.Fatalf("orphanedReplicaFolders = %v, want [archB]", got)
	}

	// An archived folder that merely looks like a replica keeps its contents:
	// it records where it came from, so it is a real item, not a shell.
	if err := markArchived(db, "C", "root", mustRowID(t, db, "archA")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE nodes SET name = 'ARCH C' WHERE drive_id = 'C'`); err != nil {
		t.Fatal(err)
	}
	if orphans, err = orphanedReplicaFolders(db, "arch"); err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].driveID != "archB" {
		t.Fatalf("orphanedReplicaFolders with an ARCH-named archived folder = %v, want just archB", orphans)
	}

	// So does a shell someone marked keep in the review UI.
	setDecision(t, db, "archB", decisionKeep)
	if orphans, err = orphanedReplicaFolders(db, "arch"); err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Errorf("orphanedReplicaFolders after marking archB keep = %v, want none", orphans)
	}
}

func TestReplicasToPruneDeepestFirst(t *testing.T) {
	db := testDB(t)
	replicaTree(t, db)

	prunes, err := replicasToPrune(db, "arch")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, p := range prunes {
		order = append(order, p.replicaID+":"+p.originalID)
	}
	// archB (depth 2, orphaned so no original to un-cache) must come before
	// archA (depth 1) that nests it, or the parent could never read as empty.
	if strings.Join(order, ",") != "archB:,archA:A" {
		t.Errorf("replicasToPrune = %v, want archB before archA", order)
	}
}

func mustRowID(t *testing.T, db *sql.DB, driveID string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM nodes WHERE drive_id = ?`, driveID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
