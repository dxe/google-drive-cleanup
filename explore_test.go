package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildExploreTree seeds a small Drive:
//
//	Root
//	├── Finance (owned by alice)
//	│   ├── budget.xlsx (owned by alice)
//	│   └── notes.txt   (owned by bob)
//	└── Shared
//	    └── plan.doc    (owned by alice)
//
// Root and Shared are owned by bob.
func buildExploreTree(t *testing.T, db *sql.DB) {
	t.Helper()
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder,
		mimeType: folderMimeType, ownerEmail: nullString("bob@example.com")})
	finID, _, _, _ := mustUpsert(t, db, node{driveID: "fin", name: "Finance", typ: typeFolder,
		mimeType: folderMimeType, ownerEmail: nullString("alice@example.com"), ownerDisplay: nullString("Alice"),
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	sharedID, _, _, _ := mustUpsert(t, db, node{driveID: "shared", name: "Shared", typ: typeFolder,
		mimeType: folderMimeType, ownerEmail: nullString("bob@example.com"),
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})

	mustUpsert(t, db, node{driveID: "budget", name: "budget.xlsx", typ: typeBinary, mimeType: "application/x",
		ownerEmail: nullString("alice@example.com"), ownerDisplay: nullString("Alice"),
		parentID: sql.NullInt64{Int64: finID, Valid: true}})
	mustUpsert(t, db, node{driveID: "notes", name: "notes.txt", typ: typeBinary, mimeType: "text/plain",
		ownerEmail: nullString("bob@example.com"),
		parentID:   sql.NullInt64{Int64: finID, Valid: true}})
	mustUpsert(t, db, node{driveID: "plan", name: "plan.doc", typ: typeGoogleDoc, mimeType: "application/vnd.google-apps.document",
		ownerEmail: nullString("alice@example.com"),
		parentID:   sql.NullInt64{Int64: sharedID, Valid: true}})
}

// flatten indexes every node in the forest by drive id.
func flatten(roots []*exploreNode) map[string]*exploreNode {
	m := make(map[string]*exploreNode)
	var walk func(*exploreNode)
	walk = func(n *exploreNode) {
		m[n.driveID] = n
		for _, c := range n.children {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return m
}

func TestOwnedAndAncestors(t *testing.T) {
	db := testDB(t)
	buildExploreTree(t, db)

	roots, displayName, err := ownedAndAncestors(db, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if displayName != "Alice" {
		t.Errorf("displayName = %q, want Alice", displayName)
	}
	if len(roots) != 1 || roots[0].driveID != "root" {
		t.Fatalf("roots = %v, want single root", roots)
	}

	byID := flatten(roots)
	// The included set is alice's items plus their ancestors only — bob's
	// notes.txt must be excluded even though it shares Finance.
	wantIncluded := []string{"root", "fin", "shared", "budget", "plan"}
	if len(byID) != len(wantIncluded) {
		t.Errorf("included %d nodes, want %d: %v", len(byID), len(wantIncluded), keys(byID))
	}
	for _, id := range wantIncluded {
		if byID[id] == nil {
			t.Errorf("missing expected node %q", id)
		}
	}
	if byID["notes"] != nil {
		t.Error("bob's notes.txt should not be included")
	}

	// owned flags
	if !byID["fin"].owned || !byID["budget"].owned || !byID["plan"].owned {
		t.Error("alice's items should be marked owned")
	}
	if byID["root"].owned || byID["shared"].owned {
		t.Error("bob's folders should not be marked owned")
	}

	// counts: subtree owned descendants, split by type, excluding self.
	if got := byID["root"]; got.ownedFolders != 1 || got.ownedFiles != 2 {
		t.Errorf("root counts = %d folders, %d files, want 1, 2", got.ownedFolders, got.ownedFiles)
	}
	if got := byID["fin"]; got.ownedFolders != 0 || got.ownedFiles != 1 {
		t.Errorf("Finance counts = %d folders, %d files, want 0, 1", got.ownedFolders, got.ownedFiles)
	}
	if got := byID["shared"]; got.ownedFolders != 0 || got.ownedFiles != 1 {
		t.Errorf("Shared counts = %d folders, %d files, want 0, 1", got.ownedFolders, got.ownedFiles)
	}

	// folders-first ordering under root: Finance, Shared (both folders, alpha).
	root := byID["root"]
	if len(root.children) != 2 || root.children[0].driveID != "fin" || root.children[1].driveID != "shared" {
		t.Errorf("root children order = %v, want [fin shared]", childIDs(root))
	}
}

func TestOwnedAndAncestorsByOwnerID(t *testing.T) {
	db := testDB(t)
	mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder, mimeType: folderMimeType})
	mustUpsert(t, db, node{driveID: "f", name: "f.pdf", typ: typeBinary, mimeType: "application/pdf",
		ownerID: nullString("999"), parentID: sql.NullInt64{Int64: 1, Valid: true}})

	roots, _, err := ownedAndAncestors(db, "999")
	if err != nil {
		t.Fatal(err)
	}
	byID := flatten(roots)
	if byID["f"] == nil || !byID["f"].owned {
		t.Error("expected node owned by owner id 999 to be included and owned")
	}
}

func TestOwnedAndAncestorsNoneOwned(t *testing.T) {
	db := testDB(t)
	buildExploreTree(t, db)
	if _, _, err := ownedAndAncestors(db, "nobody@example.com"); err == nil {
		t.Error("expected an error when the account owns nothing")
	}
}

func TestExternalOwnedAndAncestors(t *testing.T) {
	db := testDB(t)
	// Root/Finance are owned by an internal account; the two leaf files below
	// are owned by external accounts on two different domains, plus one node
	// owned only by an owner id (no email — treated external).
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder,
		mimeType: folderMimeType, ownerEmail: nullString("staff@example.com")})
	finID, _, _, _ := mustUpsert(t, db, node{driveID: "fin", name: "Finance", typ: typeFolder,
		mimeType: folderMimeType, ownerEmail: nullString("staff@example.com"),
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "a", name: "a.pdf", typ: typeBinary, mimeType: "application/pdf",
		ownerEmail: nullString("alice@partner.org"),
		parentID:   sql.NullInt64{Int64: finID, Valid: true}})
	mustUpsert(t, db, node{driveID: "b", name: "b.doc", typ: typeGoogleDoc, mimeType: "application/vnd.google-apps.document",
		ownerEmail: nullString("bob@vendor.io"),
		parentID:   sql.NullInt64{Int64: finID, Valid: true}})
	mustUpsert(t, db, node{driveID: "c", name: "c.txt", typ: typeBinary, mimeType: "text/plain",
		ownerID:  nullString("999"),
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})

	roots, err := externalOwnedAndAncestors(db, []string{"example.com"})
	if err != nil {
		t.Fatal(err)
	}
	byID := flatten(roots)

	// The two external files, the id-only node, and their ancestors are in;
	// the internal-owned folders are included as ancestors but not marked owned.
	for _, id := range []string{"a", "b", "c"} {
		if byID[id] == nil || !byID[id].owned {
			t.Errorf("external node %q should be included and owned", id)
		}
	}
	if byID["root"] == nil || byID["fin"] == nil {
		t.Fatal("ancestor folders should be included")
	}
	if byID["root"].owned || byID["fin"].owned {
		t.Error("internal-owned folders should not be marked owned")
	}
	// Pooled counts across all external accounts: root has 3 external files and
	// 0 external folders beneath it; Finance has 2.
	if got := byID["root"]; got.ownedFiles != 3 || got.ownedFolders != 0 {
		t.Errorf("root counts = %d files, %d folders, want 3, 0", got.ownedFiles, got.ownedFolders)
	}
	if got := byID["fin"]; got.ownedFiles != 2 {
		t.Errorf("Finance files = %d, want 2", got.ownedFiles)
	}

	// With no internal domains configured, the internal accounts become
	// external too, so every node is owned.
	all, err := externalOwnedAndAncestors(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	allByID := flatten(all)
	if !allByID["root"].owned || !allByID["fin"].owned {
		t.Error("with no internal domains, every owned node should be external")
	}

	// When every owner is internal, nothing is external.
	empty, err := externalOwnedAndAncestors(db, []string{"example.com", "partner.org", "vendor.io"})
	if err != nil {
		t.Fatal(err)
	}
	// The id-only node has no domain, so it remains external.
	remaining := flatten(empty)
	if remaining["c"] == nil {
		t.Error("owner-id-only node should still count as external")
	}
	if remaining["a"] != nil || remaining["b"] != nil {
		t.Error("email owners on internal domains should be excluded")
	}
}

func TestBuildOwnerBreakdowns(t *testing.T) {
	db := testDB(t)
	// Root(internal) > sub(external alice folder) > a.pdf(alice), b.doc(bob);
	// plus c.txt(id:999) directly under root.
	rootID, _, _, _ := mustUpsert(t, db, node{driveID: "root", name: "Root", typ: typeFolder,
		mimeType: folderMimeType, ownerEmail: nullString("staff@example.com")})
	subID, _, _, _ := mustUpsert(t, db, node{driveID: "sub", name: "Sub", typ: typeFolder,
		mimeType: folderMimeType, ownerEmail: nullString("alice@partner.org"), ownerDisplay: nullString("Alice"),
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	mustUpsert(t, db, node{driveID: "a", name: "a.pdf", typ: typeBinary, mimeType: "application/pdf",
		ownerEmail: nullString("alice@partner.org"), ownerDisplay: nullString("Alice"),
		parentID:   sql.NullInt64{Int64: subID, Valid: true}})
	mustUpsert(t, db, node{driveID: "b", name: "b.doc", typ: typeGoogleDoc, mimeType: "application/vnd.google-apps.document",
		ownerEmail: nullString("bob@vendor.io"),
		parentID:   sql.NullInt64{Int64: subID, Valid: true}})
	mustUpsert(t, db, node{driveID: "c", name: "c.txt", typ: typeBinary, mimeType: "text/plain",
		ownerID:  nullString("999"),
		parentID: sql.NullInt64{Int64: rootID, Valid: true}})
	// Decisions: alice's two items are keep, bob's file is delete, c.txt is left
	// undecided.
	setDecision(t, db, "sub", decisionKeep)
	setDecision(t, db, "a", decisionKeep)
	setDecision(t, db, "b", decisionDelete)

	roots, err := externalOwnedAndAncestors(db, []string{"example.com"})
	if err != nil {
		t.Fatal(err)
	}
	buildOwnerBreakdowns(roots)
	byID := flatten(roots)

	// Sub's breakdown: alice owns 1 file (a.pdf) here, bob owns 1 file. Sub
	// itself (an alice-owned folder) is excluded from its own breakdown.
	sub := breakdownMap(byID["sub"].breakdown)
	if got := sub["alice@partner.org (Alice)"]; got.Folders != 0 || got.Files != 1 {
		t.Errorf("sub alice = %+v, want 0 folders / 1 file", got)
	}
	if got := sub["bob@vendor.io"]; got.Files != 1 {
		t.Errorf("sub bob files = %d, want 1", got.Files)
	}
	// Decisions are tallied over the same items as Folders+Files.
	if got := sub["alice@partner.org (Alice)"].decisionCounts; got != (decisionCounts{Keep: 1}) {
		t.Errorf("sub alice decisions = %+v, want 1 keep", got)
	}
	if got := sub["bob@vendor.io"].decisionCounts; got != (decisionCounts{Delete: 1}) {
		t.Errorf("sub bob decisions = %+v, want 1 delete", got)
	}

	// Root's breakdown pools descendants: alice owns Sub (folder) + a.pdf, bob
	// owns b.doc, id:999 owns c.txt.
	root := breakdownMap(byID["root"].breakdown)
	if got := root["alice@partner.org (Alice)"]; got.Folders != 1 || got.Files != 1 {
		t.Errorf("root alice = %+v, want 1 folder / 1 file", got)
	}
	if got := root["id:999"]; got.Files != 1 {
		t.Errorf("root id:999 files = %d, want 1", got.Files)
	}
	// Pooled decisions: both of alice's items are keep; c.txt is undecided.
	if got := root["alice@partner.org (Alice)"].decisionCounts; got != (decisionCounts{Keep: 2}) {
		t.Errorf("root alice decisions = %+v, want 2 keep", got)
	}
	if got := root["id:999"].decisionCounts; got != (decisionCounts{Undecided: 1}) {
		t.Errorf("root id:999 decisions = %+v, want 1 undecided", got)
	}
	if len(byID["root"].breakdown) != 3 {
		t.Errorf("root breakdown has %d owners, want 3", len(byID["root"].breakdown))
	}

	// A folder's breakdown rows decompose exactly the tally shown on its own row.
	var sum decisionCounts
	for _, r := range byID["root"].breakdown {
		sum.addAll(r.decisionCounts)
	}
	if got := byID["root"].Desc(); got != sum {
		t.Errorf("root breakdown sums to %+v, want the row tally %+v", sum, got)
	}

	// The popover script reads these keys off each row, so decisionCounts must
	// flatten into the encoded object rather than nest.
	js, err := ownerBreakdownJSON(roots)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"folders":`, `"files":`, `"keep":`, `"delete":`, `"undecided":`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("breakdown JSON missing %q: %s", want, js)
		}
	}

	// Breakdown rows are ordered files-desc: alice and bob both have 1 file at
	// root, tie broken by total (alice has 2) then label.
	if byID["root"].breakdown[0].Label != "alice@partner.org (Alice)" {
		t.Errorf("root breakdown[0] = %q, want alice first", byID["root"].breakdown[0].Label)
	}

	// A per-account tree never computes breakdowns.
	acct, _, err := ownedAndAncestors(db, "alice@partner.org")
	if err != nil {
		t.Fatal(err)
	}
	for id, n := range flatten(acct) {
		if n.breakdown != nil {
			t.Errorf("per-account node %q unexpectedly has a breakdown", id)
		}
	}
}

func TestExploreDecisionStatus(t *testing.T) {
	db := testDB(t)
	buildExploreTree(t, db)
	setDecision(t, db, "budget", decisionKeep)
	setDecision(t, db, "plan", decisionDelete)
	// Everything else — including the folders — is left undecided.

	roots, _, err := ownedAndAncestors(db, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	byID := flatten(roots)

	// Files color by their own decision, folders by the tally of the owned items
	// beneath them (themselves included when they are owned).
	want := map[string]string{
		"budget": "keep",
		"plan":   "delete",
		// Finance is alice's, undecided, and holds one keep => pale green. bob's
		// notes.txt is not in alice's tree, so it does not count.
		"fin": "partial-keep",
		// Shared is bob's, so only the delete beneath it counts => solid red.
		"shared": "delete",
		// Root is bob's too and spans a keep, a delete and undecided Finance
		// => pale yellow.
		"root": "partial-mixed",
	}
	for id, wantStatus := range want {
		if got := byID[id].Status(); got != wantStatus {
			t.Errorf("%s status = %q, want %q", id, got, wantStatus)
		}
	}

	// Desc drops the folder's own contribution, so it matches the folder's
	// ownedFolders/ownedFiles counts: Finance holds just budget.xlsx.
	if got := byID["fin"].Desc(); got != (decisionCounts{Keep: 1}) {
		t.Errorf("Finance Desc = %+v, want 1 keep", got)
	}
	// Root is not owned, so nothing is subtracted: Finance (undecided) plus the
	// two files.
	if got := byID["root"].Desc(); got != (decisionCounts{Keep: 1, Delete: 1, Undecided: 1}) {
		t.Errorf("root Desc = %+v, want 1 keep / 1 delete / 1 undecided", got)
	}
	// The tally covers exactly the owned items the folder counts advertise.
	root := byID["root"]
	if got, want := root.Desc().Keep+root.Desc().Delete+root.Desc().Undecided,
		root.OwnedFolders()+root.OwnedFiles(); got != want {
		t.Errorf("root decision total = %d, want %d (owned folders + files)", got, want)
	}
}

func breakdownMap(rows []personCount) map[string]personCount {
	m := make(map[string]personCount, len(rows))
	for _, r := range rows {
		m[r.Label] = r
	}
	return m
}

func TestRunExploreOwnedFiles(t *testing.T) {
	db := testDB(t)
	buildExploreTree(t, db)
	db.Close() // runExploreOwnedFiles reopens by path

	dbPath := filepath.Join(t.TempDir(), "drive.db")
	freshDB, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	buildExploreTree(t, freshDB)
	setDecision(t, freshDB, "budget", decisionKeep)
	setDecision(t, freshDB, "plan", decisionDelete)
	freshDB.Close()

	outDir := filepath.Join(t.TempDir(), "out")
	if err := runExploreOwnedFiles(dbPath, "alice@example.com", outDir); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(outDir, "alice@example.com.html")
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("expected output file: %v", err)
	}
	html := string(b)
	for _, want := range []string{"alice@example.com", "budget.xlsx", "Finance", "plan.doc", "role=\"tree\"",
		// The per-account report carries the export-review decision coloring.
		`class="row st-keep"`, `class="row st-delete"`, `class="row st-partial-mixed"`,
		`<span class="chip chip-keep">keep</span>`} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	if strings.Contains(html, "notes.txt") {
		t.Error("HTML should not contain bob's notes.txt")
	}
}

func TestRunExploreOwnedFilesAllOwners(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "drive.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	buildExploreTree(t, db)
	db.Close() // runExploreOwnedFiles reopens by path

	outDir := filepath.Join(t.TempDir(), "out")
	// Empty account => one file per owner (alice and bob own things here).
	if err := runExploreOwnedFiles(dbPath, "", outDir); err != nil {
		t.Fatal(err)
	}

	for _, owner := range []string{"alice@example.com", "bob@example.com"} {
		if _, err := os.Stat(filepath.Join(outDir, owner+".html")); err != nil {
			t.Errorf("expected an HTML file for %s: %v", owner, err)
		}
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("wrote %d files, want 2 (alice, bob)", len(entries))
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"alice@example.com": "alice@example.com",
		"a/b\\c d":          "a_b_c_d",
		"":                  "account",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func keys(m map[string]*exploreNode) []string {
	var k []string
	for s := range m {
		k = append(k, s)
	}
	return k
}

func childIDs(n *exploreNode) []string {
	var ids []string
	for _, c := range n.children {
		ids = append(ids, c.driveID)
	}
	return ids
}
