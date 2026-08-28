package main

import (
	"database/sql"
	"strings"
	"testing"

	"google.golang.org/api/drive/v3"
)

// assertContainsAll fails when the message is missing any of the substrings.
// The refusals evict-externals raises are the whole user interface for "what do
// I do now", so the tests assert they actually name the fix.
func assertContainsAll(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("message does not mention %q:\n%s", w, got)
		}
	}
}

var evictMe = &drive.User{EmailAddress: "me@example.com", PermissionId: "me-pid", DisplayName: "Me"}

var evictDomains = []string{"example.com"}

// buildEvictTree builds the fixture the eviction tests share — a folder that has
// already been through reclaim-folders, so every externally-owned folder is
// emptied down to its "(new) <name>" shortcut:
//
//	Team (me) ─┬─ report.doc (me)
//	           ├─ notes.doc (stranger@other.com)          <- evict, leave a shortcut
//	           ├─ Photos (me) ── snap.jpg (ext@other.com) <- evict, leave a shortcut
//	           ├─ (old) Plans (ext@other.com) ── (new) Plans -> shortcut (me)
//	           │                                          <- evict the folder whole
//	           └─ Empty (ext@other.com)                   <- evict the folder whole
//
// Returns the nodes, shallowest first, exactly as subtreeNodes yields them.
func buildEvictTree(t *testing.T, db *sql.DB) []evictNode {
	t.Helper()
	folder := func(driveID, name, owner string, parent int64) int64 {
		id, _, _, _ := mustUpsert(t, db, node{driveID: driveID, name: name, typ: typeFolder,
			mimeType: folderMimeType, ownerEmail: nullString(owner),
			parentID: sql.NullInt64{Int64: parent, Valid: parent != 0}}, true)
		return id
	}
	file := func(driveID, name, owner string, parent int64) int64 {
		id, _, _, _ := mustUpsert(t, db, node{driveID: driveID, name: name, typ: typeBinary,
			mimeType: "application/pdf", ownerEmail: nullString(owner),
			parentID: sql.NullInt64{Int64: parent, Valid: true}}, true)
		return id
	}

	rootID := folder("root", "Root", "me@example.com", 0)
	teamID := folder("Team", "Team", "me@example.com", rootID)
	file("report", "report.doc", "me@example.com", teamID)
	file("notes", "notes.doc", "stranger@other.com", teamID)
	photosID := folder("Photos", "Photos", "me@example.com", teamID)
	file("snap", "snap.jpg", "ext@other.com", photosID)
	plansID := folder("Plans-old", "(old) Plans", "ext@other.com", teamID)
	mustUpsert(t, db, node{driveID: "Plans-link", name: "(new) Plans", typ: typeShortcut,
		mimeType: shortcutMimeType, ownerEmail: nullString("me@example.com"),
		shortcutTarget: nullString("Plans-new"),
		parentID:       sql.NullInt64{Int64: plansID, Valid: true}}, true)
	folder("Empty", "Empty", "ext@other.com", teamID)

	nodes, err := subtreeNodes(db, "Team")
	if err != nil {
		t.Fatal(err)
	}
	return nodes
}

func driveIDsOf(nodes []evictNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.driveID)
	}
	return out
}

func sameIDs(got []evictNode, want ...string) bool {
	ids := driveIDsOf(got)
	if len(ids) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}

func TestSubtreeNodes(t *testing.T) {
	db := testDB(t)
	nodes := buildEvictTree(t, db)

	// The subtree root comes first, at depth 0, and the walk stops at it — "Root"
	// and anything else outside Team is not included.
	if len(nodes) == 0 || nodes[0].driveID != "Team" || nodes[0].depth != 0 {
		t.Fatalf("first node = %+v, want Team at depth 0", nodes[0])
	}
	if !sameIDs(nodes, "Team", "report", "notes", "Photos", "snap", "Plans-old", "Plans-link", "Empty") {
		t.Fatalf("subtree = %v", driveIDsOf(nodes))
	}
	byID := map[string]evictNode{}
	for _, n := range nodes {
		byID[n.driveID] = n
	}
	if got := byID["snap"]; got.depth != 2 || got.parentDriveID.String != "Photos" {
		t.Errorf("snap = %+v, want depth 2 under Photos", got)
	}
	if got := byID["notes"]; got.ownerEmail.String != "stranger@other.com" || got.typ != typeBinary {
		t.Errorf("notes = %+v", got)
	}
	if got := byID["Plans-link"]; got.typ != typeShortcut {
		t.Errorf("(new) Plans recorded as %q, want a shortcut", got.typ)
	}
}

