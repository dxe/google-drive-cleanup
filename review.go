package main

// The review web UI: a local, auth-free HTTP server for triaging the crawled
// tree into keep/delete decisions (the `decision` column on nodes). The
// invariant maintained here: a folder marked 'delete' has every descendant
// marked 'delete' — nothing kept may live inside a deleted subtree. Marking a
// node keep (or clearing it) inside a delete subtree therefore un-marks its
// delete ancestors, which are then re-decided by the rollup pass.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"
)

const (
	decisionNone   = ""
	decisionKeep   = "keep"
	decisionDelete = "delete"
)

var reviewCmd = &cobra.Command{
	Use:   "review",
	Short: "Serve a local web UI to mark files and folders as keep or delete",
	Long: `Serve a local web app (no auth — dev use only) showing the crawled tree:
folders on the left, the selected folder's files on the right. Mark nodes as
Keep or Delete; folder marks propagate to descendants, fully-decided folders
are auto-decided from their children, and the last actions can be undone while
the server is running. Decisions are stored in the nodes.decision column and
exported for teammates with 'export-review'.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		listen, _ := cmd.Flags().GetString("listen")
		return runReview(dbPath, cfgPath, listen)
	},
}

func init() {
	reviewCmd.Flags().String("listen", "127.0.0.1:8844", "address to serve the review UI on")
}

func runReview(dbPath, cfgPath, listen string) error {
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// The archive tree holds already-soft-deleted items; it is kept out of the
	// review UI entirely so its decisions (still 'delete', consumed by the
	// delete command) cannot be disturbed.
	archiveRootID, err := optionalArchiveRootID(cfgPath)
	if err != nil {
		return err
	}

	s := &reviewServer{db: db, archiveRootID: archiveRootID}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// The UI is the Next.js app in web/ (it proxies /api/* here); this
		// endpoint just points a stray visitor at it.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "drive-cleanup review API. The UI is the Next.js app: cd web && npm run dev, then open http://localhost:3000")
	})
	mux.HandleFunc("/api/tree", s.handleTree)
	mux.HandleFunc("/api/files", s.handleFiles)
	mux.HandleFunc("/api/mark", s.handleMark)
	mux.HandleFunc("/api/mark-many", s.handleMarkMany)
	mux.HandleFunc("/api/undo", s.handleUndo)

	log.Printf("review UI listening on http://%s", listen)
	return http.ListenAndServe(listen, mux)
}

// reviewServer holds the open database and the in-memory undo stack. The undo
// stack deliberately lives only in process memory — restarting the server
// forgets undo history but keeps all decisions (they are in the DB).
type reviewServer struct {
	db *sql.DB
	// archiveRootID, when non-empty, is the archive tree's root Drive ID; that
	// subtree is hidden from the tree endpoint and rejected by the mark
	// endpoints (see runReview).
	archiveRootID string

	mu   sync.Mutex // serializes mark/undo and guards the undo stack
	undo []reviewUndoEntry
}

// reviewUndoEntry records how to reverse one user action: every node the
// action touched mapped to the decision it had before.
type reviewUndoEntry struct {
	label string
	prev  map[int64]string
}

// maxUndoEntries bounds the in-memory undo stack.
const maxUndoEntries = 200

// decisionCounts tallies nodes by decision.
type decisionCounts struct {
	Keep      int `json:"keep"`
	Delete    int `json:"delete"`
	Undecided int `json:"undecided"`
}

func (c *decisionCounts) add(dec string, n int) {
	switch dec {
	case decisionKeep:
		c.Keep += n
	case decisionDelete:
		c.Delete += n
	default:
		c.Undecided += n
	}
}

// reviewNode is one node of the in-memory decision forest, shared by the tree
// endpoint and the export-review renderer.
type reviewNode struct {
	rowID    int64
	driveID  string
	name     string
	typ      string
	decision string
	parentID sql.NullInt64
	// lastModified is the estimated last-content-change time (RFC3339), populated
	// by `crawl`; NULL/invalid otherwise.
	lastModified sql.NullString
	// originalOwner* record the last owner before the node was handed to an
	// ownership-transfer account (see the original_owner_* columns); NULL on a
	// snapshot crawled before those columns existed.
	originalOwnerName  sql.NullString
	originalOwnerEmail sql.NullString
	children           []*reviewNode
	// subtree tallies decisions over the node itself plus every descendant.
	subtree decisionCounts
	// directFiles tallies decisions over the node's direct non-folder children.
	directFiles decisionCounts
}

// loadReviewForest loads the whole nodes table into a sorted forest (folders
// first, then case-insensitive by name, like a file browser) and computes the
// per-node decision tallies. excludeDriveID, when non-empty, drops that node
// and its whole subtree from the forest — used to hide the archive tree from
// decision marking and the exported reports.
func loadReviewForest(db *sql.DB, excludeDriveID string) ([]*reviewNode, error) {
	rows, err := db.Query(`SELECT id, drive_id, name, type, parent_id, decision, last_modified,
		original_owner_display_name, original_owner_email FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	all := make(map[int64]*reviewNode)
	for rows.Next() {
		var n reviewNode
		if err := rows.Scan(&n.rowID, &n.driveID, &n.name, &n.typ, &n.parentID, &n.decision, &n.lastModified,
			&n.originalOwnerName, &n.originalOwnerEmail); err != nil {
			return nil, err
		}
		all[n.rowID] = &n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var roots []*reviewNode
	for _, n := range all {
		if n.driveID == excludeDriveID && excludeDriveID != "" {
			// The excluded node is linked neither as a root nor as a child, so
			// its descendants (which attach to it below) dangle out of the forest.
			continue
		}
		if n.parentID.Valid {
			if p := all[n.parentID.Int64]; p != nil {
				p.children = append(p.children, n)
				continue
			}
		}
		roots = append(roots, n)
	}
	sortReviewNodes(roots)
	for _, r := range roots {
		sortReviewChildren(r)
		computeReviewCounts(r)
	}
	return roots, nil
}

func sortReviewNodes(nodes []*reviewNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if (a.typ == typeFolder) != (b.typ == typeFolder) {
			return a.typ == typeFolder
		}
		return strings.ToLower(a.name) < strings.ToLower(b.name)
	})
}

