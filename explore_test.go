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
	for _, want := range []string{"alice@example.com", "budget.xlsx", "Finance", "plan.doc", "role=\"tree\""} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	if strings.Contains(html, "notes.txt") {
		t.Error("HTML should not contain bob's notes.txt")
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
