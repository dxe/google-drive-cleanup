package main

import (
	"database/sql"
	"strings"
	"testing"
)

// buildExternalsTree builds the fixture these tests share: an externals tree as
// evict-externals leaves it, with two replica folders holding evicted files
// belonging to two different external owners, and one folder that is NOT a
// replica — an externally-owned folder evicted whole, which arrived with its
// contents and no back-link, so nobody can be told to drag anything in it.
//
//	zz Externals (me) ── (ext) Team (me) ─┬─ ((new)) Team -> shortcut (me)
//	                                      ├─ notes.doc      (stranger@other.com)
//	                                      ├─ apples.doc     (stranger@other.com)
//	                                      ├─ colleague.doc  (mate@example.com)   <- internal
//	                                      ├─ orphan.doc     (no owner recorded)
//	                                      ├─ Plans (ext@other.com) ── buried.doc (stranger@other.com)
//	                                      │                        <- no back-link: not a replica
//	                                      └─ (ext) Photos (me) ─┬─ ((new)) Photos -> shortcut (me)
//	                                                            ├─ snap.jpg (ext@other.com)
//	                                                            └─ (external files) Photos -> shortcut (ext@other.com)
func buildExternalsTree(t *testing.T, db *sql.DB) {
	t.Helper()
	folder := func(driveID, name, owner string, parent int64) int64 {
		id, _, _, _ := mustUpsert(t, db, node{driveID: driveID, name: name, typ: typeFolder,
			mimeType: folderMimeType, ownerEmail: nullString(owner),
			parentID: sql.NullInt64{Int64: parent, Valid: parent != 0}}, true)
		return id
	}
	file := func(driveID, name, owner string, parent int64) {
		mustUpsert(t, db, node{driveID: driveID, name: name, typ: typeBinary,
			mimeType: "application/pdf", ownerEmail: nullString(owner),
			parentID: sql.NullInt64{Int64: parent, Valid: true}}, true)
	}
	shortcut := func(driveID, name, owner string, parent int64) {
		mustUpsert(t, db, node{driveID: driveID, name: name, typ: typeShortcut,
			mimeType: shortcutMimeType, ownerEmail: nullString(owner),
			shortcutTarget: nullString("target-of-" + driveID),
			parentID:       sql.NullInt64{Int64: parent, Valid: true}}, true)
	}

	extRoot := folder("externals", "zz Externals", "me@example.com", 0)
	team := folder("ext-Team", "(ext) Team", "me@example.com", extRoot)
	shortcut("bl-Team", extBackLinkPrefix+"Team", "me@example.com", team)
	file("notes", "notes.doc", "stranger@other.com", team)
	file("apples", "apples.doc", "stranger@other.com", team)
	file("colleague", "colleague.doc", "mate@example.com", team)
	file("orphan", "orphan.doc", "", team)
	plans := folder("Plans", "Plans", "ext@other.com", team)
	file("buried", "buried.doc", "stranger@other.com", plans)
	photos := folder("ext-Photos", "(ext) Photos", "me@example.com", team)
	shortcut("bl-Photos", extBackLinkPrefix+"Photos", "me@example.com", photos)
	file("snap", "snap.jpg", "ext@other.com", photos)
	shortcut("fl-Photos", extForwardLinkPrefix+"Photos", "ext@other.com", photos)
}

func loadExternalsTree(t *testing.T) []externalsSubtreeNode {
	t.Helper()
	db := testDB(t)
	buildExternalsTree(t, db)
	nodes, err := externalsSubtree(db, "externals")
	if err != nil {
		t.Fatal(err)
	}
	return nodes
}

// The grouping is the whole command: which files count as somebody's to move,
// and which folder each is asked for from.
func TestGroupEvictedFilesByOwner(t *testing.T) {
	byOwner, unaddressable, outsideReplica := groupEvictedFilesByOwner(loadExternalsTree(t), evictDomains, "externals")

	if len(byOwner) != 2 {
		t.Fatalf("owners = %d, want 2 (stranger and ext); got %v", len(byOwner), byOwner)
	}
	// colleague.doc is internal, so its owner gets no draft at all.
	if _, ok := byOwner["mate@example.com"]; ok {
		t.Error("internal owner mate@example.com should not get a draft")
	}
	// orphan.doc is external but has nowhere to send a draft.
	if unaddressable != 1 {
		t.Errorf("unaddressable = %d, want 1 (orphan.doc)", unaddressable)
	}
	// buried.doc sits in an evicted-whole folder with no back-link to drag onto.
	if outsideReplica != 1 {
		t.Errorf("outsideReplica = %d, want 1 (buried.doc)", outsideReplica)
	}

	stranger := byOwner["stranger@other.com"]
	if len(stranger) != 1 {
		t.Fatalf("stranger folders = %d, want 1", len(stranger))
	}
	if stranger[0].folderDriveID != "ext-Team" {
		t.Errorf("stranger folder = %q, want ext-Team", stranger[0].folderDriveID)
	}
	if got := strings.Join(stranger[0].files, ","); got != "apples.doc,notes.doc" {
		t.Errorf("stranger files = %q, want sorted apples.doc,notes.doc "+
			"(buried.doc is in a non-replica folder)", got)
	}

	ext := byOwner["ext@other.com"]
	if len(ext) != 1 {
		t.Fatalf("ext folders = %d, want 1", len(ext))
	}
	// The "(external files) Photos" shortcut is ours by intent even though Drive
	// records ext@other.com as its owner: it is a signpost, not a file to move.
	if got := strings.Join(ext[0].files, ","); got != "snap.jpg" {
		t.Errorf("ext files = %q, want just snap.jpg (link shortcuts skipped)", got)
	}
}