func sortReviewChildren(n *reviewNode) {
	sortReviewNodes(n.children)
	for _, c := range n.children {
		sortReviewChildren(c)
	}
}

// computeReviewCounts fills subtree and directFiles tallies bottom-up.
func computeReviewCounts(n *reviewNode) decisionCounts {
	var c decisionCounts
	c.add(n.decision, 1)
	for _, ch := range n.children {
		sub := computeReviewCounts(ch)
		c.Keep += sub.Keep
		c.Delete += sub.Delete
		c.Undecided += sub.Undecided
		if ch.typ != typeFolder {
			n.directFiles.add(ch.decision, 1)
		}
	}
	n.subtree = c
	return c
}

// reviewStatus maps a folder's subtree tally (self included) to the display
// status used for coloring: delete=red, keep=green, mixed=yellow, the partial
// variants are pale (some marks, rest undecided), todo=no marks at all.
func reviewStatus(c decisionCounts) string {
	switch {
	case c.Keep > 0 && c.Delete > 0:
		return "mixed"
	case c.Delete > 0 && c.Undecided == 0:
		return "delete"
	case c.Keep > 0 && c.Undecided == 0:
		return "keep"
	case c.Delete > 0:
		return "partial-delete"
	case c.Keep > 0:
		return "partial-keep"
	default:
		return "todo"
	}
}

// --- HTTP handlers ---

type treeFolderJSON struct {
	DriveID  string `json:"driveId"`
	Name     string `json:"name"`
	Decision string `json:"decision"`
	// Owner/OwnerEmail are the node's original owner (before any
	// ownership-transfer hand-off); empty when the snapshot has no record.
	Owner      string            `json:"owner"`
	OwnerEmail string            `json:"ownerEmail"`
	Files      decisionCounts    `json:"files"`
	Subtree    decisionCounts    `json:"subtree"`
	Folders    []*treeFolderJSON `json:"folders"`
}

