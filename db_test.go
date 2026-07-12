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

func mustUpsert(t *testing.T, db *sql.DB, n node, setParent ...bool) (rowID int64, existed bool, prevParent sql.NullInt64, prevDone bool) {
	t.Helper()
	sp := false
	if len(setParent) > 0 {
		sp = setParent[0]
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rowID, existed, prevParent, prevDone, err = upsertNode(tx, n, sp)
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

// A node found under a single parent that differs from its stored parent has
// moved since the last crawl; setParent reparents it to where it lives now.
func TestUpsertReparentsMovedNode(t *testing.T) {
	db := testDB(t)

	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType})
	aID, _, _, _ := mustUpsert(t, db, node{driveID: "A", name: "A", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	bID, _, _, _ := mustUpsert(t, db, node{driveID: "B", name: "B", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})

	child := node{driveID: "child", name: "child.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: aID, Valid: true}}
	mustUpsert(t, db, child, true)

	// Re-observed under B (a single-parent move). setParent=true reparents it.
	moved := child
	moved.parentID = sql.NullInt64{Int64: bID, Valid: true}
	_, existed, prevParent, _ := mustUpsert(t, db, moved, true)
	if !existed || !prevParent.Valid || prevParent.Int64 != aID {
		t.Fatalf("re-upsert: existed=%v prevParent=%+v, want existed under A row %d", existed, prevParent, aID)
	}

	var parent int64
	if err := db.QueryRow(`SELECT parent_id FROM nodes WHERE drive_id='child'`).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if parent != bID {
		t.Errorf("parent_id = %d after move, want reparented to B row %d", parent, bID)
	}

	// A null new parent must not wipe the existing one, even with setParent=true.
	orphan := child
	orphan.parentID = sql.NullInt64{}
	mustUpsert(t, db, orphan, true)
	if err := db.QueryRow(`SELECT parent_id FROM nodes WHERE drive_id='child'`).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if parent != bID {
		t.Errorf("parent_id = %d after null-parent upsert, want preserved B row %d", parent, bID)
	}
}

func TestPendingFolders(t *testing.T) {
	db := testDB(t)
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType})
	mustUpsert(t, db, node{driveID: "sub", name: "Sub", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "f", name: "f.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})

	pending, err := pendingFolders(db, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d folders, want 2 (root, sub)", len(pending))
	}

	tx, _ := db.Begin()
	markChildrenDone(tx, rootID)
	tx.Commit()
	if n, _ := countPendingFolders(db, ""); n != 1 {
		t.Errorf("after marking root done, %d pending, want 1", n)
	}
	if err := resetChildrenDone(db, ""); err != nil {
		t.Fatal(err)
	}
	if n, _ := countPendingFolders(db, ""); n != 2 {
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

	counts, err := ownersReport(db, "")
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

func TestOwnersReportScopedToParent(t *testing.T) {
	db := testDB(t)
	// Root (bob) ─┬─ Finance (alice) ── budget.xlsx (alice), notes.txt (bob)
	//             └─ Shared  (bob)   ── plan.doc    (alice)
	buildExploreTree(t, db)

	// Scope to Finance: only Finance itself (folder, alice) plus its two files
	// (budget→alice, notes→bob) count. Shared and plan.doc are excluded.
	counts, err := ownersReport(db, "fin")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ownerCount{}
	for _, oc := range counts {
		got[oc.email.String] = oc
	}
	if len(counts) != 2 {
		t.Fatalf("got %d owners under Finance, want 2 (alice, bob): %v", len(counts), counts)
	}
	if a := got["alice@example.com"]; a.folderCount != 1 || a.fileCount != 1 {
		t.Errorf("alice under Finance = %d folders, %d files, want 1, 1", a.folderCount, a.fileCount)
	}
	if b := got["bob@example.com"]; b.folderCount != 0 || b.fileCount != 1 {
		t.Errorf("bob under Finance = %d folders, %d files, want 0, 1", b.folderCount, b.fileCount)
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
		canEdit: true})
	parent := sql.NullInt64{Int64: rootID, Valid: true}

	// Editable file — excluded.
	mustUpsert(t, db, node{driveID: "ok", name: "ok.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: parent, canEdit: true})
	// Not editable — reported, with owner label and full path.
	mustUpsert(t, db, node{driveID: "locked", name: "locked.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: parent, ownerEmail: nullString("ext@other.com"), canEdit: false})
	// Not editable folder — reported, and sorts before the file.
	mustUpsert(t, db, node{driveID: "lockedfolder", name: "Shared", typ: typeFolder, mimeType: folderMimeType,
		parentID: parent, canEdit: false})

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

// buildPackTree builds the migration fixture used by the pack query tests:
//
//	Root (bob) ─┬─ A (alice) ─┬─ a1.pdf (alice)        nested owned: rides along
//	            │             ├─ B (alice) ── b1 (bob) owned folder + unowned child
//	            │             ├─ E (carol) ── e1 (alice, via owner_id)
//	            │             └─ x1.pdf (no owner)     unowned child of owned
//	            ├─ loose.pdf (alice)
//	            └─ C (bob) ── c1.pdf (alice)
func buildPackTree(t *testing.T, db *sql.DB) {
	t.Helper()
	alice := nullString("alice@example.com")
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType,
		ownerEmail: nullString("bob@example.com")})
	aID, _, _, _ := mustUpsert(t, db, node{driveID: "A", name: "A", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}, ownerEmail: alice})
	mustUpsert(t, db, node{driveID: "a1", name: "a1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: aID, Valid: true}, ownerEmail: alice})
	bID, _, _, _ := mustUpsert(t, db, node{driveID: "B", name: "B", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: aID, Valid: true}, ownerEmail: alice})
	mustUpsert(t, db, node{driveID: "b1", name: "b1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: bID, Valid: true}, ownerEmail: nullString("bob@example.com")})
	eID, _, _, _ := mustUpsert(t, db, node{driveID: "E", name: "E", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: aID, Valid: true}, ownerEmail: nullString("carol@example.com")})
	// Owned via owner_id rather than email, inside an unowned folder.
	mustUpsert(t, db, node{driveID: "e1", name: "e1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: eID, Valid: true}, ownerID: nullString("alice@example.com")})
	// No owner at all: must count as not-owned (IS NOT handles the NULLs).
	mustUpsert(t, db, node{driveID: "x1", name: "x1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: aID, Valid: true}})
	mustUpsert(t, db, node{driveID: "loose", name: "loose.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: rootID, Valid: true}, ownerEmail: alice})
	cID, _, _, _ := mustUpsert(t, db, node{driveID: "C", name: "C", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}, ownerEmail: nullString("bob@example.com")})
	mustUpsert(t, db, node{driveID: "c1", name: "c1.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: cID, Valid: true}, ownerEmail: alice})
}

func TestNodesOwnedBy(t *testing.T) {
	db := testDB(t)
	buildPackTree(t, db)

	nodes, err := nodesOwnedBy(db, "alice@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range nodes {
		got = append(got, n.driveID)
	}
	if strings.Join(got, ",") != "A,a1,B,e1,loose,c1" {
		t.Errorf("nodesOwnedBy = %v, want [A a1 B e1 loose c1]", got)
	}
}

func TestUpdateSubtreeOwner(t *testing.T) {
	db := testDB(t)
	buildPackTree(t, db)

	// Flip the whole subtree rooted at A (an owned root pack moves intact) to the
	// org account, as unpack does after the drag. Only alice's rows within A's
	// subtree — A, a1, B, and e1 (owned via owner_id) — should change; the nested
	// third-party items b1 (bob) and E (carol), which pack parks in the Stash,
	// and the ownerless x1 must be left alone.
	n, err := updateSubtreeOwner(db, "A", "alice@example.com", "org@example.com", "orgPID", "Org Admin")
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("updateSubtreeOwner rows affected = %d, want 4", n)
	}

	owners := map[string]string{}
	rows, err := db.Query(`SELECT drive_id, COALESCE(owner_email, owner_id, "") FROM nodes`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, owner string
		if err := rows.Scan(&id, &owner); err != nil {
			t.Fatal(err)
		}
		owners[id] = owner
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"A", "a1", "B", "e1"} {
		if owners[id] != "org@example.com" {
			t.Errorf("owner[%s] = %q after flip, want org@example.com", id, owners[id])
		}
	}
	if owners["b1"] != "bob@example.com" {
		t.Errorf("owner[b1] = %q, want bob@example.com (stashed, unchanged)", owners["b1"])
	}
	if owners["E"] != "carol@example.com" {
		t.Errorf("owner[E] = %q, want carol@example.com (stashed, unchanged)", owners["E"])
	}
	if owners["x1"] != "" {
		t.Errorf("owner[x1] = %q, want empty (ownerless, unchanged)", owners["x1"])
	}
	// Alice's items outside A's subtree must not be touched.
	if owners["loose"] != "alice@example.com" {
		t.Errorf("owner[loose] = %q, want alice@example.com (outside subtree)", owners["loose"])
	}
	if owners["c1"] != "alice@example.com" {
		t.Errorf("owner[c1] = %q, want alice@example.com (outside subtree)", owners["c1"])
	}
}

func TestNodesOwnedBySubfolder(t *testing.T) {
	db := testDB(t)
	buildPackTree(t, db)

	// Scoped to A: only alice's items within A's subtree (A itself, a1, B, e1),
	// not loose or c1 which live elsewhere under the root.
	nodes, err := nodesOwnedBy(db, "alice@example.com", "A")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range nodes {
		got = append(got, n.driveID)
	}
	if strings.Join(got, ",") != "A,a1,B,e1" {
		t.Errorf("nodesOwnedBy(A) = %v, want [A a1 B e1]", got)
	}
}

func TestOwnedRoots(t *testing.T) {
	db := testDB(t)
	buildPackTree(t, db)

	roots, err := ownedRoots(db, "alice@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range roots {
		got[r.driveID] = r.parentDriveID
	}
	// A, loose, c1 hang off unowned parents; e1's parent E is carol's; a1 and B
	// live inside owned A and ride along.
	want := map[string]string{"A": "root", "e1": "E", "loose": "root", "c1": "C"}
	if len(got) != len(want) {
		t.Fatalf("ownedRoots = %v, want %v", got, want)
	}
	for id, parent := range want {
		if got[id] != parent {
			t.Errorf("ownedRoots[%s] parent = %q, want %q", id, got[id], parent)
		}
	}
}

func TestOwnedRootsSubfolder(t *testing.T) {
	db := testDB(t)
	buildPackTree(t, db)

	// Scoped to A: A itself is a root even though its parent Root is owned by
	// bob — the subfolder boundary makes it one, and it carries a1/B along. e1's
	// parent E (carol's) is not owned, so e1 is a separate root. loose and c1
	// live outside A and are excluded.
	roots, err := ownedRoots(db, "alice@example.com", "A")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range roots {
		got[r.driveID] = r.parentDriveID
	}
	want := map[string]string{"A": "root", "e1": "E"}
	if len(got) != len(want) {
		t.Fatalf("ownedRoots(A) = %v, want %v", got, want)
	}
	for id, parent := range want {
		if got[id] != parent {
			t.Errorf("ownedRoots(A)[%s] parent = %q, want %q", id, got[id], parent)
		}
	}
}

func TestOwnedRootsSubfolderOwnedParent(t *testing.T) {
	db := testDB(t)
	buildPackTree(t, db)

	// Scoped to B (owned by alice, whose parent A is also owned by alice): B must
	// still count as a root of the scoped pack even though its parent is owned,
	// because A lies outside the subfolder. b1 (bob's) is not owned, so B is the
	// only root.
	roots, err := ownedRoots(db, "alice@example.com", "B")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range roots {
		got[r.driveID] = r.parentDriveID
	}
	want := map[string]string{"B": "A"}
	if len(got) != len(want) {
		t.Fatalf("ownedRoots(B) = %v, want %v", got, want)
	}
	for id, parent := range want {
		if got[id] != parent {
			t.Errorf("ownedRoots(B)[%s] parent = %q, want %q", id, got[id], parent)
		}
	}
}

func TestOwnedRootsExcludesCrawlRoot(t *testing.T) {
	db := testDB(t)
	// The crawl root itself owned by the migrating user must never be a root.
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType,
		ownerEmail: nullString("alice@example.com")})
	mustUpsert(t, db, node{driveID: "f", name: "f.pdf", typ: typeBinary, mimeType: "application/pdf",
		parentID: sql.NullInt64{Int64: rootID, Valid: true}, ownerEmail: nullString("alice@example.com")})

	roots, err := ownedRoots(db, "alice@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 0 {
		t.Errorf("ownedRoots = %v, want none (root has no parent, f's parent is owned)", roots)
	}
}

func TestUnownedChildrenOfOwned(t *testing.T) {
	db := testDB(t)
	buildPackTree(t, db)

	items, err := unownedChildrenOfOwned(db, "alice@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range items {
		got[s.driveID] = s.parentDriveID
	}
	// b1 (bob's, in alice's B), E (carol's, in alice's A), and ownerless x1.
	want := map[string]string{"b1": "B", "E": "A", "x1": "A"}
	if len(got) != len(want) {
		t.Fatalf("unownedChildrenOfOwned = %v, want %v", got, want)
	}
	for id, parent := range want {
		if got[id] != parent {
			t.Errorf("unownedChildrenOfOwned[%s] parent = %q, want %q", id, got[id], parent)
		}
	}
}

func TestUnownedChildrenOfOwnedSubfolder(t *testing.T) {
	db := testDB(t)
	buildPackTree(t, db)

	// Scoped to B: only b1 (bob's, inside alice's B) is previewed. E and x1 live
	// directly under A, outside B's subtree, so they are excluded.
	items, err := unownedChildrenOfOwned(db, "alice@example.com", "B")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range items {
		got[s.driveID] = s.parentDriveID
	}
	want := map[string]string{"b1": "B"}
	if len(got) != len(want) {
		t.Fatalf("unownedChildrenOfOwned(B) = %v, want %v", got, want)
	}
	for id, parent := range want {
		if got[id] != parent {
			t.Errorf("unownedChildrenOfOwned(B)[%s] parent = %q, want %q", id, got[id], parent)
		}
	}
}

func TestUserMigrationLifecycle(t *testing.T) {
	db := testDB(t)

	if m, err := getUserMigration(db, "alice@example.com"); err != nil || m != nil {
		t.Fatalf("before pack: migration = %v, err = %v; want nil, nil", m, err)
	}

	if err := upsertUserMigration(db, "alice@example.com", "uf", "pick", "cont", "stash"); err != nil {
		t.Fatal(err)
	}
	if err := markPacked(db, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	m, err := getUserMigration(db, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if m.containerID != "cont" || m.stashID != "stash" || m.userFolderID != "uf" || m.pickupID != "pick" {
		t.Errorf("migration ids = %+v", m)
	}
	if !m.packedAt.Valid || m.unpackedAt.Valid {
		t.Errorf("after markPacked: packedAt=%v unpackedAt=%v, want set/unset", m.packedAt, m.unpackedAt)
	}

	if err := markUnpacked(db, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if m, _ = getUserMigration(db, "alice@example.com"); !m.unpackedAt.Valid {
		t.Error("after markUnpacked: unpackedAt not set")
	}

	// Re-running pack re-records the scaffolding and restarts the cycle.
	if err := upsertUserMigration(db, "alice@example.com", "uf2", "pick2", "cont2", "stash2"); err != nil {
		t.Fatal(err)
	}
	m, _ = getUserMigration(db, "alice@example.com")
	if m.containerID != "cont2" || m.pickupID != "pick2" || m.packedAt.Valid || m.unpackedAt.Valid {
		t.Errorf("after re-upsert: %+v, want new ids and cleared timestamps", m)
	}
}

func TestPackOrphans(t *testing.T) {
	db := testDB(t)

	if _, err := packOrphanParent(db, "alice@example.com", "x"); err != sql.ErrNoRows {
		t.Errorf("missing orphan: err = %v, want sql.ErrNoRows", err)
	}
	if err := recordPackOrphan(db, "alice@example.com", "x", "parent1"); err != nil {
		t.Fatal(err)
	}
	// Re-sweeping the same item from a different live parent must overwrite.
	if err := recordPackOrphan(db, "alice@example.com", "x", "parent2"); err != nil {
		t.Fatal(err)
	}
	parent, err := packOrphanParent(db, "alice@example.com", "x")
	if err != nil {
		t.Fatal(err)
	}
	if parent != "parent2" {
		t.Errorf("orphan parent = %q, want parent2", parent)
	}
}

func TestFolderPermissionsFor(t *testing.T) {
	db := testDB(t)
	mustUpsert(t, db, node{driveID: "folder", name: "Folder", typ: typeFolder, mimeType: folderMimeType})

	tx, _ := db.Begin()
	if err := replacePermissions(tx, "folder", []permission{
		{permissionID: "p1", typ: "user", role: "writer", emailAddress: nullString("a@example.com")},
		{permissionID: "p2", typ: "anyone", role: "reader", allowFileDiscovery: sql.NullBool{Bool: false, Valid: true}},
		{permissionID: "p3", typ: "user", role: "owner", emailAddress: nullString("owner@gmail.com"), deleted: true},
	}); err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	perms, err := folderPermissionsFor(db, "folder")
	if err != nil {
		t.Fatal(err)
	}
	if len(perms) != 3 {
		t.Fatalf("got %d permissions, want 3", len(perms))
	}
	byID := map[string]permission{}
	for _, p := range perms {
		byID[p.permissionID] = p
	}
	if p := byID["p1"]; p.typ != "user" || p.role != "writer" || p.emailAddress.String != "a@example.com" {
		t.Errorf("p1 = %+v", p)
	}
	if p := byID["p2"]; !p.allowFileDiscovery.Valid || p.allowFileDiscovery.Bool {
		t.Errorf("p2 allowFileDiscovery = %+v, want valid false", p.allowFileDiscovery)
	}
	if p := byID["p3"]; !p.deleted {
		t.Errorf("p3 deleted = %v, want true", p.deleted)
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

func TestSubtreeRelativePath(t *testing.T) {
	db := testDB(t)
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "DxE General", typ: typeFolder, mimeType: folderMimeType})
	subID, _, _, _ := mustUpsert(t, db, node{driveID: "sub", name: "Finance", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "deep", name: "2025", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: subID, Valid: true}})

	// Path relative to the crawl root drops the root's own name.
	if got, err := subtreeRelativePath(db, "deep"); err != nil {
		t.Fatal(err)
	} else if got != "Finance/2025" {
		t.Errorf("subtreeRelativePath(deep) = %q, want %q", got, "Finance/2025")
	}
	// The crawl root itself has an empty relative path.
	if got, err := subtreeRelativePath(db, "root"); err != nil {
		t.Fatal(err)
	} else if got != "" {
		t.Errorf("subtreeRelativePath(root) = %q, want empty", got)
	}
}

func TestSubtreeDriveIDs(t *testing.T) {
	db := testDB(t)
	buildPackTree(t, db)

	ids, err := subtreeDriveIDs(db, "A")
	if err != nil {
		t.Fatal(err)
	}
	// A and everything beneath it, but not loose, C, c1, or the root.
	want := map[string]bool{"A": true, "a1": true, "B": true, "b1": true, "E": true, "e1": true, "x1": true}
	if len(ids) != len(want) {
		t.Fatalf("subtreeDriveIDs(A) = %v, want %v", ids, want)
	}
	for id := range want {
		if !ids[id] {
			t.Errorf("subtreeDriveIDs(A) missing %q", id)
		}
	}
}

// setCrawledAt overrides a node's crawled_at so tests can place it on either
// side of a session cutoff without depending on wall-clock timing.
func setCrawledAt(t *testing.T, db *sql.DB, driveID, ts string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE nodes SET crawled_at = ? WHERE drive_id = ?`, ts, driveID); err != nil {
		t.Fatal(err)
	}
}

func TestCrawlMeta(t *testing.T) {
	db := testDB(t)

	if _, _, ok, err := getCrawlMeta(db); err != nil || ok {
		t.Fatalf("fresh db: ok=%v err=%v, want false, nil", ok, err)
	}
	if err := setCrawlMeta(db, "rootA", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	root, start, ok, err := getCrawlMeta(db)
	if err != nil || !ok || root != "rootA" || start != "2026-01-01T00:00:00Z" {
		t.Fatalf("after set: root=%q start=%q ok=%v err=%v", root, start, ok, err)
	}
	// A later session overwrites the single row rather than accumulating.
	if err := setCrawlMeta(db, "rootB", "2026-02-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	root, start, _, _ = getCrawlMeta(db)
	if root != "rootB" || start != "2026-02-02T00:00:00Z" {
		t.Fatalf("after re-set: root=%q start=%q", root, start)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM crawl_meta`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("crawl_meta rows = %d, want 1", count)
	}
}

func TestDeleteStaleNodes(t *testing.T) {
	db := testDB(t)
	const cutoff = "2026-06-01T00:00:00Z"
	old, fresh := "2026-05-01T00:00:00Z", "2026-06-01T00:00:00Z"

	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType})
	// A stale folder (not re-seen) with a stale child beneath it.
	staleDirID, _, _, _ := mustUpsert(t, db, node{driveID: "staleDir", name: "Gone", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "staleChild", name: "gone.txt", typ: typeBinary, mimeType: "application/x",
		parentID: sql.NullInt64{Int64: staleDirID, Valid: true}})
	// A current folder that survives.
	mustUpsert(t, db, node{driveID: "keep", name: "kept.txt", typ: typeBinary, mimeType: "application/x",
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})

	// Record aux rows tied to the stale folder and a surviving one.
	tx, _ := db.Begin()
	if err := replacePermissions(tx, "staleDir", []permission{{permissionID: "p1", typ: "user", role: "writer"}}); err != nil {
		t.Fatal(err)
	}
	if err := replacePermissions(tx, "root", []permission{{permissionID: "p2", typ: "user", role: "owner"}}); err != nil {
		t.Fatal(err)
	}
	if err := recordExtraParent(tx, "staleChild", "root"); err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	setCrawledAt(t, db, "root", fresh)
	setCrawledAt(t, db, "keep", fresh)
	setCrawledAt(t, db, "staleDir", old)
	setCrawledAt(t, db, "staleChild", old)

	removed, err := deleteStaleNodes(db, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	for _, id := range []string{"root", "keep"} {
		if _, err := nodeTypeByDriveID(db, id); err != nil {
			t.Errorf("%q should have survived: %v", id, err)
		}
	}
	for _, id := range []string{"staleDir", "staleChild"} {
		if _, err := nodeTypeByDriveID(db, id); err != sql.ErrNoRows {
			t.Errorf("%q should have been deleted, err=%v", id, err)
		}
	}
	// Aux rows for the deleted node are pruned; the surviving folder's stay.
	if perms, _ := folderPermissionsFor(db, "staleDir"); len(perms) != 0 {
		t.Errorf("stale folder permissions not pruned: %d rows", len(perms))
	}
	if perms, _ := folderPermissionsFor(db, "root"); len(perms) != 1 {
		t.Errorf("surviving folder permissions = %d, want 1", len(perms))
	}
	var extra int
	if err := db.QueryRow(`SELECT COUNT(*) FROM extra_parents`).Scan(&extra); err != nil {
		t.Fatal(err)
	}
	if extra != 0 {
		t.Errorf("extra_parents referencing a deleted node not pruned: %d rows", extra)
	}
}

// A surviving node whose first-discovered parent goes stale is detached to a
// root rather than left dangling (or blocking the delete on the foreign key).
func TestDeleteStaleNodesReparentsSurvivor(t *testing.T) {
	db := testDB(t)
	const cutoff = "2026-06-01T00:00:00Z"

	staleParentID, _, _, _ := mustUpsert(t, db, node{driveID: "staleParent", name: "Gone", typ: typeFolder, mimeType: folderMimeType})
	mustUpsert(t, db, node{driveID: "survivor", name: "moved.txt", typ: typeBinary, mimeType: "application/x",
		parentID: sql.NullInt64{Int64: staleParentID, Valid: true}})

	setCrawledAt(t, db, "staleParent", "2026-05-01T00:00:00Z")
	setCrawledAt(t, db, "survivor", "2026-06-15T00:00:00Z")

	if _, err := deleteStaleNodes(db, cutoff); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeTypeByDriveID(db, "staleParent"); err != sql.ErrNoRows {
		t.Errorf("stale parent should be gone, err=%v", err)
	}
	var parent sql.NullInt64
	if err := db.QueryRow(`SELECT parent_id FROM nodes WHERE drive_id = 'survivor'`).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if parent.Valid {
		t.Errorf("survivor parent_id = %v, want NULL after its parent was removed", parent)
	}
}

func TestScopedPendingFolders(t *testing.T) {
	db := testDB(t)
	// root ─┬─ A ── sub
	//       └─ B
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType})
	aID, _, _, _ := mustUpsert(t, db, node{driveID: "A", name: "A", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "sub", name: "Sub", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: aID, Valid: true}})
	mustUpsert(t, db, node{driveID: "B", name: "B", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})

	// Everything done, then scope a reset to A's subtree.
	if err := markAllChildrenDone(t, db); err != nil {
		t.Fatal(err)
	}
	if err := resetChildrenDone(db, "A"); err != nil {
		t.Fatal(err)
	}

	pending, err := pendingFolders(db, "A")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range pending {
		got = append(got, f.driveID)
	}
	if strings.Join(got, ",") != "A,sub" {
		t.Errorf("pendingFolders(A) = %v, want [A sub] (root and B untouched)", got)
	}
	if n, _ := countPendingFolders(db, "A"); n != 2 {
		t.Errorf("countPendingFolders(A) = %d, want 2", n)
	}
	// The reset must not have touched folders outside A's subtree.
	if n, _ := countPendingFolders(db, ""); n != 2 {
		t.Errorf("total pending = %d, want 2 (only A and sub); reset leaked outside the subtree", n)
	}
}

func markAllChildrenDone(t *testing.T, db *sql.DB) error {
	t.Helper()
	_, err := db.Exec(`UPDATE nodes SET children_done = 1 WHERE type = '` + typeFolder + `'`)
	return err
}

func TestDeleteStaleNodesUnder(t *testing.T) {
	db := testDB(t)
	const cutoff = "2026-06-01T00:00:00Z"
	old, fresh := "2026-05-01T00:00:00Z", "2026-06-15T00:00:00Z"

	// root ─┬─ A ─┬─ aKeep        (re-indexed subtree: A)
	//       │     └─ aStaleDir ── aStaleChild
	//       └─ B ── bChild        (outside A: must be left alone even though stale)
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType})
	aID, _, _, _ := mustUpsert(t, db, node{driveID: "A", name: "A", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "aKeep", name: "keep.txt", typ: typeBinary, mimeType: "application/x",
		parentID: sql.NullInt64{Int64: aID, Valid: true}})
	staleDirID, _, _, _ := mustUpsert(t, db, node{driveID: "aStaleDir", name: "Gone", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: aID, Valid: true}})
	mustUpsert(t, db, node{driveID: "aStaleChild", name: "gone.txt", typ: typeBinary, mimeType: "application/x",
		parentID: sql.NullInt64{Int64: staleDirID, Valid: true}})
	bID, _, _, _ := mustUpsert(t, db, node{driveID: "B", name: "B", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "bChild", name: "b.txt", typ: typeBinary, mimeType: "application/x",
		parentID: sql.NullInt64{Int64: bID, Valid: true}})

	// Aux rows: one on the stale folder (should be pruned) and one on B outside
	// the scope (must survive even though B is stale, because it is not re-indexed).
	tx, _ := db.Begin()
	if err := replacePermissions(tx, "aStaleDir", []permission{{permissionID: "p1", typ: "user", role: "writer"}}); err != nil {
		t.Fatal(err)
	}
	if err := replacePermissions(tx, "B", []permission{{permissionID: "p2", typ: "user", role: "owner"}}); err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	// Re-index of A refreshed A, aKeep; the vanished aStaleDir/aStaleChild keep
	// their old timestamps. B's subtree was never touched this run, so it is old.
	setCrawledAt(t, db, "root", fresh)
	setCrawledAt(t, db, "A", fresh)
	setCrawledAt(t, db, "aKeep", fresh)
	setCrawledAt(t, db, "aStaleDir", old)
	setCrawledAt(t, db, "aStaleChild", old)
	setCrawledAt(t, db, "B", old)
	setCrawledAt(t, db, "bChild", old)

	removed, err := deleteStaleNodesUnder(db, "A", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (aStaleDir, aStaleChild)", removed)
	}
	for _, id := range []string{"root", "A", "aKeep", "B", "bChild"} {
		if _, err := nodeTypeByDriveID(db, id); err != nil {
			t.Errorf("%q should have survived: %v", id, err)
		}
	}
	for _, id := range []string{"aStaleDir", "aStaleChild"} {
		if _, err := nodeTypeByDriveID(db, id); err != sql.ErrNoRows {
			t.Errorf("%q should have been deleted, err=%v", id, err)
		}
	}
	if perms, _ := folderPermissionsFor(db, "aStaleDir"); len(perms) != 0 {
		t.Errorf("stale folder permissions not pruned: %d rows", len(perms))
	}
	if perms, _ := folderPermissionsFor(db, "B"); len(perms) != 1 {
		t.Errorf("out-of-scope folder permissions = %d, want 1 (must not be pruned)", len(perms))
	}
}

// A stale node deep beneath a survivor whose own (stale) parent is being deleted
// must still be pruned: the subtree is snapshotted before the survivor is
// detached, so severing its parent link does not hide descendants from the sweep.
func TestDeleteStaleNodesUnderDeepStaleBelowDetachedSurvivor(t *testing.T) {
	db := testDB(t)
	const cutoff = "2026-06-01T00:00:00Z"
	old, fresh := "2026-05-01T00:00:00Z", "2026-06-15T00:00:00Z"

	// S ── staleParent ── survivor ── deepStale
	// survivor is re-observed (fresh) but keeps staleParent as first-discovered
	// parent; staleParent and deepStale vanished from Drive.
	sID, _, _, _ := mustUpsert(t, db, node{driveID: "S", name: "S", typ: typeFolder, mimeType: folderMimeType})
	spID, _, _, _ := mustUpsert(t, db, node{driveID: "staleParent", name: "Gone", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: sID, Valid: true}})
	survID, _, _, _ := mustUpsert(t, db, node{driveID: "survivor", name: "Survivor", typ: typeFolder, mimeType: folderMimeType,
		parentID: sql.NullInt64{Int64: spID, Valid: true}})
	mustUpsert(t, db, node{driveID: "deepStale", name: "deep.txt", typ: typeBinary, mimeType: "application/x",
		parentID: sql.NullInt64{Int64: survID, Valid: true}})

	setCrawledAt(t, db, "S", fresh)
	setCrawledAt(t, db, "staleParent", old)
	setCrawledAt(t, db, "survivor", fresh)
	setCrawledAt(t, db, "deepStale", old)

	removed, err := deleteStaleNodesUnder(db, "S", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (staleParent, deepStale)", removed)
	}
	for _, id := range []string{"staleParent", "deepStale"} {
		if _, err := nodeTypeByDriveID(db, id); err != sql.ErrNoRows {
			t.Errorf("%q should have been deleted, err=%v", id, err)
		}
	}
	// The survivor is kept and detached to a root, mirroring deleteStaleNodes.
	var parent sql.NullInt64
	if err := db.QueryRow(`SELECT parent_id FROM nodes WHERE drive_id = 'survivor'`).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if parent.Valid {
		t.Errorf("survivor parent_id = %v, want NULL after its stale parent was removed", parent)
	}
}

func TestWipeCrawlSnapshot(t *testing.T) {
	db := testDB(t)
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType})
	mustUpsert(t, db, node{driveID: "child", name: "c", typ: typeBinary, mimeType: "application/x",
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	tx, _ := db.Begin()
	if err := replacePermissions(tx, "root", []permission{{permissionID: "p1", typ: "user", role: "owner"}}); err != nil {
		t.Fatal(err)
	}
	if err := recordExtraParent(tx, "child", "root"); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	// Migration state must survive a snapshot wipe.
	if err := upsertUserMigration(db, "alice@example.com", "uf", "pick", "cont", "stash"); err != nil {
		t.Fatal(err)
	}

	if err := wipeCrawlSnapshot(db); err != nil {
		t.Fatal(err)
	}

	for _, tbl := range []string{"nodes", "folder_permissions", "extra_parents"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s not wiped: %d rows", tbl, n)
		}
	}
	if m, err := getUserMigration(db, "alice@example.com"); err != nil || m == nil {
		t.Errorf("migration state should survive wipe: m=%v err=%v", m, err)
	}
}
