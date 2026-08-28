package main

import (
	"database/sql"
	"strings"
	"testing"

	"google.golang.org/api/drive/v3"
)

// replicaTree builds the archive side of buildArchiveTree: replicas of A and B
// (B's nested inside A's, as on Drive) with A's cached on its original folder
// and B's orphaned — the state delete leaves behind when it deletes folder B
// itself, taking the row that pointed at B's replica with it.
func replicaTree(t *testing.T, db *sql.DB) (archID int64) {
	t.Helper()
	_, archID = buildArchiveTree(t, db)
	replicaA, err := upsertReplicaRow(db, "archA", "ARCH A", sql.NullInt64{Int64: archID, Valid: true}, "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertReplicaRow(db, "archB", "ARCH B", sql.NullInt64{Int64: replicaA, Valid: true}, "me@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := setArchiveFolder(db, "A", "archA"); err != nil {
		t.Fatal(err)
	}
	if err := deleteNodeRow(db, "B"); err != nil {
		t.Fatal(err)
	}
	return archID
}

func TestOrphanedReplicaFolders(t *testing.T) {
	db := testDB(t)
	replicaTree(t, db)

	orphans, err := orphanedReplicaFolders(db, "arch")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range orphans {
		got = append(got, o.driveID)
	}
	// Only archB: archA is still pointed at by folder A, and the archive root
	// itself has no parent row.
	if strings.Join(got, ",") != "archB" {
		t.Fatalf("orphanedReplicaFolders = %v, want [archB]", got)
	}

	// An archived folder that merely looks like a replica keeps its contents:
	// it records where it came from, so it is a real item, not a shell.
	if err := markArchived(db, "C", "root", mustRowID(t, db, "archA")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE nodes SET name = 'ARCH C' WHERE drive_id = 'C'`); err != nil {
		t.Fatal(err)
	}
	if orphans, err = orphanedReplicaFolders(db, "arch"); err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].driveID != "archB" {
		t.Fatalf("orphanedReplicaFolders with an ARCH-named archived folder = %v, want just archB", orphans)
	}

	// So does a shell someone marked keep in the review UI.
	setDecision(t, db, "archB", decisionKeep)
	if orphans, err = orphanedReplicaFolders(db, "arch"); err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Errorf("orphanedReplicaFolders after marking archB keep = %v, want none", orphans)
	}
}

func TestReplicasToPruneDeepestFirst(t *testing.T) {
	db := testDB(t)
	replicaTree(t, db)

	prunes, err := replicasToPrune(db, "arch")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, p := range prunes {
		order = append(order, p.replicaID+":"+p.originalID)
	}
	// archB (depth 2, orphaned so no original to un-cache) must come before
	// archA (depth 1) that nests it, or the parent could never read as empty.
	if strings.Join(order, ",") != "archB:,archA:A" {
		t.Errorf("replicasToPrune = %v, want archB before archA", order)
	}
}

func mustRowID(t *testing.T, db *sql.DB, driveID string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM nodes WHERE drive_id = ?`, driveID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// The owner Drive reports now, not the one the snapshot recorded, decides what
// happens to an item delete picked up as externally owned — a shortcut
// reclaim-folders created inside somebody else's folder is ours, and Drive
// refuses to strip the only parent off an item we own (400 badRequest), so it
// has to be deleted rather than removed.
func TestLiveOwnerClassOverridesTheSnapshot(t *testing.T) {
	me := &drive.User{EmailAddress: "me@example.com", PermissionId: "perm-me"}
	domains := []string{"example.com"}
	cases := []struct {
		name string
		f    *drive.File
		want ownerClass
	}{
		{"our email", &drive.File{Owners: []*drive.User{{EmailAddress: "ME@example.com"}}}, ownerMine},
		{"our permission id", &drive.File{Owners: []*drive.User{{PermissionId: "perm-me"}}}, ownerMine},
		{"colleague in the org", &drive.File{Owners: []*drive.User{{EmailAddress: "colleague@example.com"}}}, ownerInternal},
		{"genuinely external", &drive.File{Owners: []*drive.User{{EmailAddress: "stranger@other.com"}}}, ownerExternal},
		{"shared-drive item, no owner", &drive.File{}, ownerExternal},
	}
	for _, c := range cases {
		if got := liveOwnerClass(c.f, me, domains); got != c.want {
			t.Errorf("liveOwnerClass(%s) = %d, want %d", c.name, got, c.want)
		}
	}
}

// An account with no permission id must not match an owner that has none
// either — every item Drive reports without one would otherwise read as ours.
func TestLiveOwnerClassIgnoresEmptyPermissionIDs(t *testing.T) {
	me := &drive.User{EmailAddress: "me@example.com"}
	f := &drive.File{Owners: []*drive.User{{EmailAddress: "stranger@other.com"}}}
	if got := liveOwnerClass(f, me, []string{"example.com"}); got != ownerExternal {
		t.Errorf("liveOwnerClass with empty permission ids = %d, want %d", got, ownerExternal)
	}
}

// A folder owned by another internal account cannot take the dropoff route its
// files take: Drive moves files into a shared drive and folders never, so the
// move that flips a file's ownership to the org answers 403 for a folder. It is
// gathered under the archive root for an admin to take over instead.
func TestRouteForInternalFoldersAreCollected(t *testing.T) {
	cases := []struct {
		class ownerClass
		typ   string
		want  deleteRoute
	}{
		{ownerMine, typeFolder, routeDelete},
		{ownerMine, "file", routeDelete},
		{ownerInternal, "file", routeDropoff},
		{ownerInternal, typeShortcut, routeDropoff},
		{ownerInternal, typeFolder, routeCollect},
		{ownerExternal, typeFolder, routeExternal},
		{ownerExternal, "file", routeExternal},
	}
	for _, c := range cases {
		if got := routeFor(c.class, c.typ); got != c.want {
			t.Errorf("routeFor(class %d, %q) = %d, want %d", c.class, c.typ, got, c.want)
		}
	}
}

// The prefix has to survive a rename that landed in a run whose move then
// failed: the next run renames from the folder's live name, so prefixing twice
// would leave "(deleteme) (deleteme) Plans" behind.
func TestDeleteMeNameIsIdempotent(t *testing.T) {
	if got := deleteMeName("Plans"); got != "(deleteme) Plans" {
		t.Errorf("deleteMeName(%q) = %q", "Plans", got)
	}
	if got := deleteMeName("(deleteme) Plans"); got != "(deleteme) Plans" {
		t.Errorf("deleteMeName on an already-prefixed name = %q, want it unchanged", got)
	}
}

// An item in a shared drive has no owner at all, and the log lines naming one
// must not index into an empty list.
func TestLiveOwnerNameWithoutAnOwner(t *testing.T) {
	if got := liveOwnerName(&drive.File{Owners: []*drive.User{{EmailAddress: "them@example.com"}}}); got != "them@example.com" {
		t.Errorf("liveOwnerName = %q, want them@example.com", got)
	}
	for _, f := range []*drive.File{{}, {Owners: []*drive.User{{}}}} {
		if got := liveOwnerName(f); got == "" || strings.Contains(got, "@") {
			t.Errorf("liveOwnerName(%+v) = %q, want a placeholder", f, got)
		}
	}
}