// Files land under the folder they sit in now, and the folders come out ordered
// by path so a draft reads as a walk down the tree.
func TestGroupEvictedFilesByOwnerOrdersFoldersByPath(t *testing.T) {
	db := testDB(t)
	buildExternalsTree(t, db)
	// Give stranger a file in the nested replica too, so they have two folders.
	mustUpsert(t, db, node{driveID: "deep", name: "deep.doc", typ: typeBinary,
		mimeType: "application/pdf", ownerEmail: nullString("stranger@other.com"),
		parentID: sql.NullInt64{Int64: mustRowID(t, db, "ext-Photos"), Valid: true}}, true)

	nodes, err := externalsSubtree(db, "externals")
	if err != nil {
		t.Fatal(err)
	}
	byOwner, _, _ := groupEvictedFilesByOwner(nodes, evictDomains, "externals")
	groups := byOwner["stranger@other.com"]
	if len(groups) != 2 {
		t.Fatalf("folders = %d, want 2", len(groups))
	}
	if groups[0].folderPath != "zz Externals/(ext) Team" ||
		groups[1].folderPath != "zz Externals/(ext) Team/(ext) Photos" {
		t.Errorf("folders out of path order: %q then %q", groups[0].folderPath, groups[1].folderPath)
	}
}

func TestRenderEvictedFilesEmail(t *testing.T) {
	got := renderEvictedFilesEmail("stranger@other.com", []evictedFileGroup{
		{folderDriveID: "ext-Team", folderPath: "zz/Team", files: []string{"a.doc", "b.doc"}},
		{folderDriveID: "ext-Photos", folderPath: "zz/Team/Photos", files: []string{"snap.jpg"}},
	}, 5)

	assertContainsAll(t, got,
		"To: stranger@other.com",
		"Subject: Please move your files to our Shared Drive",
		"Body:",
		`"((new)) ..."`,
		"~~ https://drive.google.com/drive/folders/ext-Team ~~",
		"files you own in this folder:\na.doc\nb.doc\n",
		"~~ https://drive.google.com/drive/folders/ext-Photos ~~",
	)
	if strings.Contains(got, "and 0 more") {
		t.Error("an untruncated folder should not get a remainder line")
	}
}

// The sample is capped, and the cap is reported rather than silently hiding the
// rest — an owner with 40 files needs to know the ask is not just the five.
func TestRenderEvictedFilesEmailCapsTheSample(t *testing.T) {
	got := renderEvictedFilesEmail("stranger@other.com", []evictedFileGroup{
		{folderDriveID: "ext-Team", files: []string{"a", "b", "c", "d", "e", "f", "g"}},
	}, 5)

	assertContainsAll(t, got, "a\nb\nc\nd\ne\n... and 2 more\n")
	if strings.Contains(got, "\nf\n") || strings.Contains(got, "\ng\n") {
		t.Errorf("files past the cap should not be listed:\n%s", got)
	}
}

// A folder is a replica because it holds the back-link the email says to drag
// onto — not because of its name, and not because some original folder records
// it. A back-link pointing nowhere is no back-link at all.
func TestReplicaFolderRowIDsNeedsALiveBackLink(t *testing.T) {
	db := testDB(t)
	buildExternalsTree(t, db)
	// Strip the target from (ext) Photos' back-link: it now points nowhere.
	if _, err := db.Exec(`UPDATE nodes SET shortcut_target_id = NULL WHERE drive_id = 'bl-Photos'`); err != nil {
		t.Fatal(err)
	}
	nodes, err := externalsSubtree(db, "externals")
	if err != nil {
		t.Fatal(err)
	}

	byOwner, _, outsideReplica := groupEvictedFilesByOwner(nodes, evictDomains, "externals")
	if _, ok := byOwner["ext@other.com"]; ok {
		t.Error("snap.jpg's folder has a back-link pointing nowhere; it is not a replica")
	}
	// snap.jpg joins buried.doc as undraftable.
	if outsideReplica != 2 {
		t.Errorf("outsideReplica = %d, want 2 (buried.doc and snap.jpg)", outsideReplica)
	}
	// The (ext) Team replica is untouched and still drafted.
	if len(byOwner["stranger@other.com"]) != 1 {
		t.Error("the intact replica should still be drafted")
	}
}
