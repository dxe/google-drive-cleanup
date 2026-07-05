package main

import (
	"errors"
	"testing"
)

func TestOpLogRecord(t *testing.T) {
	db := testDB(t)
	l := &opLog{db: db, account: "user@example.com", command: "pack"}

	l.record("move", "file123", "", "parentA", "parentB", "", now(), nil)
	l.record("move", "file456", "", "parentC", "parentD", "", now(), errors.New("boom"))
	l.record("create_folder", "folder789", "Errors", "", "parentE", "", now(), nil)

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drive_ops WHERE account = ? AND command = ?`,
		"user@example.com", "pack").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("expected 3 rows, got %d", n)
	}

	var op, itemID, fromP, toP, status, errMsg string
	if err := db.QueryRow(`SELECT operation, item_id, from_parent, to_parent, status, error
		FROM drive_ops WHERE item_id = 'file456'`).Scan(&op, &itemID, &fromP, &toP, &status, &errMsg); err != nil {
		t.Fatal(err)
	}
	if op != "move" || fromP != "parentC" || toP != "parentD" || status != "error" || errMsg != "boom" {
		t.Fatalf("failed-move row wrong: op=%s from=%s to=%s status=%s err=%s", op, fromP, toP, status, errMsg)
	}

	// Empty strings must land as NULL, not "".
	var emptyCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drive_ops WHERE item_id = 'file123' AND item_name IS NULL AND error IS NULL`).Scan(&emptyCount); err != nil {
		t.Fatal(err)
	}
	if emptyCount != 1 {
		t.Fatalf("expected empty fields to be NULL, got %d matching rows", emptyCount)
	}
}
