package main

import (
	"database/sql"
	"testing"

	"google.golang.org/api/drive/v3"
)

// buildReclaimTree builds the fixture shared by the reclaim query tests:
//
//	root ─┬─ A (alice) ─┬─ a1.pdf
//	      │             └─ B (alice) ── b1.pdf
//	      ├─ C (bob) ── c1.pdf
//	      └─ D ── E (ALICE@EXAMPLE.COM, same account, different case)
//
// Returns the row ids of root and A.
func buildReclaimTree(t *testing.T, db *sql.DB) (rootID, aID int64) {
	t.Helper()
	folder := func(driveID, name, owner string, parent int64) int64 {
		p := sql.NullInt64{Int64: parent, Valid: parent != 0}
		id, _, _, _ := mustUpsert(t, db, node{driveID: driveID, name: name, typ: typeFolder,
			mimeType: folderMimeType, ownerEmail: nullString(owner), parentID: p})
		return id
	}
	file := func(driveID, name string, parent int64) {
		mustUpsert(t, db, node{driveID: driveID, name: name, typ: typeBinary, mimeType: "application/pdf",
			parentID: sql.NullInt64{Int64: parent, Valid: true}})
	}
	rootID = folder("root", "Root", "me@example.com", 0)
	aID = folder("A", "A", "alice@example.com", rootID)
	file("a1", "a1.pdf", aID)
	bID := folder("B", "B", "alice@example.com", aID)
	file("b1", "b1.pdf", bID)
	cID := folder("C", "C", "bob@example.com", rootID)
	file("c1", "c1.pdf", cID)
	dID := folder("D", "D", "me@example.com", rootID)
	folder("E", "E", "ALICE@EXAMPLE.COM", dID)
	return rootID, aID
}

func TestFoldersOwnedBy(t *testing.T) {
	db := testDB(t)
	buildReclaimTree(t, db)

	got, err := foldersOwnedBy(db, "root", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// Shallowest first, and the differently-cased owner_email on E matches too.
	want := []reclaimTarget{
		{driveID: "A", name: "A", depth: 1},
		{driveID: "B", name: "B", depth: 2},
		{driveID: "E", name: "E", depth: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d folders %v, want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].driveID != w.driveID || got[i].name != w.name || got[i].depth != w.depth {
			t.Errorf("folder %d = %+v, want drive_id %s name %s depth %d", i, got[i], w.driveID, w.name, w.depth)
		}
		if got[i].rowID == 0 {
			t.Errorf("folder %d has no row id", i)
		}
	}
}

func TestFoldersOwnedByScoped(t *testing.T) {
	db := testDB(t)
	buildReclaimTree(t, db)

	// Scoping to A includes A itself at depth 0 — reclaim-folders replaces the scope
	// folder too, unless it is the crawl root.
	got, err := foldersOwnedBy(db, "A", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].driveID != "A" || got[0].depth != 0 || got[1].driveID != "B" {
		t.Fatalf("scoped to A = %+v, want A (depth 0) then B", got)
	}

	// Nobody else's folders come along.
	if got, err := foldersOwnedBy(db, "root", "nobody@example.com"); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Errorf("unknown owner returned %+v, want none", got)
	}
}

