package main

import (
	"database/sql"
	"testing"
)

// buildReviewTree builds the fixture used by the decision-marking tests:
//
//	Root ─┬─ A ─┬─ a1.pdf
//	      │     ├─ a2.pdf
//	      │     └─ B ── b1.pdf
//	      └─ C ── c1.pdf
func buildReviewTree(t *testing.T, db *sql.DB) {
	t.Helper()
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType})
	aID, _, _, _ := mustUpsert(t, db, node{driveID: "A", name: "A", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "a1", name: "a1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: aID, Valid: true}})
	mustUpsert(t, db, node{driveID: "a2", name: "a2.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: aID, Valid: true}})
	bID, _, _, _ := mustUpsert(t, db, node{driveID: "B", name: "B", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: aID, Valid: true}})
	mustUpsert(t, db, node{driveID: "b1", name: "b1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: bID, Valid: true}})
	cID, _, _, _ := mustUpsert(t, db, node{driveID: "C", name: "C", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "c1", name: "c1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: cID, Valid: true}})
}

func decisionsByDriveID(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT drive_id, decision FROM nodes`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, d string
		if err := rows.Scan(&id, &d); err != nil {
			t.Fatal(err)
		}
		out[id] = d
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustMark(t *testing.T, s *reviewServer, driveID, decision, onConflict string) markResult {
	t.Helper()
	res, err := s.applyMark(markRequest{DriveID: driveID, Decision: decision, OnConflict: onConflict})
	if err != nil {
		t.Fatalf("mark %s %s: %v", driveID, decision, err)
	}
	return res
}

func wantDecisions(t *testing.T, db *sql.DB, want map[string]string) {
	t.Helper()
	got := decisionsByDriveID(t, db)
	for id, w := range want {
		if got[id] != w {
			t.Errorf("decision[%s] = %q, want %q", id, got[id], w)
		}
	}
}

// Marking a folder delete propagates to every descendant; once the root's
// other subtree is deleted too, the rollup auto-deletes the root.
func TestMarkFolderDeletePropagatesAndRollsUp(t *testing.T) {
	db := testDB(t)
	buildReviewTree(t, db)
	s := &reviewServer{db: db}

	res := mustMark(t, s, "A", decisionDelete, "")
	if res.NeedsConfirm {
		t.Fatal("delete of an all-undecided folder should not need confirmation")
	}
	wantDecisions(t, db, map[string]string{
		"A": "delete", "a1": "delete", "a2": "delete", "B": "delete", "b1": "delete",
		"root": "", "C": "", "c1": "",
	})

	mustMark(t, s, "C", decisionDelete, "")
	// All of root's children are now delete subtrees, so root auto-deletes.
	wantDecisions(t, db, map[string]string{"C": "delete", "c1": "delete", "root": "delete"})
}

// Marking a folder keep when everything below is undecided marks the whole
// subtree keep; mixed children roll parents up to keep, not delete.
func TestMarkFolderKeepPropagatesAndRollsUp(t *testing.T) {
	db := testDB(t)
	buildReviewTree(t, db)
	s := &reviewServer{db: db}

	mustMark(t, s, "A", decisionKeep, "")
	wantDecisions(t, db, map[string]string{
		"A": "keep", "a1": "keep", "a2": "keep", "B": "keep", "b1": "keep",
		"root": "", "C": "",
	})

	mustMark(t, s, "C", decisionDelete, "")
	// Root's children are decided but not all delete: rollup marks it keep.
	wantDecisions(t, db, map[string]string{"C": "delete", "c1": "delete", "root": "keep"})
}

// Keeping a folder with delete descendants prompts; "preserve" leaves the
// delete subtree alone, "overwrite" flips everything to keep.
func TestMarkFolderKeepConflict(t *testing.T) {
	db := testDB(t)
	buildReviewTree(t, db)
	s := &reviewServer{db: db}

	mustMark(t, s, "B", decisionDelete, "")
	res := mustMark(t, s, "A", decisionKeep, "")
	if !res.NeedsConfirm || res.ConflictDeletes != 2 {
		t.Fatalf("keep over delete subtree: res = %+v, want needsConfirm with 2 delete descendants", res)
	}
	// Nothing may have been written by the conflicting attempt.
	wantDecisions(t, db, map[string]string{"A": "", "a1": "", "B": "delete", "b1": "delete"})

	res = mustMark(t, s, "A", decisionKeep, "preserve")
	if res.NeedsConfirm {
		t.Fatal("preserve should resolve the conflict")
	}
	wantDecisions(t, db, map[string]string{
		"A": "keep", "a1": "keep", "a2": "keep", "B": "delete", "b1": "delete",
	})

	mustMark(t, s, "A", decisionKeep, "overwrite")
	wantDecisions(t, db, map[string]string{"A": "keep", "B": "keep", "b1": "keep"})
}

// Deleting a folder with keep descendants prompts; confirming overwrites them.
func TestMarkFolderDeleteConflict(t *testing.T) {
	db := testDB(t)
	buildReviewTree(t, db)
	s := &reviewServer{db: db}

	mustMark(t, s, "b1", decisionKeep, "")
	res := mustMark(t, s, "A", decisionDelete, "")
	if !res.NeedsConfirm || res.ConflictKeeps < 1 {
		t.Fatalf("delete over keep descendants: res = %+v, want needsConfirm", res)
	}
	wantDecisions(t, db, map[string]string{"A": "", "b1": "keep"})

	mustMark(t, s, "A", decisionDelete, "overwrite")
	wantDecisions(t, db, map[string]string{
		"A": "delete", "a1": "delete", "a2": "delete", "B": "delete", "b1": "delete",
	})
}

// Marking a file keep inside a delete subtree clears the delete ancestors
// (nothing kept may sit inside a deleted folder) and re-decides them from
// their children: they become keep (mixed content).
func TestKeepInsideDeleteSubtreeClearsAncestors(t *testing.T) {
	db := testDB(t)
	buildReviewTree(t, db)
	s := &reviewServer{db: db}

	mustMark(t, s, "A", decisionDelete, "")
	res := mustMark(t, s, "b1", decisionKeep, "")
	if res.ClearedAncestors != 2 {
		t.Errorf("clearedAncestors = %d, want 2 (B and A)", res.ClearedAncestors)
	}
	wantDecisions(t, db, map[string]string{
		"b1": "keep",
		// B's only child is keep -> rollup keeps B; A's children a1/a2 delete +
		// B keep -> mixed -> keep.
		"B": "keep", "A": "keep",
		"a1": "delete", "a2": "delete",
	})
}

// Marking files one by one rolls the parent folder up automatically once the
// last sibling is decided, and cascades upward.
func TestFileMarksRollUpFolders(t *testing.T) {
	db := testDB(t)
	buildReviewTree(t, db)
	s := &reviewServer{db: db}

	mustMark(t, s, "b1", decisionDelete, "")
	wantDecisions(t, db, map[string]string{"B": "delete"}) // only child decided -> auto

	mustMark(t, s, "a1", decisionDelete, "")
	wantDecisions(t, db, map[string]string{"A": ""}) // a2 still undecided

	mustMark(t, s, "a2", decisionDelete, "")
	wantDecisions(t, db, map[string]string{"A": "delete", "root": ""}) // C undecided
}

// Clearing a folder resets its whole subtree to undecided.
func TestClearFolderClearsSubtree(t *testing.T) {
	db := testDB(t)
	buildReviewTree(t, db)
	s := &reviewServer{db: db}

	mustMark(t, s, "A", decisionDelete, "")
	mustMark(t, s, "A", decisionNone, "")
	wantDecisions(t, db, map[string]string{"A": "", "a1": "", "a2": "", "B": "", "b1": ""})
}

// mark-many bulk-marks files in one undo entry and rolls up their parent.
func TestMarkMany(t *testing.T) {
	db := testDB(t)
	buildReviewTree(t, db)
	s := &reviewServer{db: db}

	res, err := s.applyMarkMany(markManyRequest{DriveIDs: []string{"a1", "a2"}, Decision: decisionDelete})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed != 2 {
		t.Errorf("changed = %d, want 2 (B undecided keeps A undecided)", res.Changed)
	}
	wantDecisions(t, db, map[string]string{"a1": "delete", "a2": "delete", "A": ""})

	// Deciding the last sibling folder completes A.
	mustMark(t, s, "B", decisionDelete, "")
	wantDecisions(t, db, map[string]string{"A": "delete"})

	if len(s.undo) != 2 {
		t.Fatalf("undo stack = %d entries, want 2", len(s.undo))
	}
}

// Undo restores the exact previous state of the last action, including
// rollup and ancestor-clearing side effects, and pops entries in order.
func TestUndoRestoresState(t *testing.T) {
	db := testDB(t)
	buildReviewTree(t, db)
	s := &reviewServer{db: db}

	mustMark(t, s, "A", decisionDelete, "")
	before := decisionsByDriveID(t, db)

	mustMark(t, s, "b1", decisionKeep, "") // clears + re-rolls A and B
	res, err := s.applyUndo()
	if err != nil {
		t.Fatal(err)
	}
	if res.Undone == "" || res.Changed == 0 {
		t.Errorf("undo response = %+v, want label and changed > 0", res)
	}
	after := decisionsByDriveID(t, db)
	for id, want := range before {
		if after[id] != want {
			t.Errorf("after undo, decision[%s] = %q, want %q", id, after[id], want)
		}
	}

	// Second undo reverts the original delete of A.
	if _, err := s.applyUndo(); err != nil {
		t.Fatal(err)
	}
	wantDecisions(t, db, map[string]string{"A": "", "a1": "", "a2": "", "B": "", "b1": ""})
	if len(s.undo) != 0 {
		t.Errorf("undo stack = %d entries, want 0", len(s.undo))
	}
}

// A conflicting attempt that needs confirmation must not create an undo entry.
func TestConflictLeavesNoUndoEntry(t *testing.T) {
	db := testDB(t)
	buildReviewTree(t, db)
	s := &reviewServer{db: db}

	mustMark(t, s, "b1", decisionKeep, "")
	entries := len(s.undo)
	res := mustMark(t, s, "A", decisionDelete, "")
	if !res.NeedsConfirm {
		t.Fatal("expected confirmation request")
	}
	if len(s.undo) != entries {
		t.Errorf("undo stack grew to %d on a needs-confirm attempt, want %d", len(s.undo), entries)
	}
}

// The forest loader's tallies drive both the tree endpoint and export colors.
func TestReviewForestCountsAndStatus(t *testing.T) {
	db := testDB(t)
	buildReviewTree(t, db)
	s := &reviewServer{db: db}
	mustMark(t, s, "B", decisionDelete, "")
	mustMark(t, s, "a1", decisionKeep, "")

	roots, err := loadReviewForest(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0].driveID != "root" {
		t.Fatalf("roots = %v, want just root", roots)
	}
	byID := map[string]*reviewNode{}
	var walk func(n *reviewNode)
	walk = func(n *reviewNode) {
		byID[n.driveID] = n
		for _, c := range n.children {
			walk(c)
		}
	}
	walk(roots[0])

	a := byID["A"]
	if a.subtree.Keep != 1 || a.subtree.Delete != 2 || a.subtree.Undecided != 2 {
		t.Errorf("A subtree = %+v, want 1 keep / 2 delete / 2 undecided", a.subtree)
	}
	if a.directFiles.Keep != 1 || a.directFiles.Undecided != 1 {
		t.Errorf("A direct files = %+v, want 1 keep / 1 undecided", a.directFiles)
	}
	if got := reviewStatus(a.subtree); got != "mixed" {
		t.Errorf("A status = %q, want mixed", got)
	}
	if got := reviewStatus(byID["B"].subtree); got != "delete" {
		t.Errorf("B status = %q, want delete", got)
	}
	if got := reviewStatus(byID["C"].subtree); got != "todo" {
		t.Errorf("C status = %q, want todo", got)
	}
	if got := reviewStatus(byID["root"].subtree); got != "mixed" {
		t.Errorf("root status = %q, want mixed", got)
	}
}
