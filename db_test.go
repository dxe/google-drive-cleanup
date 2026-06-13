package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustUpsert(t *testing.T, db *sql.DB, n node) (rowID int64, existed bool, prevParent sql.NullInt64, prevDone bool) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rowID, existed, prevParent, prevDone, err = upsertNode(tx, n)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return rowID, existed, prevParent, prevDone
}

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"application/vnd.google-apps.folder":      "folder",
		"application/vnd.google-apps.shortcut":    "shortcut",
		"application/vnd.google-apps.document":    "google_doc",
		"application/vnd.google-apps.spreadsheet": "google_doc",
		"application/pdf":                         "binary",
		"image/png":                               "binary",
	}
	for mime, want := range cases {
		if got := classify(mime); got != want {
			t.Errorf("classify(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestUpsertIdempotentAndProgressPreserving(t *testing.T) {
	db := testDB(t)

	rootID, existed, _, _ := mustUpsert(t, db, node{
		driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType,
	})
	if existed {
		t.Fatal("fresh insert reported existed=true")
	}

	childN := node{
		driveID: "child", name: "Child", typ: typeBinary, mimeType: "application/pdf",
		ownerEmail: nullString("a@example.com"),
		parentID:   sql.NullInt64{Int64: rootID, Valid: true},
	}
	childID, _, _, _ := mustUpsert(t, db, childN)

	// Mark root done, then re-upsert both: progress and parent must survive.
	tx, _ := db.Begin()
	if err := markChildrenDone(tx, rootID); err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	mustUpsert(t, db, node{driveID: "root", name: "Root renamed", typ: typeFolder, mimeType: folderMimeType})

	otherID, _, _, _ := mustUpsert(t, db, node{
		driveID: "other", name: "Other", typ: typeFolder, mimeType: folderMimeType,
	})
	reN := childN
	reN.name = "Child renamed"
	reN.ownerEmail = nullString("b@example.com")
	reN.parentID = sql.NullInt64{Int64: otherID, Valid: true} // rediscovered under a different parent
	reID, existed, prevParent, _ := mustUpsert(t, db, reN)

	if reID != childID {
		t.Errorf("re-upsert returned row %d, want %d", reID, childID)
	}
	if !existed || !prevParent.Valid || prevParent.Int64 != rootID {
		t.Errorf("re-upsert: existed=%v prevParent=%+v, want existed under root row %d", existed, prevParent, rootID)
	}

	var name, email string
	var parent int64
	var done int
	if err := db.QueryRow(`SELECT name, owner_email, parent_id FROM nodes WHERE drive_id='child'`).
		Scan(&name, &email, &parent); err != nil {
		t.Fatal(err)
	}
	if name != "Child renamed" || email != "b@example.com" {
		t.Errorf("metadata not refreshed: name=%q email=%q", name, email)
	}
	if parent != rootID {
		t.Errorf("parent_id was reparented to %d, want first-discovered %d", parent, rootID)
	}
	if err := db.QueryRow(`SELECT children_done FROM nodes WHERE drive_id='root'`).Scan(&done); err != nil {
		t.Fatal(err)
	}
	if done != 1 {
		t.Error("re-upsert regressed children_done on root")
	}
}

func TestPendingFolders(t *testing.T) {
	db := testDB(t)
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType})
	mustUpsert(t, db, node{driveID: "sub", name: "Sub", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "f", name: "f.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})

	pending, err := pendingFolders(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d folders, want 2 (root, sub)", len(pending))
	}

	tx, _ := db.Begin()
	markChildrenDone(tx, rootID)
	tx.Commit()
	if n, _ := countPendingFolders(db); n != 1 {
		t.Errorf("after marking root done, %d pending, want 1", n)
	}
	if err := resetChildrenDone(db); err != nil {
		t.Fatal(err)
	}
	if n, _ := countPendingFolders(db); n != 2 {
		t.Errorf("after refresh, %d pending, want 2", n)
	}
}

func TestOwnersReport(t *testing.T) {
	db := testDB(t)
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType,
		ownerEmail: nullString("owner@example.com")}) // a folder-only owner
	parent := sql.NullInt64{Int64: rootID, Valid: true}

	for i, n := range []node{
		{ownerEmail: nullString("alice@example.com"), ownerID: nullString("111"), ownerDisplay: nullString("Alice")},
		{ownerEmail: nullString("alice@example.com"), ownerID: nullString("111"), ownerDisplay: nullString("Alice")},
		{ownerID: nullString("222"), ownerDisplay: nullString("Bob")}, // email missing -> id bucket
		{}, // both missing -> unknown bucket
	} {
		n.driveID = string(rune('a' + i))
		n.name = n.driveID
		n.typ = typeBinary
		n.mimeType = "application/pdf"
		n.parentID = parent
		mustUpsert(t, db, n)
	}

	counts, err := ownersReport(db)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, oc := range counts {
		got = append(got, ownerLabel(oc))
	}
	want := []string{"alice@example.com (Alice)", "id:222 (Bob)", "(unknown)", "owner@example.com"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("owners = %v, want %v", got, want)
	}
	if counts[0].fileCount != 2 || counts[1].fileCount != 1 || counts[2].fileCount != 1 || counts[3].fileCount != 0 {
		t.Errorf("file counts = %d,%d,%d,%d, want 2,1,1,0",
			counts[0].fileCount, counts[1].fileCount, counts[2].fileCount, counts[3].fileCount)
	}
	if counts[3].folderCount != 1 || counts[3].total != 1 {
		t.Errorf("owner@example.com = %d folders, %d total, want 1,1", counts[3].folderCount, counts[3].total)
	}
}