type treeResponseJSON struct {
	Roots        []*treeFolderJSON `json:"roots"`
	UndoLabel    string            `json:"undoLabel"`
	FileTotals   decisionCounts    `json:"fileTotals"`
	FolderTotals decisionCounts    `json:"folderTotals"`
}

func (s *reviewServer) handleTree(w http.ResponseWriter, r *http.Request) {
	roots, err := loadReviewForest(s.db, s.archiveRootID)
	if err != nil {
		httpError(w, err)
		return
	}
	resp := treeResponseJSON{Roots: []*treeFolderJSON{}}
	var walk func(n *reviewNode) *treeFolderJSON
	walk = func(n *reviewNode) *treeFolderJSON {
		f := &treeFolderJSON{
			DriveID:    n.driveID,
			Name:       n.name,
			Decision:   n.decision,
			Owner:      n.originalOwnerName.String,
			OwnerEmail: n.originalOwnerEmail.String,
			Files:      n.directFiles,
			Subtree:    n.subtree,
			Folders:    []*treeFolderJSON{},
		}
		for _, c := range n.children {
			if c.typ == typeFolder {
				f.Folders = append(f.Folders, walk(c))
			}
		}
		return f
	}
	var tally func(n *reviewNode)
	tally = func(n *reviewNode) {
		if n.typ == typeFolder {
			resp.FolderTotals.add(n.decision, 1)
		} else {
			resp.FileTotals.add(n.decision, 1)
		}
		for _, c := range n.children {
			tally(c)
		}
	}
	for _, root := range roots {
		tally(root)
		if root.typ == typeFolder {
			resp.Roots = append(resp.Roots, walk(root))
		}
	}
	s.mu.Lock()
	if len(s.undo) > 0 {
		resp.UndoLabel = s.undo[len(s.undo)-1].label
	}
	s.mu.Unlock()
	writeJSON(w, resp)
}

type fileItemJSON struct {
	DriveID  string `json:"driveId"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Decision string `json:"decision"`
	// Owner/OwnerEmail are the file's original owner (before any
	// ownership-transfer hand-off); empty when the snapshot has no record.
	Owner      string `json:"owner"`
	OwnerEmail string `json:"ownerEmail"`
	// LastModified is the recorded last-content-change time (RFC3339), empty
	// when no crawl has recorded one.
	LastModified string `json:"lastModified"`
}

func (s *reviewServer) handleFiles(w http.ResponseWriter, r *http.Request) {
	folder := r.URL.Query().Get("folder")
	if folder == "" {
		http.Error(w, "missing folder parameter", http.StatusBadRequest)
		return
	}
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT n.drive_id, n.name, n.type, n.decision,
		       n.original_owner_display_name, n.original_owner_email, n.last_modified
		FROM nodes n JOIN nodes p ON n.parent_id = p.id
		WHERE p.drive_id = ? AND n.type <> '%s'
		ORDER BY n.name COLLATE NOCASE, n.id`, typeFolder), folder)
	if err != nil {
		httpError(w, err)
		return
	}
	defer rows.Close()
	files := []fileItemJSON{}
	for rows.Next() {
		var (
			f                          fileItemJSON
			owner, ownerEmail, modTime sql.NullString
		)
		if err := rows.Scan(&f.DriveID, &f.Name, &f.Type, &f.Decision, &owner, &ownerEmail, &modTime); err != nil {
			httpError(w, err)
			return
		}
		f.Owner, f.OwnerEmail, f.LastModified = owner.String, ownerEmail.String, modTime.String
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, files)
}

// markResult is the outcome of a mark request. When the requested mark
// conflicts with existing descendant decisions and no onConflict resolution
// was supplied, NeedsConfirm is true, the conflict counts are filled in, and
// nothing was written.
type markResult struct {
	NeedsConfirm     bool `json:"needsConfirm"`
	ConflictKeeps    int  `json:"conflictKeeps"`
	ConflictDeletes  int  `json:"conflictDeletes"`
	Changed          int  `json:"changed"`
	ClearedAncestors int  `json:"clearedAncestors"`
}

type markRequest struct {
	DriveID    string `json:"driveId"`
	Decision   string `json:"decision"`
	OnConflict string `json:"onConflict"` // "", "overwrite", "preserve"
}