func TestExternallyOwned(t *testing.T) {
	cases := []struct {
		name string
		n    evictNode
		want bool
	}{
		{"the running account", evictNode{ownerEmail: nullString("me@example.com")}, false},
		{"the running account, other case", evictNode{ownerEmail: nullString("ME@EXAMPLE.COM")}, false},
		{"the running account by permission id", evictNode{ownerID: nullString("me-pid")}, false},
		{"a colleague in the org", evictNode{ownerEmail: nullString("colleague@example.com")}, false},
		{"a colleague, other case", evictNode{ownerEmail: nullString("Colleague@Example.COM")}, false},
		{"somebody outside", evictNode{ownerEmail: nullString("stranger@other.com")}, true},
		// An owner we could not read is not one we can vouch for; a shared drive
		// would reject it, so it counts as external.
		{"no recorded owner", evictNode{}, true},
	}
	for _, c := range cases {
		if got := externallyOwned(c.n, evictMe, evictDomains); got != c.want {
			t.Errorf("%s: externallyOwned = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsLeafFolder(t *testing.T) {
	shortcut := evictNode{driveID: "sc", typ: typeShortcut}
	file := evictNode{driveID: "f", typ: typeBinary}
	folder := evictNode{driveID: "d", typ: typeFolder}
	cases := []struct {
		name     string
		children []evictNode
		want     bool
	}{
		{"empty", nil, true},
		{"just the reclaim-folders link", []evictNode{shortcut}, true},
		{"one real file", []evictNode{file}, false},
		{"one subfolder", []evictNode{folder}, false},
		{"two shortcuts", []evictNode{shortcut, shortcut}, false},
		{"a link and a file", []evictNode{shortcut, file}, false},
	}
	for _, c := range cases {
		if got := isLeafFolder(c.children); got != c.want {
			t.Errorf("%s: isLeafFolder = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestExternalsReplicaNames covers the naming of the externals tree: the replica
// prefix, the back-link prefix, and the rune-safe truncation both share with the
// archive tree's "ARCH " replicas.
func TestExternalsReplicaNames(t *testing.T) {
	if got := externalsReplicaName("Finance"); got != "(ext) Finance" {
		t.Errorf("externalsReplicaName = %q, want %q", got, "(ext) Finance")
	}
	if got := externalsBackLinkName("Finance"); got != "((new)) Finance" {
		t.Errorf("externalsBackLinkName = %q, want %q", got, "((new)) Finance")
	}
	if got := externalsForwardLinkName("Finance"); got != "(external files) Finance" {
		t.Errorf("externalsForwardLinkName = %q, want %q", got, "(external files) Finance")
	}
	// A replica must never be named like the canonical folder it mirrors, nor
	// like an archive replica.
	if externalsReplicaName("Finance") == "Finance" || externalsReplicaName("Finance") == replicaName("Finance") {
		t.Error("the externals replica name does not distinguish itself")
	}
	long := strings.Repeat("é", 300)
	for _, got := range []string{externalsReplicaName(long), externalsBackLinkName(long), externalsForwardLinkName(long)} {
		if r := []rune(got); len(r) != maxReplicaNameRunes {
			t.Errorf("truncated length = %d runes, want %d", len(r), maxReplicaNameRunes)
		}
	}
	if !strings.HasPrefix(externalsReplicaName(long), extReplicaPrefix) {
		t.Error("a truncated replica name lost its prefix")
	}
	if !strings.HasPrefix(externalsBackLinkName(long), extBackLinkPrefix) {
		t.Error("a truncated back-link name lost its prefix")
	}
	if !strings.HasPrefix(externalsForwardLinkName(long), extForwardLinkPrefix) {
		t.Error("a truncated forward-link name lost its prefix")
	}
}

// TestRecordLink covers both signposts landing in the snapshot under the folder
// that actually holds them: the replica's link back to the original, and the
// original's link to its replica.
func TestRecordLink(t *testing.T) {
	db := testDB(t)
	nodes := buildEvictTree(t, db)
	var teamRow int64
	for _, n := range nodes {
		if n.driveID == "Team" {
			teamRow = n.rowID
		}
	}
	replicaRow, err := upsertReplicaRow(db, "Team-replica", "(ext) Team", sql.NullInt64{}, "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	e := &evictor{db: db, me: evictMe}

	back := &drive.File{Id: "team-backlink", Name: externalsBackLinkName("Team")}
	if err := e.recordLink(back, "Team", replicaRow); err != nil {
		t.Fatal(err)
	}
	forward := &drive.File{Id: "team-forwardlink", Name: externalsForwardLinkName("Team")}
	if err := e.recordLink(forward, "Team-replica", teamRow); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		driveID    string
		wantParent int64
		wantTarget string
	}{
		{"team-backlink", replicaRow, "Team"},
		{"team-forwardlink", teamRow, "Team-replica"},
	} {
		var (
			parent int64
			target sql.NullString
			typ    string
		)
		if err := db.QueryRow(`SELECT parent_id, shortcut_target_id, type FROM nodes WHERE drive_id = ?`, tc.driveID).
			Scan(&parent, &target, &typ); err != nil {
			t.Fatalf("%s: %v", tc.driveID, err)
		}
		if parent != tc.wantParent || target.String != tc.wantTarget || typ != typeShortcut {
			t.Errorf("%s recorded under %d pointing at %q as %q; want %d -> %q as a shortcut",
				tc.driveID, parent, target.String, typ, tc.wantParent, tc.wantTarget)
		}
	}
}

// TestForwardLinkOnlyWhereItemsLeave checks the rule that decides which original
// folders get an "(external files)" shortcut: the ones whose replica actually
// receives items, not the ancestors created on the way to them.
func TestForwardLinkOnlyWhereItemsLeave(t *testing.T) {
	db := testDB(t)
	nodes := buildEvictTree(t, db)
	byID := map[string]evictNode{}
	for _, n := range nodes {
		byID[n.driveID] = n
	}
	plan, err := planEviction(nodes, evictMe, evictDomains, "Team", false)
	if err != nil {
		t.Fatal(err)
	}
	// Team parts with notes.doc and the two emptied folders, Photos with
	// snap.jpg; Root only holds Team, so its replica would hold only replicas.
	receiving := plan.receivingDriveIDs()
	if !receiving["Team"] || !receiving["Photos"] {
		t.Errorf("receivingDriveIDs = %v, want Team and Photos", receiving)
	}
	if receiving["root"] {
		t.Error("the crawl root parts with nothing of its own, so it must not be linked to a replica")
	}

	// A folder left out of this run's plan is still linked when an earlier run
	// evicted something out of it, which is what the snapshot records.
	e := &evictor{db: db, me: evictMe, receiving: receiving}
	photos := archiveTarget{rowID: byID["Photos"].rowID, driveID: "Photos", name: "Photos"}
	empty := archiveTarget{rowID: byID["Team"].rowID, driveID: "Nothing-left-here", name: "Elsewhere"}
	if e.givesUpItems(empty) {
		t.Error("a folder nothing has ever been evicted out of must not be linked to a replica")
	}
	replicaRow, err := upsertReplicaRow(db, "Photos-replica", "(ext) Photos", sql.NullInt64{}, "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.record(byID["snap"], replicaRef{driveID: "Photos-replica", rowID: replicaRow}, nil); err != nil {
		t.Fatal(err)
	}
	e.receiving = map[string]bool{}
	if !e.givesUpItems(photos) {
		t.Error("a folder an earlier run evicted snap.jpg out of must still be linked to its replica")
	}
}

// TestIsExternalsBackLink covers what delete's replica prune is allowed to
// discount: our own signpost shortcuts, and nothing else.
func TestIsExternalsBackLink(t *testing.T) {
	if !isExternalsBackLink("((new)) Finance", shortcutMimeType) {
		t.Error("a replica's back-link was not recognised")
	}
	// A folder of that name is not a shortcut, reclaim-folders' single-paren
	// links point the other way, and a real shortcut somebody put in a replica
	// still keeps it alive.
	if isExternalsBackLink("((new)) Finance", folderMimeType) {
		t.Error("a folder named like a back-link was taken for one")
	}
	if isExternalsBackLink("(new) Finance", shortcutMimeType) {
		t.Error("reclaim-folders' own link was taken for a back-link")
	}
	if isExternalsBackLink("Budget", shortcutMimeType) {
		t.Error("an ordinary shortcut was taken for a back-link")
	}
}

func TestLiveLeaf(t *testing.T) {
	sc := &drive.File{MimeType: shortcutMimeType}
	doc := &drive.File{MimeType: "application/pdf"}
	if !liveLeaf(nil) || !liveLeaf([]*drive.File{sc}) {
		t.Error("an empty folder and one holding a single shortcut must both count as leaves")
	}
	if liveLeaf([]*drive.File{doc}) || liveLeaf([]*drive.File{sc, sc}) {
		t.Error("a folder holding real content must not count as a leaf")
	}
}

func TestPlanEviction(t *testing.T) {
	db := testDB(t)
	nodes := buildEvictTree(t, db)

	plan, err := planEviction(nodes, evictMe, evictDomains, "Team", false)
	if err != nil {
		t.Fatal(err)
	}
	// notes.doc and snap.jpg are evicted individually; "(new) Plans" is not, even
	// though it sits in a folder that is leaving — it travels with the folder.
	if !sameIDs(plan.files, "notes", "snap") {
		t.Errorf("files to evict = %v, want notes and snap", driveIDsOf(plan.files))
	}
	if !sameIDs(plan.folders, "Plans-old", "Empty") {
		t.Errorf("folders to evict = %v, want (old) Plans and Empty", driveIDsOf(plan.folders))
	}
	// Both replica chains are needed, and each folder is asked for once.
	if got := plan.parentDriveIDs(); len(got) != 2 {
		t.Errorf("parents needing a replica = %v, want Team and Photos once each", got)
	}
}

func TestPlanEvictionNothingToDo(t *testing.T) {
	db := testDB(t)
	// A folder holding only the org's own material needs no eviction at all.
	rootID := mustUpsertFolder(t, db, "root", "Root", "me@example.com", 0)
	folderID := mustUpsertFolder(t, db, "Owned", "Owned", "colleague@example.com", rootID)
	mustUpsert(t, db, node{driveID: "doc", name: "doc.pdf", typ: typeBinary, mimeType: "application/pdf",
		ownerEmail: nullString("me@example.com"), parentID: sql.NullInt64{Int64: folderID, Valid: true}}, true)

	nodes, err := subtreeNodes(db, "Owned")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planEviction(nodes, evictMe, evictDomains, "Owned", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.files)+len(plan.folders) != 0 {
		t.Errorf("plan = %+v, want nothing to evict", plan)
	}
}

func TestPlanEvictionRefusesDeleteMarked(t *testing.T) {
	db := testDB(t)
	buildEvictTree(t, db)
	if _, err := db.Exec(`UPDATE nodes SET decision = ? WHERE drive_id = 'notes'`, decisionDelete); err != nil {
		t.Fatal(err)
	}
	nodes, err := subtreeNodes(db, "Team")
	if err != nil {
		t.Fatal(err)
	}
	_, err = planEviction(nodes, evictMe, evictDomains, "Team", false)
	if err == nil {
		t.Fatal("a delete-marked item did not stop the run")
	}
	assertContainsAll(t, err.Error(), "marked delete", "archive", "notes.doc")
}

func TestPlanEvictionRefusesFolderWithContent(t *testing.T) {
	db := testDB(t)
	buildEvictTree(t, db)
	// Somebody put a real file back into the folder reclaim-folders had emptied.
	var plansRow int64
	if err := db.QueryRow(`SELECT id FROM nodes WHERE drive_id = 'Plans-old'`).Scan(&plansRow); err != nil {
		t.Fatal(err)
	}
	mustUpsert(t, db, node{driveID: "stuck", name: "stuck.pdf", typ: typeBinary, mimeType: "application/pdf",
		ownerEmail: nullString("me@example.com"), parentID: sql.NullInt64{Int64: plansRow, Valid: true}}, true)

	nodes, err := subtreeNodes(db, "Team")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planEviction(nodes, evictMe, evictDomains, "Team", false)
	if err == nil {
		t.Fatal("an externally-owned folder still holding content did not stop the run")
	}
	// The message has to name both ways out and whom to run reclaim-folders for.
	assertContainsAll(t, err.Error(), "reclaim-folders", "ext@other.com", "--allow-unowned-folders")
	// The half-built plan behind the refusal is not used, but the offending
	// folders are still reported, with where they are and how much they hold.
	if len(plan.stuffed) != 1 || plan.stuffed[0].node.driveID != "Plans-old" {
		t.Fatalf("folders still holding content = %+v, want just (old) Plans", plan.stuffed)
	}
	// "(new) Plans" and the file somebody put back: both of them count.
	if got := plan.stuffed[0]; got.path != "(old) Plans" || got.items != 2 {
		t.Errorf("reported folder = %+v, want path %q holding 2 items", got, "(old) Plans")
	}
}

// TestPlanEvictionAllowUnownedFolders covers --allow-unowned-folders: the folder
// that still holds content is evicted like any other leaf, and everything below
// it travels with it instead of being evicted in its own right.
func TestPlanEvictionAllowUnownedFolders(t *testing.T) {
	db := testDB(t)
	buildEvictTree(t, db)
	var plansRow int64
	if err := db.QueryRow(`SELECT id FROM nodes WHERE drive_id = 'Plans-old'`).Scan(&plansRow); err != nil {
		t.Fatal(err)
	}
	// A file of ours, an unowned file, and a whole unowned subfolder with an
	// unowned file of its own — none of which may be evicted separately.
	mustUpsert(t, db, node{driveID: "stuck", name: "stuck.pdf", typ: typeBinary, mimeType: "application/pdf",
		ownerEmail: nullString("me@example.com"), parentID: sql.NullInt64{Int64: plansRow, Valid: true}}, true)
	mustUpsert(t, db, node{driveID: "theirs", name: "theirs.pdf", typ: typeBinary, mimeType: "application/pdf",
		ownerEmail: nullString("ext@other.com"), parentID: sql.NullInt64{Int64: plansRow, Valid: true}}, true)
	subRow, _, _, _ := mustUpsert(t, db, node{driveID: "Deep", name: "Deep", typ: typeFolder,
		mimeType: folderMimeType, ownerEmail: nullString("ext@other.com"),
		parentID: sql.NullInt64{Int64: plansRow, Valid: true}}, true)
	mustUpsert(t, db, node{driveID: "deepfile", name: "deep.pdf", typ: typeBinary, mimeType: "application/pdf",
		ownerEmail: nullString("ext@other.com"), parentID: sql.NullInt64{Int64: subRow, Valid: true}}, true)

	nodes, err := subtreeNodes(db, "Team")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planEviction(nodes, evictMe, evictDomains, "Team", true)
	if err != nil {
		t.Fatalf("--allow-unowned-folders did not clear the refusal: %v", err)
	}
	// (old) Plans leaves whole; "Deep" is inside it, so it is not evicted on its
	// own, and neither is anything below either of them.
	if !sameIDs(plan.folders, "Plans-old", "Empty") {
		t.Errorf("folders to evict = %v, want (old) Plans and Empty", driveIDsOf(plan.folders))
	}
	if !sameIDs(plan.files, "notes", "snap") {
		t.Errorf("files to evict = %v, want notes and snap only", driveIDsOf(plan.files))
	}
	if len(plan.stuffed) != 1 || !plan.carriedDriveIDs()["Plans-old"] {
		t.Errorf("folders leaving with their contents = %+v, want (old) Plans", plan.stuffed)
	}
	// The wording of what is about to happen has to stop claiming these folders
	// are empty.
	if plan.folderNoun() == "emptied folder(s)" {
		t.Error("folders leaving with their contents are still described as emptied")
	}
	// notes.doc and snap.jpg, plus one for (old) Plans — which, unlike an emptied
	// folder, has no "(new) <name>" link of its own to travel with it. "Empty"
	// still gets none.
	if got := plan.shortcutCount(); got != 3 {
		t.Errorf("shortcuts to be left behind = %d, want 3", got)
	}
}

func TestPlanEvictionRefusesUnownedSubtreeRoot(t *testing.T) {
	db := testDB(t)
	rootID := mustUpsertFolder(t, db, "root", "Root", "me@example.com", 0)
	mustUpsertFolder(t, db, "Theirs", "Theirs", "stranger@other.com", rootID)

	nodes, err := subtreeNodes(db, "Theirs")
	if err != nil {
		t.Fatal(err)
	}
	_, err = planEviction(nodes, evictMe, evictDomains, "Theirs", false)
	if err == nil {
		t.Fatal("preparing a folder somebody else owns did not fail")
	}
	assertContainsAll(t, err.Error(), "reclaim-folders", "stranger@other.com")
}

// TestEvictRecordFile covers the bookkeeping for one evicted file: it is stamped
// with the folder it came out of, reparented under the replica, and the shortcut
// left in its place becomes a row in the folder it came from.
func TestEvictRecordFile(t *testing.T) {
	db := testDB(t)
	nodes := buildEvictTree(t, db)
	byID := map[string]evictNode{}
	for _, n := range nodes {
		byID[n.driveID] = n
	}
	teamRow := byID["Team"].rowID

	// Stand in for the replica of "Team" inside the externals tree.
	replicaRow, err := upsertReplicaRow(db, "Team-replica", "Team", sql.NullInt64{}, "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	e := &evictor{db: db, me: evictMe}
	shortcut := &drive.File{Id: "notes-link", Name: "notes.doc"}
	if err := e.record(byID["notes"], replicaRef{driveID: "Team-replica", rowID: replicaRow}, shortcut); err != nil {
		t.Fatal(err)
	}

	var (
		from   sql.NullString
		parent int64
	)
	if err := db.QueryRow(`SELECT evicted_from_drive_id, parent_id FROM nodes WHERE drive_id = 'notes'`).
		Scan(&from, &parent); err != nil {
		t.Fatal(err)
	}
	if from.String != "Team" {
		t.Errorf("evicted_from_drive_id = %q, want Team", from.String)
	}
	if parent != replicaRow {
		t.Errorf("notes.doc reparented to %d, want the replica row %d", parent, replicaRow)
	}

	var (
		scParent int64
		scTarget sql.NullString
		scType   string
	)
	if err := db.QueryRow(`SELECT parent_id, shortcut_target_id, type FROM nodes WHERE drive_id = 'notes-link'`).
		Scan(&scParent, &scTarget, &scType); err != nil {
		t.Fatal(err)
	}
	if scParent != teamRow || scTarget.String != "notes" || scType != typeShortcut {
		t.Errorf("shortcut recorded under %d pointing at %q as %q; want %d -> notes as a shortcut",
			scParent, scTarget.String, scType, teamRow)
	}
}

// TestMarkEvictedKeepsFirstOrigin checks that a second eviction of the same item
// cannot overwrite where it originally came from.
func TestMarkEvictedKeepsFirstOrigin(t *testing.T) {
	db := testDB(t)
	buildEvictTree(t, db)
	first, err := upsertReplicaRow(db, "r1", "r1", sql.NullInt64{}, "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	second, err := upsertReplicaRow(db, "r2", "r2", sql.NullInt64{}, "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	mustMarkEvicted(t, db, "notes", "Team", first)
	mustMarkEvicted(t, db, "notes", "r1", second)

	var (
		from   sql.NullString
		parent int64
	)
	if err := db.QueryRow(`SELECT evicted_from_drive_id, parent_id FROM nodes WHERE drive_id = 'notes'`).
		Scan(&from, &parent); err != nil {
		t.Fatal(err)
	}
	if from.String != "Team" {
		t.Errorf("evicted_from_drive_id = %q after a second eviction, want the original Team", from.String)
	}
	if parent != second {
		t.Errorf("parent_id = %d, want the newest replica %d", parent, second)
	}
}

// TestEvictedRowsSurviveStalePruning checks that an evicted item is not swept
// away by a crawl that never visits the externals tree — the row is the only
// record of where the item came from.
func TestEvictedRowsSurviveStalePruning(t *testing.T) {
	db := testDB(t)
	buildEvictTree(t, db)
	replicaRow, err := upsertReplicaRow(db, "Team-replica", "Team", sql.NullInt64{}, "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	mustMarkEvicted(t, db, "notes", "Team", replicaRow)

	// Sweep with a cutoff in the future, so every row looks un-re-observed.
	removed, err := deleteStaleNodes(db, "2999-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if removed == 0 {
		t.Fatal("the sweep removed nothing, so it is not testing anything")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE drive_id = 'notes'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("the evicted item's row was pruned")
	}
}

// mustUpsertFolder inserts one folder row and returns its row id; parent 0 means
// no parent, i.e. a crawl root.
func mustUpsertFolder(t *testing.T, db *sql.DB, driveID, name, owner string, parent int64) int64 {
	t.Helper()
	id, _, _, _ := mustUpsert(t, db, node{driveID: driveID, name: name, typ: typeFolder,
		mimeType: folderMimeType, ownerEmail: nullString(owner),
		parentID: sql.NullInt64{Int64: parent, Valid: parent != 0}}, true)
	return id
}

func mustMarkEvicted(t *testing.T, db *sql.DB, driveID, fromParent string, replicaRow int64) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := markEvicted(tx, driveID, fromParent, replicaRow); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
