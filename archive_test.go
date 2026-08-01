package main

import (
	"database/sql"
	"strings"
	"testing"
)

func TestReplicaName(t *testing.T) {
	if got := replicaName("Finance"); got != "ARCH Finance" {
		t.Errorf("replicaName = %q, want %q", got, "ARCH Finance")
	}
	// Truncation is rune-safe and deterministic.
	long := strings.Repeat("é", 300)
	got := replicaName(long)
	if r := []rune(got); len(r) != maxReplicaNameRunes {
		t.Errorf("truncated length = %d runes, want %d", len(r), maxReplicaNameRunes)
	}
	if !strings.HasPrefix(got, archReplicaPrefix) {
		t.Errorf("truncated name lost the prefix: %q", got[:10])
	}
	if again := replicaName(long); again != got {
		t.Error("replicaName is not deterministic for long names")
	}
}

func TestEscapeDriveQuery(t *testing.T) {
	if got := escapeDriveQuery(`Bob's "stuff" \ misc`); got != `Bob\'s "stuff" \\ misc` {
		t.Errorf("escapeDriveQuery = %q", got)
	}
}

// setDecision stamps a decision directly; the propagation logic has its own
// tests in review_test.go.
func setDecision(t *testing.T, db *sql.DB, driveID, decision string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE nodes SET decision = ? WHERE drive_id = ?`, decision, driveID); err != nil {
		t.Fatal(err)
	}
}

// buildArchiveTree builds the fixture shared by the archive query tests:
//
//	root ─┬─ A (delete) ─┬─ a1.pdf (delete)
//	      │              └─ B (delete) ── b1.pdf (delete)
//	      ├─ keep.pdf (keep)
//	      └─ C ── c1.pdf (delete)
//	arch (second root: the archive tree)
//
// Returns the row ids of root and arch.
func buildArchiveTree(t *testing.T, db *sql.DB) (rootID, archID int64) {
	t.Helper()
	rootID, _, _, _ = mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType})
	aID, _, _, _ := mustUpsert(t, db, node{driveID: "A", name: "A", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}, ownerEmail: nullString("me@example.com")})
	mustUpsert(t, db, node{driveID: "a1", name: "a1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: aID, Valid: true}, ownerEmail: nullString("me@example.com")})
	bID, _, _, _ := mustUpsert(t, db, node{driveID: "B", name: "B", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: aID, Valid: true}, ownerEmail: nullString("colleague@example.com")})
	mustUpsert(t, db, node{driveID: "b1", name: "b1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: bID, Valid: true}, ownerEmail: nullString("stranger@other.com")})
	mustUpsert(t, db, node{driveID: "keep", name: "keep.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	cID, _, _, _ := mustUpsert(t, db, node{driveID: "C", name: "C", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "c1", name: "c1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: cID, Valid: true}})
	archID, _, _, _ = mustUpsert(t, db, node{driveID: "arch", name: "Archive root", typ: typeFolder, mimeType: folderMimeType})

	for _, id := range []string{"A", "a1", "B", "b1", "C", "c1"} {
		setDecision(t, db, id, decisionDelete)
	}
	setDecision(t, db, "C", decisionNone) // C is undecided; only its file c1 is delete
	setDecision(t, db, "keep", decisionKeep)
	return rootID, archID
}

func TestArchivableFilesAndFolders(t *testing.T) {
	db := testDB(t)
	buildArchiveTree(t, db)

	files, err := archivableFiles(db, "")
	if err != nil {
		t.Fatal(err)
	}
	gotFiles := map[string]string{}
	for _, f := range files {
		gotFiles[f.driveID] = f.parentDriveID.String
	}
	want := map[string]string{"a1": "A", "b1": "B", "c1": "C"}
	if len(gotFiles) != len(want) {
		t.Fatalf("archivableFiles = %v, want %v", gotFiles, want)
	}
	for id, parent := range want {
		if gotFiles[id] != parent {
			t.Errorf("archivableFiles[%s] parent = %q, want %q", id, gotFiles[id], parent)
		}
	}

	folders, err := archivableFolders(db, "")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, f := range folders {
		order = append(order, f.driveID)
	}
	// Deepest first: B (depth 2) before A (depth 1). C is undecided, roots never appear.
	if strings.Join(order, ",") != "B,A" {
		t.Errorf("archivableFolders order = %v, want [B A]", order)
	}

	// Subtree scoping to A: only a1/b1 and folders B, A.
	files, err = archivableFiles(db, "A")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Errorf("archivableFiles(A) = %d files, want 2 (a1, b1)", len(files))
	}

	// An already-archived file no longer matches.
	if _, err := db.Exec(`UPDATE nodes SET original_parent_drive_id = 'A' WHERE drive_id = 'a1'`); err != nil {
		t.Fatal(err)
	}
	files, err = archivableFiles(db, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.driveID == "a1" {
			t.Error("archivableFiles includes already-archived a1")
		}
	}
}

func TestMarkArchivedAndClear(t *testing.T) {
	db := testDB(t)
	rootID, archID := buildArchiveTree(t, db)
	_ = rootID

	replicaID, err := upsertReplicaRow(db, "archA", "ARCH A", sql.NullInt64{Int64: archID, Valid: true}, "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// Replica rows are recorded fully listed so they never look like pending
	// crawl work.
	var done int
	if err := db.QueryRow(`SELECT children_done FROM nodes WHERE drive_id = 'archA'`).Scan(&done); err != nil {
		t.Fatal(err)
	}
	if done != 1 {
		t.Error("replica row not marked children_done")
	}

	if err := markArchived(db, "a1", "A", replicaID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE nodes SET delete_skipped = 1 WHERE drive_id = 'a1'`); err != nil {
		t.Fatal(err)
	}
	var origParent sql.NullString
	var parentID int64
	if err := db.QueryRow(`SELECT original_parent_drive_id, parent_id FROM nodes WHERE drive_id = 'a1'`).
		Scan(&origParent, &parentID); err != nil {
		t.Fatal(err)
	}
	if origParent.String != "A" || parentID != replicaID {
		t.Errorf("after markArchived: origParent=%v parent=%d, want A, replica row %d", origParent, parentID, replicaID)
	}

	// A second markArchived must not overwrite the true origin.
	if err := markArchived(db, "a1", "somewhere-else", replicaID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT original_parent_drive_id FROM nodes WHERE drive_id = 'a1'`).Scan(&origParent); err != nil {
		t.Fatal(err)
	}
	if origParent.String != "A" {
		t.Errorf("re-archive overwrote original parent: %v", origParent)
	}

	// clearArchived resets the marker and skip flag and re-points parent_id at
	// the original parent's row.
	if err := clearArchived(db, "a1", "A"); err != nil {
		t.Fatal(err)
	}
	var skipped int
	if err := db.QueryRow(`SELECT original_parent_drive_id, parent_id, delete_skipped FROM nodes WHERE drive_id = 'a1'`).
		Scan(&origParent, &parentID, &skipped); err != nil {
		t.Fatal(err)
	}
	var aRow int64
	if err := db.QueryRow(`SELECT id FROM nodes WHERE drive_id = 'A'`).Scan(&aRow); err != nil {
		t.Fatal(err)
	}
	if origParent.Valid || parentID != aRow || skipped != 0 {
		t.Errorf("after clearArchived: origParent=%v parent=%d skipped=%d, want NULL, A row %d, 0", origParent, parentID, skipped, aRow)
	}
}

func TestArchivedForDeletion(t *testing.T) {
	db := testDB(t)
	_, archID := buildArchiveTree(t, db)

	replicaA, err := upsertReplicaRow(db, "archA", "ARCH A", sql.NullInt64{Int64: archID, Valid: true}, "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	replicaB, err := upsertReplicaRow(db, "archB", "ARCH B", sql.NullInt64{Int64: replicaA, Valid: true}, "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	for driveID, dest := range map[string]struct {
		parentDriveID string
		row           int64
	}{
		"a1": {"A", replicaA},
		"b1": {"B", replicaB},
		"B":  {"A", replicaA},
	} {
		if err := markArchived(db, driveID, dest.parentDriveID, dest.row); err != nil {
			t.Fatal(err)
		}
	}
	// A restored-to-keep archived item must not be deleted.
	if err := markArchived(db, "c1", "C", replicaA); err != nil {
		t.Fatal(err)
	}
	setDecision(t, db, "c1", decisionKeep)

	files, folders, err := archivedForDeletion(db, "")
	if err != nil {
		t.Fatal(err)
	}
	var fileIDs []string
	for _, f := range files {
		fileIDs = append(fileIDs, f.driveID)
	}
	if len(files) != 2 || fileIDs[0] == fileIDs[1] {
		t.Fatalf("archivedForDeletion files = %v, want a1 and b1", fileIDs)
	}
	for _, id := range fileIDs {
		if id != "a1" && id != "b1" {
			t.Errorf("unexpected file %s in deletion list", id)
		}
	}
	if len(folders) != 1 || folders[0].driveID != "B" {
		t.Errorf("archivedForDeletion folders = %v, want [B]", folders)
	}
	if p := files[0].parentDriveID.String; p != "archA" && p != "archB" {
		t.Errorf("archived file's recorded parent = %q, want a replica drive id", p)
	}

	// Scoped to the replica of B: only b1 (reparented under archB).
	files, folders, err = archivedForDeletion(db, "archB")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].driveID != "b1" || len(folders) != 0 {
		t.Errorf("archivedForDeletion(archB) = %v/%v, want just b1", files, folders)
	}
}

func TestFoldersWithReplicasDeepestFirst(t *testing.T) {
	db := testDB(t)
	buildArchiveTree(t, db)
	if err := setArchiveFolder(db, "A", "archA"); err != nil {
		t.Fatal(err)
	}
	if err := setArchiveFolder(db, "B", "archB"); err != nil {
		t.Fatal(err)
	}
	reps, err := foldersWithReplicas(db)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, r := range reps {
		order = append(order, r.driveID+":"+r.archiveFolder.String)
	}
	if strings.Join(order, ",") != "B:archB,A:archA" {
		t.Errorf("foldersWithReplicas order = %v, want B before A", order)
	}

	if err := clearArchiveFolder(db, "B"); err != nil {
		t.Fatal(err)
	}
	if reps, err = foldersWithReplicas(db); err != nil || len(reps) != 1 || reps[0].driveID != "A" {
		t.Errorf("after clearArchiveFolder(B): %v err=%v, want just A", reps, err)
	}
}

func TestFolderChainToRoot(t *testing.T) {
	db := testDB(t)
	buildArchiveTree(t, db)
	if err := setArchiveFolder(db, "A", "archA"); err != nil {
		t.Fatal(err)
	}

	chain, err := folderChainToRoot(db, "B")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range chain {
		ids = append(ids, c.driveID)
	}
	// Root-exclusive, target-inclusive, top-down.
	if strings.Join(ids, ",") != "A,B" {
		t.Fatalf("folderChainToRoot(B) = %v, want [A B]", ids)
	}
	if !chain[0].archiveFolder.Valid || chain[0].archiveFolder.String != "archA" {
		t.Errorf("chain did not carry A's replica cache: %+v", chain[0])
	}

	// The root itself has an empty chain (its replica is the archive root).
	if chain, err = folderChainToRoot(db, "root"); err != nil || len(chain) != 0 {
		t.Errorf("folderChainToRoot(root) = %v err=%v, want empty", chain, err)
	}
}

func TestNodeInSubtree(t *testing.T) {
	db := testDB(t)
	buildArchiveTree(t, db)

	cases := []struct {
		root, node string
		want       bool
	}{
		{"root", "b1", true},
		{"A", "b1", true},
		{"A", "A", true},
		{"C", "b1", false},
		{"arch", "b1", false},
		{"root", "missing", false},
		{"", "b1", false},
	}
	for _, c := range cases {
		got, err := nodeInSubtree(db, c.root, c.node)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("nodeInSubtree(%q, %q) = %v, want %v", c.root, c.node, got, c.want)
		}
	}
}

// Archived rows must survive the stale sweep even when not re-observed — the
// safety net for a missing/misconfigured archive crawl phase — and detach
// cleanly when their recorded parent is pruned.
func TestDeleteStaleNodesProtectsArchived(t *testing.T) {
	db := testDB(t)
	const cutoff = "2026-06-01T00:00:00Z"
	old, fresh := "2026-05-01T00:00:00Z", "2026-06-15T00:00:00Z"

	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType})
	staleDirID, _, _, _ := mustUpsert(t, db, node{driveID: "staleDir", name: "Gone", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "archived", name: "kept.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: staleDirID, Valid: true}})
	if err := markArchived(db, "archived", "root", staleDirID); err != nil {
		t.Fatal(err)
	}

	setCrawledAt(t, db, "root", fresh)
	setCrawledAt(t, db, "staleDir", old)
	setCrawledAt(t, db, "archived", old)

	removed, err := deleteStaleNodes(db, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (only staleDir)", removed)
	}
	var parent sql.NullInt64
	if err := db.QueryRow(`SELECT parent_id FROM nodes WHERE drive_id = 'archived'`).Scan(&parent); err != nil {
		t.Fatalf("archived row should have survived: %v", err)
	}
	if parent.Valid {
		t.Errorf("archived row's parent_id = %v, want detached NULL (its parent was pruned)", parent)
	}
}

func TestDeleteStaleNodesUnderProtectsArchived(t *testing.T) {
	db := testDB(t)
	const cutoff = "2026-06-01T00:00:00Z"
	old, fresh := "2026-05-01T00:00:00Z", "2026-06-15T00:00:00Z"

	sID, _, _, _ := mustUpsert(t, db, node{driveID: "S", name: "S", typ: typeFolder, mimeType: folderMimeType})
	mustUpsert(t, db, node{driveID: "archived", name: "kept.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: sID, Valid: true}})
	mustUpsert(t, db, node{driveID: "stale", name: "gone.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: sID, Valid: true}})
	if err := markArchived(db, "archived", "S", sID); err != nil {
		t.Fatal(err)
	}

	setCrawledAt(t, db, "S", fresh)
	setCrawledAt(t, db, "archived", old)
	setCrawledAt(t, db, "stale", old)

	removed, err := deleteStaleNodesUnder(db, "S", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (only stale)", removed)
	}
	if _, err := nodeTypeByDriveID(db, "archived"); err != nil {
		t.Errorf("archived row should have survived the scoped sweep: %v", err)
	}
}

func TestDeleteNodeRow(t *testing.T) {
	db := testDB(t)
	buildArchiveTree(t, db)

	tx, _ := db.Begin()
	if err := replacePermissions(tx, "B", []permission{{permissionID: "p1", typ: "user", role: "writer"}}); err != nil {
		t.Fatal(err)
	}
	if err := recordExtraParent(tx, "B", "root"); err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	// B still has child b1; deleteNodeRow must detach it rather than trip the FK.
	if err := deleteNodeRow(db, "B"); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeTypeByDriveID(db, "B"); err != sql.ErrNoRows {
		t.Errorf("B should be gone, err=%v", err)
	}
	var parent sql.NullInt64
	if err := db.QueryRow(`SELECT parent_id FROM nodes WHERE drive_id = 'b1'`).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if parent.Valid {
		t.Errorf("b1 parent_id = %v, want detached NULL", parent)
	}
	if perms, _ := folderPermissionsFor(db, "B"); len(perms) != 0 {
		t.Errorf("B's permission rows not pruned: %d", len(perms))
	}
	var extras int
	if err := db.QueryRow(`SELECT COUNT(*) FROM extra_parents WHERE node_drive_id = 'B'`).Scan(&extras); err != nil {
		t.Fatal(err)
	}
	if extras != 0 {
		t.Errorf("B's extra_parents rows not pruned: %d", extras)
	}
}

func TestLoadReviewForestExcludesArchiveTree(t *testing.T) {
	db := testDB(t)
	_, archID := buildArchiveTree(t, db)
	mustUpsert(t, db, node{driveID: "inArch", name: "old.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: archID, Valid: true}})

	roots, err := loadReviewForest(db, "arch")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0].driveID != "root" {
		t.Fatalf("roots = %d, want just the crawl root", len(roots))
	}
	var walk func(n *reviewNode)
	seen := map[string]bool{}
	walk = func(n *reviewNode) {
		seen[n.driveID] = true
		for _, c := range n.children {
			walk(c)
		}
	}
	walk(roots[0])
	if seen["arch"] || seen["inArch"] {
		t.Error("archive tree leaked into the review forest")
	}

	// Without an exclusion both roots show, as before.
	if roots, err = loadReviewForest(db, ""); err != nil || len(roots) != 2 {
		t.Errorf("unexcluded forest roots = %d err=%v, want 2", len(roots), err)
	}
}

func TestCrawlRootDriveIDPrefersCrawlMeta(t *testing.T) {
	db := testDB(t)
	buildArchiveTree(t, db) // two parentless roots: "root" (older) and "arch"

	// Without crawl_meta, fall back to the oldest parentless node.
	if got, err := crawlRootDriveID(db); err != nil || got != "root" {
		t.Errorf("fallback crawlRootDriveID = %q err=%v, want root", got, err)
	}
	if err := setCrawlMeta(db, "root", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if got, err := crawlRootDriveID(db); err != nil || got != "root" {
		t.Errorf("crawlRootDriveID = %q err=%v, want root from crawl_meta", got, err)
	}
}

func TestConfigTemplateHasArchiveSection(t *testing.T) {
	tmpl, err := configTemplate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tmpl, `"archive"`) || !strings.Contains(tmpl, "REPLACE_WITH_ARCHIVE_FOLDER_ID") {
		t.Errorf("template missing archive section:\n%s", tmpl)
	}
	if (rootConfig{ID: "REPLACE_WITH_X", Name: "REPLACE_WITH_Y"}).configured() {
		t.Error("placeholder spec reported configured")
	}
	if (rootConfig{}).configured() {
		t.Error("empty spec reported configured")
	}
	if !(rootConfig{ID: "1abc", Name: "Archive"}).configured() {
		t.Error("real spec reported not configured")
	}
}