func (s *reviewServer) handleMark(w http.ResponseWriter, r *http.Request) {
	var req markRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validDecision(req.Decision) {
		http.Error(w, "invalid decision", http.StatusBadRequest)
		return
	}
	if inArchive, err := s.inArchiveTree(req.DriveID); err != nil {
		httpError(w, err)
		return
	} else if inArchive {
		http.Error(w, "node is inside the archive tree; archived decisions cannot be changed here (use restore)", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.applyMark(req)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, res)
}

func validDecision(d string) bool {
	return d == decisionNone || d == decisionKeep || d == decisionDelete
}

// inArchiveTree reports whether driveID sits inside the configured archive
// tree. The UI never shows those nodes (handleTree excludes them), so this is
// defense-in-depth: an archived item's 'delete' decision is what the delete
// command consumes, and a stray mark would silently exempt it.
func (s *reviewServer) inArchiveTree(driveID string) (bool, error) {
	if s.archiveRootID == "" {
		return false, nil
	}
	return nodeInSubtree(s.db, s.archiveRootID, driveID)
}

// applyMark runs one mark action in a transaction and, if it wrote anything,
// pushes an undo entry. Caller must hold s.mu.
func (s *reviewServer) applyMark(req markRequest) (markResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return markResult{}, err
	}
	defer tx.Rollback()

	var name string
	if err := tx.QueryRow(`SELECT name FROM nodes WHERE drive_id = ?`, req.DriveID).Scan(&name); err != nil {
		if err == sql.ErrNoRows {
			return markResult{}, fmt.Errorf("no node with drive id %s", req.DriveID)
		}
		return markResult{}, err
	}

	rec := make(map[int64]string)
	res, err := markInTx(tx, req.DriveID, req.Decision, req.OnConflict, rec)
	if err != nil {
		return markResult{}, err
	}
	if res.NeedsConfirm {
		return res, nil // rolled back by the deferred Rollback
	}
	if err := tx.Commit(); err != nil {
		return markResult{}, err
	}
	res.Changed = len(rec)
	if len(rec) > 0 {
		s.pushUndo(reviewUndoEntry{label: fmt.Sprintf("%s %q", decisionVerb(req.Decision), name), prev: rec})
	}
	return res, nil
}

func decisionVerb(d string) string {
	switch d {
	case decisionKeep:
		return "Keep"
	case decisionDelete:
		return "Delete"
	default:
		return "Clear"
	}
}

// nodeBrief pairs a node's row id with its (pre-action) decision.
type nodeBrief struct {
	id       int64
	decision string
}

// markInTx performs the propagation logic for marking one node. rec
// accumulates each touched node's original decision for undo (first write
// wins, so a node updated twice in one action still records its true
// original).
func markInTx(tx *sql.Tx, driveID, decision, onConflict string, rec map[int64]string) (markResult, error) {
	var (
		rowID int64
		typ   string
		cur   string
	)
	if err := tx.QueryRow(`SELECT id, type, decision FROM nodes WHERE drive_id = ?`, driveID).
		Scan(&rowID, &typ, &cur); err != nil {
		return markResult{}, err
	}

	sub := []nodeBrief{{rowID, cur}}
	if typ == typeFolder {
		var err error
		sub, err = subtreeDecisions(tx, rowID)
		if err != nil {
			return markResult{}, err
		}
	}
	var descKeeps, descDeletes int
	for _, n := range sub {
		if n.id == rowID {
			continue
		}
		switch n.decision {
		case decisionKeep:
			descKeeps++
		case decisionDelete:
			descDeletes++
		}
	}

	var res markResult
	switch decision {
	case decisionDelete:
		// Everything under a deleted folder must be deleted too; overwriting
		// keeps requires explicit confirmation.
		if descKeeps > 0 && onConflict != "overwrite" {
			return markResult{NeedsConfirm: true, ConflictKeeps: descKeeps}, nil
		}
		if err := applyDecision(tx, sub, decisionDelete, rec); err != nil {
			return markResult{}, err
		}
	case decisionKeep:
		if typ == typeFolder && descDeletes > 0 && onConflict == "" {
			return markResult{NeedsConfirm: true, ConflictDeletes: descDeletes}, nil
		}
		if onConflict == "overwrite" {
			if err := applyDecision(tx, sub, decisionKeep, rec); err != nil {
				return markResult{}, err
			}
		} else {
			// Default / "preserve": keep the node and its undecided
			// descendants; descendant delete subtrees stay delete.
			targets := make([]nodeBrief, 0, len(sub))
			for _, n := range sub {
				if n.id == rowID || n.decision == decisionNone {
					targets = append(targets, n)
				}
			}
			if err := applyDecision(tx, targets, decisionKeep, rec); err != nil {
				return markResult{}, err
			}
		}
	case decisionNone:
		if err := applyDecision(tx, sub, decisionNone, rec); err != nil {
			return markResult{}, err
		}
	}

	// A keep or cleared node may not live inside a delete subtree: un-mark any
	// delete ancestors, then let the rollup re-decide them from their children.
	if decision != decisionDelete {
		cleared, err := clearDeleteAncestors(tx, rowID, rec)
		if err != nil {
			return markResult{}, err
		}
		res.ClearedAncestors = cleared
	}
	if err := rollupAncestors(tx, rowID, rec); err != nil {
		return markResult{}, err
	}
	return res, nil
}