func TestReplacePermissionsSnapshots(t *testing.T) {
	db := testDB(t)
	mustUpsert(t, db, node{driveID: "folder", name: "Folder", typ: typeFolder, mimeType: folderMimeType})

	write := func(perms []permission) {
		t.Helper()
		tx, _ := db.Begin()
		if err := replacePermissions(tx, "folder", perms); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
	}

	write([]permission{
		{permissionID: "p1", typ: "user", role: "writer", emailAddress: nullString("a@example.com")},
		{permissionID: "p2", typ: "anyone", role: "reader", allowFileDiscovery: sql.NullBool{Bool: true, Valid: true}},
	})

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM folder_permissions WHERE node_drive_id='folder'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("after first write: %d permissions, want 2", n)
	}

	// A re-crawl with a smaller set must replace, not accumulate.
	write([]permission{{permissionID: "p1", typ: "user", role: "reader", emailAddress: nullString("a@example.com")}})

	var role string
	if err := db.QueryRow(`SELECT role FROM folder_permissions WHERE node_drive_id='folder' AND permission_id='p1'`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "reader" {
		t.Errorf("role = %q, want reader (refreshed)", role)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM folder_permissions WHERE node_drive_id='folder'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("after second write: %d permissions, want 1 (stale p2 dropped)", n)
	}
}

func TestNodesLackingEditAccess(t *testing.T) {
	db := testDB(t)
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType,
		canEdit: sql.NullBool{Bool: true, Valid: true}})
	parent := sql.NullInt64{Int64: rootID, Valid: true}

	// Editable file — excluded.
	mustUpsert(t, db, node{driveID: "ok", name: "ok.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: parent, canEdit: sql.NullBool{Bool: true, Valid: true}})
	// Unknown capability (e.g. legacy crawl) — excluded.
	mustUpsert(t, db, node{driveID: "unknown", name: "unknown.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: parent})
	// Not editable — reported, with owner label and full path.
	mustUpsert(t, db, node{driveID: "locked", name: "locked.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: parent, ownerEmail: nullString("ext@other.com"), canEdit: sql.NullBool{Bool: false, Valid: true}})
	// Not editable folder — reported, and sorts before the file.
	mustUpsert(t, db, node{driveID: "lockedfolder", name: "Shared", typ: typeFolder, mimeType: folderMimeType,
		parentID: parent, canEdit: sql.NullBool{Bool: false, Valid: true}})

	rows, err := nodesLackingEditAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (locked folder + locked file)", len(rows))
	}
	if rows[0].typ != typeFolder || rows[0].path != "Root / Shared" {
		t.Errorf("row[0] = %+v, want folder at Root / Shared", rows[0])
	}
	if rows[1].driveID != "locked" || rows[1].path != "Root / locked.pdf" {
		t.Errorf("row[1] path = %q, want Root / locked.pdf", rows[1].path)
	}
	if rows[1].owner != "ext@other.com" {
		t.Errorf("row[1] owner = %q, want ext@other.com", rows[1].owner)
	}
}

func TestNodePath(t *testing.T) {
	db := testDB(t)
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "DxE General", typ: typeFolder, mimeType: folderMimeType})
	subID, _, _, _ := mustUpsert(t, db, node{driveID: "sub", name: "Finance", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "doc", name: "budget.xlsx", typ: typeBinary, mimeType: "application/x",
		parentID: sql.NullInt64{Int64: subID, Valid: true}})

	segments, err := nodePath(db, "doc")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(segments, " / "), "DxE General / Finance / budget.xlsx"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if _, err := nodePath(db, "missing"); err == nil {
		t.Error("expected error for unknown drive id")
	}
}