func TestReparentNode(t *testing.T) {
	db := testDB(t)
	rootID, aID := buildReclaimTree(t, db)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	found, err := reparentNode(tx, "a1", rootID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("reparenting a known node reported no row")
	}
	// An item created after the last crawl has no row to move.
	if found, err := reparentNode(tx, "never-crawled", aID); err != nil {
		t.Fatal(err)
	} else if found {
		t.Error("reparenting an unknown drive id reported a row")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var parent int64
	if err := db.QueryRow(`SELECT parent_id FROM nodes WHERE drive_id = 'a1'`).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if parent != rootID {
		t.Errorf("a1 parent = %d, want root %d", parent, rootID)
	}
}

func TestReclaimNames(t *testing.T) {
	cases := []struct{ current, origName, oldName string }{
		{"Finance", "Finance", "(old) Finance"},
		// Already renamed by an earlier run: the name is left alone.
		{"(old) Finance", "Finance", "(old) Finance"},
		// Degenerate name: trimming would leave nothing, so it stands as-is.
		{"(old) ", "(old) ", "(old) (old) "},
		{"(new) Finance", "(new) Finance", "(old) (new) Finance"},
	}
	for _, c := range cases {
		orig, old := reclaimNames(c.current)
		if orig != c.origName || old != c.oldName {
			t.Errorf("reclaimNames(%q) = (%q, %q), want (%q, %q)", c.current, orig, old, c.origName, c.oldName)
		}
	}
}

func TestIsShortcutTo(t *testing.T) {
	sc := &drive.File{MimeType: shortcutMimeType, ShortcutDetails: &drive.FileShortcutDetails{TargetId: "target"}}
	if !isShortcutTo(sc, "target") {
		t.Error("shortcut to target not recognised")
	}
	if isShortcutTo(sc, "other") {
		t.Error("shortcut matched the wrong target")
	}
	if isShortcutTo(&drive.File{MimeType: folderMimeType}, "target") {
		t.Error("a folder matched as a shortcut")
	}
	// A shortcut with no expanded details must not match anything.
	if isShortcutTo(&drive.File{MimeType: shortcutMimeType}, "target") {
		t.Error("a shortcut with no details matched")
	}
}

// testReclaimer builds a reclaimer that only ever touches the database —
// record() does no Drive I/O, so the whole bookkeeping step is testable with
// plain drive.File values.
func testReclaimer(db *sql.DB) *reclaimer {
	return &reclaimer{
		db: db,
		me: &drive.User{EmailAddress: "me@example.com", PermissionId: "me-pid", DisplayName: "Me"},
	}
}

func decisionOf(t *testing.T, db *sql.DB, driveID string) string {
	t.Helper()
	var d string
	if err := db.QueryRow(`SELECT decision FROM nodes WHERE drive_id = ?`, driveID).Scan(&d); err != nil {
		t.Fatalf("decision of %s: %v", driveID, err)
	}
	return d
}

func parentOf(t *testing.T, db *sql.DB, driveID string) int64 {
	t.Helper()
	var p sql.NullInt64
	if err := db.QueryRow(`SELECT parent_id FROM nodes WHERE drive_id = ?`, driveID).Scan(&p); err != nil {
		t.Fatalf("parent of %s: %v", driveID, err)
	}
	return p.Int64
}

// TestReclaimRecordEmptied covers the common case: everything moved across, so
// their folder is left holding nothing but the shortcut to ours. It is kept all
// the same, so links pointing at it keep working — without dragging the moved
// items into its decision.
func TestReclaimRecordEmptied(t *testing.T) {
	db := testDB(t)
	rootID, aRow := buildReclaimTree(t, db)
	r := testReclaimer(db)

	target := reclaimTarget{rowID: aRow, driveID: "A", name: "A"}
	theirs := &drive.File{Id: "A", Name: "(old) A"}
	ours := &drive.File{Id: "A-new", Name: "A"}
	backlink := &drive.File{Id: "A-link", Name: "(new) A"}

	if err := r.record(target, theirs, ours, "A", "root", []string{"a1", "B"}, backlink, nil, nil); err != nil {
		t.Fatal(err)
	}

	// The replacement is recorded beside their folder, owned by us, and fully
	// listed — every child it has is a row we just reparented into it.
	var (
		name, owner string
		parent      sql.NullInt64
		done        int
	)
	if err := db.QueryRow(`SELECT name, owner_email, parent_id, children_done FROM nodes WHERE drive_id = 'A-new'`).
		Scan(&name, &owner, &parent, &done); err != nil {
		t.Fatal(err)
	}
	if name != "A" || owner != "me@example.com" || parent.Int64 != rootID || done != 1 {
		t.Errorf("replacement row = (%q, %q, parent %v, done %d), want (\"A\", \"me@example.com\", %d, 1)",
			name, owner, parent, done, rootID)
	}
	newRow := parentOf(t, db, "a1")
	if newRow == aRow {
		t.Fatal("a1 was not reparented out of their folder")
	}
	if parentOf(t, db, "B") != newRow {
		t.Error("B was not reparented into the replacement")
	}
	if parentOf(t, db, "A-link") != aRow {
		t.Error("the (new) shortcut was not recorded inside their folder")
	}

	// Their folder and the shortcut left inside it are keep; the contents that
	// moved out keep their own (undecided) decisions.
	if got := decisionOf(t, db, "A"); got != decisionKeep {
		t.Errorf("their folder = %q, want keep", got)
	}
	if got := decisionOf(t, db, "A-link"); got != decisionKeep {
		t.Errorf("the (new) shortcut = %q, want keep (it goes with the folder)", got)
	}
	for _, id := range []string{"a1", "B", "b1"} {
		if got := decisionOf(t, db, id); got != decisionNone {
			t.Errorf("%s = %q, want undecided — it moved out before the keep", id, got)
		}
	}
}

// TestReclaimRecordKeepsStuckContent covers a folder nothing could be moved out
// of: an earlier delete on it is replaced by the keep, its undecided leftovers
// are kept with it, and a leftover already marked delete stays delete.
func TestReclaimRecordKeepsStuckContent(t *testing.T) {
	db := testDB(t)
	_, aRow := buildReclaimTree(t, db)
	setDecision(t, db, "A", decisionDelete)
	setDecision(t, db, "b1", decisionDelete) // two levels down, already decided
	r := testReclaimer(db)

	target := reclaimTarget{rowID: aRow, driveID: "A", name: "A"}
	theirs := &drive.File{Id: "A", Name: "(old) A"}
	ours := &drive.File{Id: "A-new", Name: "A"}
	leftoverLink := &drive.File{Id: "A-old-link", Name: "(old) A"}

	if err := r.record(target, theirs, ours, "A", "root", nil, nil, leftoverLink, nil); err != nil {
		t.Fatal(err)
	}
	if got := decisionOf(t, db, "A"); got != decisionKeep {
		t.Errorf("their folder = %q, want keep", got)
	}
	if got := decisionOf(t, db, "a1"); got != decisionKeep {
		t.Errorf("undecided leftover a1 = %q, want keep", got)
	}
	if got := decisionOf(t, db, "b1"); got != decisionDelete {
		t.Errorf("already-deleted leftover b1 = %q, want delete", got)
	}
	// The shortcut back to their folder lives in ours.
	var newRow int64
	if err := db.QueryRow(`SELECT id FROM nodes WHERE drive_id = 'A-new'`).Scan(&newRow); err != nil {
		t.Fatal(err)
	}
	if parentOf(t, db, "A-old-link") != newRow {
		t.Error("the (old) shortcut was not recorded inside the replacement")
	}
}

// TestReclaimRecordLeavesUncrawledChildrenPending checks that a moved item with
// no row (created since the last crawl) leaves the replacement flagged for the
// next crawl to list.
func TestReclaimRecordLeavesUncrawledChildrenPending(t *testing.T) {
	db := testDB(t)
	_, aRow := buildReclaimTree(t, db)
	r := testReclaimer(db)

	if err := r.record(reclaimTarget{rowID: aRow, driveID: "A", name: "A"},
		&drive.File{Id: "A", Name: "(old) A"}, &drive.File{Id: "A-new", Name: "A"},
		"A", "root", []string{"a1", "created-after-the-crawl"}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	var done int
	if err := db.QueryRow(`SELECT children_done FROM nodes WHERE drive_id = 'A-new'`).Scan(&done); err != nil {
		t.Fatal(err)
	}
	if done != 0 {
		t.Error("replacement marked fully listed despite holding an item with no row")
	}
}

func TestPermissionKey(t *testing.T) {
	cases := []struct {
		p    *drive.Permission
		want string
	}{
		{&drive.Permission{Type: "user", EmailAddress: "Alice@Example.com"}, "user:alice@example.com"},
		{&drive.Permission{Type: "group", EmailAddress: "Team@Example.com"}, "group:team@example.com"},
		{&drive.Permission{Type: "domain", Domain: "Example.COM"}, "domain:example.com"},
		{&drive.Permission{Type: "anyone"}, "anyone"},
	}
	for _, c := range cases {
		if got := permissionKey(c.p); got != c.want {
			t.Errorf("permissionKey(%+v) = %q, want %q", c.p, got, c.want)
		}
	}
	// The role and the permission id are deliberately not part of the key: the
	// same grantee must match across their folder and ours.
	a := permissionKey(&drive.Permission{Id: "1", Type: "user", EmailAddress: "a@x.com", Role: "reader"})
	b := permissionKey(&drive.Permission{Id: "2", Type: "user", EmailAddress: "a@x.com", Role: "writer"})
	if a != b {
		t.Errorf("same grantee keyed differently: %q vs %q", a, b)
	}
}

func TestShouldCopyPermission(t *testing.T) {
	r := testReclaimer(testDB(t))
	cases := []struct {
		name string
		p    *drive.Permission
		want bool
	}{
		{"ordinary user grant", &drive.Permission{Type: "user", Role: "writer", EmailAddress: "a@x.com"}, true},
		{"group grant", &drive.Permission{Type: "group", Role: "reader", EmailAddress: "team@x.com"}, true},
		{"domain grant", &drive.Permission{Type: "domain", Role: "reader", Domain: "x.com"}, true},
		{"anyone grant", &drive.Permission{Type: "anyone", Role: "reader"}, true},
		{"their ownership", &drive.Permission{Type: "user", Role: "owner", EmailAddress: "a@x.com"}, false},
		{"deleted user", &drive.Permission{Type: "user", Role: "writer", EmailAddress: "a@x.com", Deleted: true}, false},
		{"us by email", &drive.Permission{Type: "user", Role: "writer", EmailAddress: "ME@example.com"}, false},
		{"us by permission id", &drive.Permission{Type: "user", Role: "writer", Id: "me-pid"}, false},
		{"user with no email", &drive.Permission{Type: "user", Role: "writer"}, false},
		{"domain with no domain", &drive.Permission{Type: "domain", Role: "reader"}, false},
	}
	for _, c := range cases {
		if got := shouldCopyPermission(c.p, r.me); got != c.want {
			t.Errorf("%s: shouldCopyPermission = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestReclaimRecordStoresPermissions checks that the replacement's sharing
// lands in folder_permissions, so the snapshot says who can reach it.
func TestReclaimRecordStoresPermissions(t *testing.T) {
	db := testDB(t)
	_, aRow := buildReclaimTree(t, db)
	r := testReclaimer(db)

	perms := []*drive.Permission{
		{Id: "p-owner", Type: "user", Role: "owner", EmailAddress: "me@example.com"},
		{Id: "p-alice", Type: "user", Role: "writer", EmailAddress: "alice@example.com"},
		{Id: "p-domain", Type: "domain", Role: "reader", Domain: "example.com"},
	}
	if err := r.record(reclaimTarget{rowID: aRow, driveID: "A", name: "A"},
		&drive.File{Id: "A", Name: "(old) A"}, &drive.File{Id: "A-new", Name: "A"},
		"A", "root", []string{"a1"}, nil, nil, perms); err != nil {
		t.Fatal(err)
	}
	stored, err := folderPermissionsFor(db, "A-new")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(perms) {
		t.Fatalf("stored %d permission(s), want %d", len(stored), len(perms))
	}
	byID := map[string]permission{}
	for _, p := range stored {
		byID[p.permissionID] = p
	}
	if got := byID["p-alice"]; got.typ != "user" || got.role != "writer" || got.emailAddress.String != "alice@example.com" {
		t.Errorf("alice's grant stored as %+v", got)
	}
	// allowFileDiscovery is only meaningful for domain/anyone grants.
	if got := byID["p-domain"]; got.domain.String != "example.com" || !got.allowFileDiscovery.Valid {
		t.Errorf("domain grant stored as %+v", got)
	}
	if got := byID["p-alice"]; got.allowFileDiscovery.Valid {
		t.Error("allowFileDiscovery recorded for a user grant")
	}
}

func TestPermissionRoleRank(t *testing.T) {
	// Ordering is what decides whether a grant is an upgrade or a no-op.
	ordered := []string{"reader", "commenter", "writer", "fileOrganizer", "organizer", "owner"}
	for i := 1; i < len(ordered); i++ {
		if permissionRoleRank(ordered[i-1]) >= permissionRoleRank(ordered[i]) {
			t.Errorf("%s does not rank below %s", ordered[i-1], ordered[i])
		}
	}
	// An unknown role ranks below everything, so it is copied rather than
	// assumed to be already covered.
	if permissionRoleRank("someNewRole") >= permissionRoleRank("reader") {
		t.Error("an unrecognised role must rank below reader")
	}
}