// subtreeDecisions returns (id, decision) for every node in the subtree rooted
// at rowID, inclusive.
func subtreeDecisions(tx *sql.Tx, rowID int64) ([]nodeBrief, error) {
	rows, err := tx.Query(`
		WITH RECURSIVE subtree(id) AS (
			SELECT id FROM nodes WHERE id = ?
			UNION ALL
			SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		SELECT id, decision FROM nodes WHERE id IN (SELECT id FROM subtree)`, rowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []nodeBrief
	for rows.Next() {
		var n nodeBrief
		if err := rows.Scan(&n.id, &n.decision); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ancestorChain returns the node's ancestors from parent up to the root,
// with their current in-transaction decisions. Cycle-guarded.
func ancestorChain(tx *sql.Tx, rowID int64) ([]nodeBrief, error) {
	var chain []nodeBrief
	seen := map[int64]bool{rowID: true}
	cur := rowID
	for {
		var parent sql.NullInt64
		if err := tx.QueryRow(`SELECT parent_id FROM nodes WHERE id = ?`, cur).Scan(&parent); err != nil {
			return nil, err
		}
		if !parent.Valid || seen[parent.Int64] {
			return chain, nil
		}
		var n nodeBrief
		n.id = parent.Int64
		if err := tx.QueryRow(`SELECT decision FROM nodes WHERE id = ?`, n.id).Scan(&n.decision); err != nil {
			return nil, err
		}
		chain = append(chain, n)
		seen[n.id] = true
		cur = n.id
	}
}

// clearDeleteAncestors resets every 'delete' ancestor of rowID to undecided,
// preserving the no-kept-item-inside-a-deleted-subtree invariant. Returns how
// many ancestors were cleared.
func clearDeleteAncestors(tx *sql.Tx, rowID int64, rec map[int64]string) (int, error) {
	anc, err := ancestorChain(tx, rowID)
	if err != nil {
		return 0, err
	}
	var del []nodeBrief
	for _, a := range anc {
		if a.decision == decisionDelete {
			del = append(del, a)
		}
	}
	if err := applyDecision(tx, del, decisionNone, rec); err != nil {
		return 0, err
	}
	return len(del), nil
}

// rollupAncestors walks rowID's ancestor chain upward and auto-decides every
// undecided folder whose children are now all decided: delete when every
// child is delete (an all-delete subtree), keep otherwise. An explicitly
// decided ancestor is never overridden. Stops at the first undecided ancestor
// that still has undecided children — nothing above it can be complete either.
func rollupAncestors(tx *sql.Tx, rowID int64, rec map[int64]string) error {
	anc, err := ancestorChain(tx, rowID)
	if err != nil {
		return err
	}
	for _, a := range anc {
		if a.decision != decisionNone {
			continue
		}
		decided, err := rollupFolder(tx, a, rec)
		if err != nil {
			return err
		}
		if !decided {
			break
		}
	}
	return nil
}

// rollupFolder auto-decides one undecided folder if all of its children are
// decided: delete when every child is delete, keep otherwise. Reports whether
// the folder ended up decided.
func rollupFolder(tx *sql.Tx, folder nodeBrief, rec map[int64]string) (bool, error) {
	rows, err := tx.Query(`SELECT decision FROM nodes WHERE parent_id = ?`, folder.id)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var nChildren int
	allDecided, allDelete := true, true
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return false, err
		}
		nChildren++
		if d == decisionNone {
			allDecided = false
		}
		if d != decisionDelete {
			allDelete = false
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if nChildren == 0 || !allDecided {
		return false, nil
	}
	newDec := decisionKeep
	if allDelete {
		newDec = decisionDelete
	}
	if err := applyDecision(tx, []nodeBrief{folder}, newDec, rec); err != nil {
		return false, err
	}
	return true, nil
}

// applyDecision sets decision on every listed node whose current decision
// differs, recording each node's first-seen previous value in rec for undo.
func applyDecision(tx *sql.Tx, nodes []nodeBrief, dec string, rec map[int64]string) error {
	var ids []int64
	for _, n := range nodes {
		if n.decision == dec {
			continue
		}
		if _, ok := rec[n.id]; !ok {
			rec[n.id] = n.decision
		}
		ids = append(ids, n.id)
	}
	return updateDecisions(tx, ids, dec)
}

// updateDecisions runs the batched UPDATE, chunked to stay under SQLite's
// bound-parameter limit.
func updateDecisions(tx *sql.Tx, ids []int64, dec string) error {
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]
		placeholders := make([]byte, 0, 2*len(part))
		args := make([]any, 0, len(part)+1)
		args = append(args, dec)
		for i, id := range part {
			if i > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args = append(args, id)
		}
		if _, err := tx.Exec(
			`UPDATE nodes SET decision = ? WHERE id IN (`+string(placeholders)+`)`, args...); err != nil {
			return err
		}
	}
	return nil
}

type markManyRequest struct {
	DriveIDs []string `json:"driveIds"`
	Decision string   `json:"decision"`
}

// handleMarkMany bulk-marks files (non-folders) — the file pane's "all files"
// buttons. Folders in the list are rejected; use /api/mark for propagation.
func (s *reviewServer) handleMarkMany(w http.ResponseWriter, r *http.Request) {
	var req markManyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validDecision(req.Decision) {
		http.Error(w, "invalid decision", http.StatusBadRequest)
		return
	}
	if len(req.DriveIDs) == 0 {
		writeJSON(w, markResult{})
		return
	}
	for _, id := range req.DriveIDs {
		if inArchive, err := s.inArchiveTree(id); err != nil {
			httpError(w, err)
			return
		} else if inArchive {
			http.Error(w, fmt.Sprintf("node %s is inside the archive tree; archived decisions cannot be changed here (use restore)", id), http.StatusBadRequest)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.applyMarkMany(req)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, res)
}

// applyMarkMany bulk-marks the listed files in one transaction and undo
// entry. Caller must hold s.mu.
func (s *reviewServer) applyMarkMany(req markManyRequest) (markResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return markResult{}, err
	}
	defer tx.Rollback()

	rec := make(map[int64]string)
	res, matched, err := markManyInTx(tx, req.DriveIDs, req.Decision, rec)
	if err != nil {
		return markResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return markResult{}, err
	}
	res.Changed = len(rec)
	if len(rec) > 0 {
		s.pushUndo(reviewUndoEntry{
			label: fmt.Sprintf("%s %d files", decisionVerb(req.Decision), matched),
			prev:  rec,
		})
	}
	return res, nil
}

// markManyInTx bulk-marks the given files (non-folders) with decision inside tx,
// maintaining the no-kept-item-inside-a-delete-subtree invariant and rolling up
// their ancestor folders — the same logic the review UI's "all files" buttons
// use. Touched nodes' previous decisions accumulate in rec for undo. Missing
// drive IDs are skipped; a folder in the list is an error. It does not commit:
// the caller owns the transaction. Returns the result and the number of files
// matched (existing, non-folder).
func markManyInTx(tx *sql.Tx, driveIDs []string, decision string, rec map[int64]string) (markResult, int, error) {
	var targets []nodeBrief
	parentSet := make(map[int64]bool)
	for _, driveID := range driveIDs {
		var (
			n      nodeBrief
			typ    string
			parent sql.NullInt64
		)
		err := tx.QueryRow(`SELECT id, type, decision, parent_id FROM nodes WHERE drive_id = ?`, driveID).
			Scan(&n.id, &typ, &n.decision, &parent)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return markResult{}, 0, err
		}
		if typ == typeFolder {
			return markResult{}, 0, fmt.Errorf("mark-many accepts files only; mark folders individually")
		}
		targets = append(targets, n)
		if parent.Valid {
			parentSet[parent.Int64] = true
		}
	}
	if err := applyDecision(tx, targets, decision, rec); err != nil {
		return markResult{}, 0, err
	}
	res := markResult{}
	// Invariant fix + rollup share each file's ancestor chain, so run them
	// once per distinct parent folder, not once per file. A kept/cleared file
	// may not sit inside a delete subtree: un-mark the parent itself and any
	// delete ancestors above it, then let the rollup re-decide the chain.
	if decision != decisionDelete {
		for parent := range parentSet {
			var pd string
			if err := tx.QueryRow(`SELECT decision FROM nodes WHERE id = ?`, parent).Scan(&pd); err != nil {
				return markResult{}, 0, err
			}
			if pd == decisionDelete {
				if err := applyDecision(tx, []nodeBrief{{parent, pd}}, decisionNone, rec); err != nil {
					return markResult{}, 0, err
				}
				res.ClearedAncestors++
			}
			cleared, err := clearDeleteAncestors(tx, parent, rec)
			if err != nil {
				return markResult{}, 0, err
			}
			res.ClearedAncestors += cleared
		}
	}
	for parent := range parentSet {
		if err := rollupSelfAndAncestors(tx, parent, rec); err != nil {
			return markResult{}, 0, err
		}
	}
	return res, len(targets), nil
}

// rollupSelfAndAncestors auto-decides the given folder (if undecided and all
// its children are decided) and then continues up the ancestor chain.
func rollupSelfAndAncestors(tx *sql.Tx, folderID int64, rec map[int64]string) error {
	var dec string
	if err := tx.QueryRow(`SELECT decision FROM nodes WHERE id = ?`, folderID).Scan(&dec); err != nil {
		return err
	}
	if dec == decisionNone {
		if _, err := rollupFolder(tx, nodeBrief{folderID, dec}, rec); err != nil {
			return err
		}
	}
	return rollupAncestors(tx, folderID, rec)
}

func (s *reviewServer) pushUndo(e reviewUndoEntry) {
	s.undo = append(s.undo, e)
	if len(s.undo) > maxUndoEntries {
		s.undo = s.undo[len(s.undo)-maxUndoEntries:]
	}
}

type undoResponseJSON struct {
	Undone  string `json:"undone"`
	Changed int    `json:"changed"`
}

func (s *reviewServer) handleUndo(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.undo) == 0 {
		http.Error(w, "nothing to undo", http.StatusConflict)
		return
	}
	res, err := s.applyUndo()
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, res)
}

// applyUndo reverts the most recent undo entry, restoring every touched
// node's previous decision. Caller must hold s.mu and ensure the stack is
// non-empty.
func (s *reviewServer) applyUndo() (undoResponseJSON, error) {
	entry := s.undo[len(s.undo)-1]

	tx, err := s.db.Begin()
	if err != nil {
		return undoResponseJSON{}, err
	}
	defer tx.Rollback()
	// Group restored values so each distinct previous decision is one batch.
	byPrev := make(map[string][]int64)
	for id, prev := range entry.prev {
		byPrev[prev] = append(byPrev[prev], id)
	}
	for prev, ids := range byPrev {
		if err := updateDecisions(tx, ids, prev); err != nil {
			return undoResponseJSON{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return undoResponseJSON{}, err
	}
	s.undo = s.undo[:len(s.undo)-1]
	return undoResponseJSON{Undone: entry.label, Changed: len(entry.prev)}, nil
}

// --- small HTTP helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encoding response: %v", err)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func httpError(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
