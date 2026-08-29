package main

// The evict-externals command prepares one folder of the crawled tree to be
// moved into a shared drive.
//
// A shared drive can only hold items the org owns. Drive does let you drag a
// folder in that contains items you do not own — up to 25 of them — but what it
// does with those items is take them out of the folder entirely and drop them in
// their own owners' My Drives, where we can no longer see them, let alone move
// them. Past 25 it refuses the move outright. Either way the outcome is bad: the
// files are lost to us, or the folder cannot go.
//
// So we move them ourselves, first, and into somewhere we keep access to. Every
// externally-owned item comes out of the subtree and into a parallel "externals"
// tree we own (config externals.root), where it stays visible, stays shared with
// the same people, and stays available to the ordinary pack/unpack migration
// later on if its owner ever hands it over. The folder can then go to the shared
// drive with nothing left in it that Drive would scatter:
//
//	unowned file    -> moved into the externals tree (config externals.root),
//	                   leaving a shortcut to it behind in its original folder
//	unowned folder  -> moved into the externals tree as it stands, once it is
//	                   an empty "leaf" (see below); no shortcut is created,
//	                   because reclaim-folders already left one behind — unless
//	                   it is leaving with its contents (see
//	                   --allow-unowned-folders), when it gets one like a file
//
// "Unowned" means what it means everywhere else in this tool: an owner that is
// neither the running account nor on one of the configured internal-domains.
// There is no 25-item ceiling on this — it is our own moves, one file at a time.
//
// Two things must be settled before any of that is safe, so both are refused up
// front rather than worked around:
//
//   - Anything still marked delete has to be archived first. Those items are
//     unwanted, may well be unowned, and archiving them is both the cheaper and
//     the correct way to get them out of the subtree.
//   - An unowned folder still holding content has to go through reclaim-folders
//     first. Moving such a folder out would drag its contents — quite possibly
//     owned ones — along with it. After reclaim-folders each of their folders is
//     empty except for the "(new) <name>" shortcut pointing at the replacement,
//     and a folder like that carries nothing but itself. Every such folder is
//     listed, and --allow-unowned-folders overrides the refusal: the folders
//     then move out much like empty ones, contents and all, each leaving a
//     shortcut behind in its old place.
//
// The externals tree mirrors the crawl root's folder structure, so an evicted
// file keeps its original location and name, and each replica folder gets the
// original's sharing copied onto it, so everybody who could reach the file
// before still can. Only the ancestor folders that actually receive something
// are created; the tree never fills up with empty placeholders. Each replica is
// named "(ext) <original>", so it is never taken for the canonical folder it
// mirrors, and holds a "((new)) <original>" shortcut pointing at that folder, so
// the way back is one click from wherever an evicted file landed. An original
// folder that actually gives something up gets the matching link the other way —
// an "(external files) <original>" shortcut pointing at its replica — so the
// evicted files are visible from where they used to be; a folder that is only an
// ancestor on the way to one gets none, its replica holding nothing but other
// replicas. Neither replica name is what a replica is found by on a later run:
// the cached Drive ID is.
//
// The snapshot is updated as the moves happen — replica folders and the
// shortcuts left behind become nodes rows, moved items are reparented under
// their replica and stamped with evicted_from_drive_id — so later runs and a
// re-crawl see where things really are.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
)

var evictExternalsCmd = &cobra.Command{
	Use:   "evict-externals <folder_id>",
	Short: "Move externally-owned items out of a folder so it can go to a shared drive",
	Long: `Move every externally-owned file and emptied externally-owned folder out of
<folder_id> (a crawled folder under the crawl root) and into the externals
folder (config externals.root), so what is left in the subtree is owned by the
org and the whole folder can be moved into a shared drive.

Why bother, rather than just dragging the folder in: a shared drive cannot hold
items the org does not own. Drive tolerates up to 25 such items in a folder
being moved in, and "tolerates" means it pulls them out of the folder and drops
them into their own owners' My Drives — out of our sight and out of our reach.
More than 25 and it refuses the move altogether. Evicting them first avoids
both: they land in a folder WE own, still shared with the same people, still
visible, and still available to the normal pack/unpack migration later if their
owner ever hands them over. There is no 25-item limit on this — these are our
own moves, one file at a time.

"Externally owned" means the same here as everywhere else in this tool: an
owner that is neither the account running the command nor on one of the
configured internal-domains.

The run refuses to start, changing nothing, if either of these is outstanding:

  * anything in the subtree is still marked delete — run archive first, so
    those unwanted (and possibly unowned) items are out of the way instead of
    being evicted as if they were worth keeping;
  * an externally-owned folder in the subtree still holds content — run
    reclaim-folders --email <owner> first, or pass --allow-unowned-folders. Moving such
    a folder out would take its contents with it. A folder that is empty, or
    that holds nothing but a single shortcut (the "(new) <name>" link
    reclaim-folders leaves pointing at the replacement), carries nothing but
    itself and is fine.

Every externally-owned folder still holding content is listed before either of
those happens, so it is clear what is at stake. With --allow-unowned-folders
those folders are treated as leaf folders like any other: each moves into the
externals tree as it stands, and everything inside it — owned material included
— travels along and is not evicted in its own right. Because such a folder holds
no "(new) <name>" link to travel with it, a shortcut to it is left behind in the
folder it came from, exactly as for an evicted file.

What then happens, for each item:

  1. Externally-owned files move into their folder's replica inside the
     externals tree, and a shortcut to the moved file is created in the folder
     it came from, so the file is still reachable from where it used to be.
  2. Externally-owned leaf folders move into their parent's replica, once a
     live check confirms they are still empty (or still hold just the one
     shortcut) — a folder cleared by --allow-unowned-folders skips that check,
     since holding content is the point. No shortcut is created for an emptied
     one: reclaim-folders already left a "(new) <name>" link inside it pointing
     at the folder that took over, and that link travels with the folder. A
     folder leaving with its contents has no such link, so it does get a
     shortcut in its old place.

The externals tree replicates the crawl root's folder structure, so an evicted
file keeps its original location and name. Each replica folder is named
"(ext) <original>" to keep it apart from the canonical folder it mirrors, and
holds a "((new)) <original>" shortcut pointing at that folder so the way back is
one click. A folder that actually parts with items of its own gets the link the
other way at the same time: an "(external files) <original>" shortcut pointing
at its replica, created once and never duplicated — if a shortcut of that name
is already there it is left as it stands. A folder that merely sits on the way
to one gets no such shortcut, since its replica holds nothing but other
replicas. A replica is found again by the Drive ID recorded
for it, not by its name, and one left under its bare name by a run from before
the "(ext) " prefix is adopted and renamed rather than duplicated.

Each replica folder gets the original folder's sharing copied onto it, WITHOUT
sending anybody a notification email, so everyone who could reach the file
before still can; only the grants the replica does not already inherit are
created, and roles are compared, so a folder that widens an inherited grant is
matched. Only ancestor folders that actually receive something are created.

externals.root must be a regular My-Drive folder — a shared drive cannot hold
externally-owned files, which is the entire point — and it may sit inside the
crawl root (recommended: evicted files then stay searchable from the crawl
root, stay in the snapshot on every crawl, and stay migratable). It may not sit
inside <folder_id> itself: moving that subtree to a shared drive would take the
externals tree along with it.

The database is updated as the run goes — replica folders and the shortcuts
become nodes rows, and every moved item is reparented under its replica and
stamped with where it came from — so the snapshot keeps describing where things
actually live. Re-crawl afterwards to pick up anything created since the last
crawl.

This command requires the full Drive scope. If the cached token.json only has
read-only access, the tool re-runs consent automatically to obtain it.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		maxErrors, _ := cmd.Flags().GetInt("max-errors")
		concurrency, _ := cmd.Flags().GetInt("concurrency")
		allowUnowned, _ := cmd.Flags().GetBool("allow-unowned-folders")
		return runEvictExternals(dbPath, cfgPath, args[0], dryRun, allowUnowned, maxErrors, concurrency)
	},
}

func init() {
	evictExternalsCmd.Flags().Bool("dry-run", false, "report what would be evicted without changing anything (read-only scope)")
	evictExternalsCmd.Flags().Bool("allow-unowned-folders", false, "evict externally-owned folders that still hold content, taking their contents with them, instead of refusing and pointing at reclaim-folders")
	evictExternalsCmd.Flags().Int("max-errors", 5, "abort once more than this many items fail")
	evictExternalsCmd.Flags().Int("concurrency", defaultMoveConcurrency, "how many item moves to run in parallel (all still share the global rate limiter)")
}

const (
	// extReplicaPrefix marks a folder in the externals tree as a replica: it
	// mirrors a folder of the crawl root but holds only evicted items, and must
	// not be mistaken for the canonical folder of that name.
	extReplicaPrefix = "(ext) "
	// extBackLinkPrefix names the shortcut each replica holds pointing at the
	// folder it mirrors, so somebody who lands among the evicted files can get to
	// where they came from. The doubled parens keep it apart from
	// reclaim-folders' "(new) <name>" links, which point the other way.
	extBackLinkPrefix = "((new)) "
	// extForwardLinkPrefix names the shortcut each replicated folder gets
	// pointing at its own replica, so somebody standing in the original folder
	// can see, and reach, the files that were taken out of it.
	extForwardLinkPrefix = "(external files) "
)

// externalsReplicaName returns the externals-tree replica folder name for an
// original folder name, prefixed and rune-safely truncated like the archive
// tree's replicas.
func externalsReplicaName(original string) string {
	return prefixedReplicaName(extReplicaPrefix, original)
}

// externalsBackLinkName returns the name of the shortcut inside a replica that
// points at the original folder the replica mirrors.
func externalsBackLinkName(original string) string {
	return prefixedReplicaName(extBackLinkPrefix, original)
}

// externalsForwardLinkName returns the name of the shortcut inside an original
// folder that points at the replica holding the items evicted out of it.
func externalsForwardLinkName(original string) string {
	return prefixedReplicaName(extForwardLinkPrefix, original)
}

// isExternalsBackLink reports whether a folder's live child is one of those
// shortcuts: the delete command's replica prune discounts them, since a pointer
// we put there ourselves is not content keeping a folder alive.
func isExternalsBackLink(name, mimeType string) bool {
	return mimeType == shortcutMimeType && strings.HasPrefix(name, extBackLinkPrefix)
}

// maxBlockerExamples bounds how many offending items a refusal lists by name.
// The counts in the message are complete; the examples are there to point at
// where to look.
const maxBlockerExamples = 20

// evictStats holds a run's tallies. The file phase updates them from its worker
// pool, so they are guarded by mu; the shared error budget (embedded) carries
// the failure count and abort logic.
type evictStats struct {
	*errorBudget
	mu           sync.Mutex
	files        int
	folders      int
	shortcuts    int
	backLinks    int
	forwardLinks int
	grants       int
	skipped      int
}

func (s *evictStats) file()        { s.mu.Lock(); s.files++; s.mu.Unlock() }
func (s *evictStats) folder()      { s.mu.Lock(); s.folders++; s.mu.Unlock() }
func (s *evictStats) shortcut()    { s.mu.Lock(); s.shortcuts++; s.mu.Unlock() }
func (s *evictStats) backLink()    { s.mu.Lock(); s.backLinks++; s.mu.Unlock() }
func (s *evictStats) forwardLink() { s.mu.Lock(); s.forwardLinks++; s.mu.Unlock() }
func (s *evictStats) grant(n int)  { s.mu.Lock(); s.grants += n; s.mu.Unlock() }
func (s *evictStats) skip()        { s.mu.Lock(); s.skipped++; s.mu.Unlock() }

func (s *evictStats) fileCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.files }

// externallyOwned reports whether a node's recorded owner sits outside the org:
// not the running account, and not on one of the configured internal domains.
// This is classifyOwner's test with "mine" and "another internal account"
// collapsed — for a move into a shared drive the only thing that matters is
// whether the org owns the item. A node with no recorded owner counts as
// external, exactly as classifyOwner treats one: an owner we could not read is
// not an owner we can vouch for, and evicting it (leaving a shortcut behind) is
// the recoverable mistake, whereas leaving it in place blocks the move.
func externallyOwned(n evictNode, me *drive.User, internalDomains []string) bool {
	if n.ownerEmail.Valid && strings.EqualFold(n.ownerEmail.String, me.EmailAddress) {
		return false
	}
	if n.ownerID.Valid && me.PermissionId != "" && n.ownerID.String == me.PermissionId {
		return false
	}
	return !isInternalEmail(n.ownerEmail, internalDomains)
}

// ownerLabelOf names a node's owner for a message, falling back to the Drive
// user id and then to "(unknown owner)".
func ownerLabelOf(n evictNode) string {
	switch {
	case n.ownerEmail.Valid:
		return n.ownerEmail.String
	case n.ownerID.Valid:
		return "id:" + n.ownerID.String
	default:
		return "(unknown owner)"
	}
}

// isLeafFolder reports whether an externally-owned folder can be moved out
// whole: it holds nothing, or nothing but a single shortcut. That shortcut is
// reclaim-folders' "(new) <name>" link to the folder that took over, which is
// merely a pointer and takes no content with it.
func isLeafFolder(children []evictNode) bool {
	switch len(children) {
	case 0:
		return true
	case 1:
		return children[0].typ == typeShortcut
	default:
		return false
	}
}

// evictPlan is what one run has decided to do, worked out entirely from the
// snapshot before any Drive call that changes something.
type evictPlan struct {
	files   []evictNode // externally-owned files to move, each leaving a shortcut behind
	folders []evictNode // externally-owned leaf folders to move as they stand
	// stuffed is every externally-owned folder in the subtree that still holds
	// content. Without --allow-unowned-folders these are the refusal; with it
	// they are in folders too, and this is what gets reported before they go.
	stuffed []stuffedFolder
}

// stuffedFolder is an externally-owned folder that the snapshot says still holds
// content, so moving it out cannot help taking that content along.
type stuffedFolder struct {
	node  evictNode
	path  string // path below the subtree root, so it can be found by eye
	items int    // how much the snapshot says it holds
}

// carriedDriveIDs is the set of folders that are leaving with their contents, so
// the live leaf check can be waived for exactly those.
func (p evictPlan) carriedDriveIDs() map[string]bool {
	out := make(map[string]bool, len(p.stuffed))
	for _, s := range p.stuffed {
		out[s.node.driveID] = true
	}
	return out
}

// planEviction works out what to evict from the subtree, or returns the refusal
// that has to be dealt with first. nodes is the whole subtree, shallowest
// first, with its root at index 0. allowUnownedFolders waives the refusal over
// externally-owned folders that still hold content: they are then evicted like
// empty ones, taking what is inside them along.
//
// The folders that still hold content are recorded in the plan either way, so
// the caller can list them whether it is about to refuse or about to move them.
func planEviction(nodes []evictNode, me *drive.User, internalDomains []string, subtreePath string, allowUnownedFolders bool) (evictPlan, error) {
	var plan evictPlan
	if len(nodes) == 0 {
		return plan, fmt.Errorf("the subtree is empty in the database; re-run crawl")
	}
	root := nodes[0]
	children := make(map[int64][]evictNode, len(nodes))
	byRow := make(map[int64]evictNode, len(nodes))
	for _, n := range nodes {
		byRow[n.rowID] = n
	}
	for _, n := range nodes[1:] {
		if n.parentRowID.Valid {
			children[n.parentRowID.Int64] = append(children[n.parentRowID.Int64], n)
		}
	}

	// The folder being prepared has to be ours to hand over in the first place;
	// nothing below it can fix that.
	if externallyOwned(root, me, internalDomains) {
		if !root.ownerEmail.Valid {
			return plan, fmt.Errorf("the snapshot records no owner email for %q (%s), so there is no telling whether it can go into a shared drive at all; re-crawl, and if the owner is still unknown check the folder by hand",
				root.name, root.driveID)
		}
		return plan, fmt.Errorf("%q (%s) is itself owned by %s, so it cannot be moved into a shared drive at all; run `drive-cleanup reclaim-folders --email %s --folder %s` first to replace it with a folder you own, then prepare that replacement instead",
			root.name, root.driveID, root.ownerEmail.String, root.ownerEmail.String, root.driveID)
	}

	// Refusal 1: anything still marked delete. Archiving is both the cheaper and
	// the correct way to get unwanted items out of the subtree, and an unwanted
	// file that happens to be unowned must not be dressed up with a shortcut as
	// if it were worth keeping.
	var doomed []evictNode
	for _, n := range nodes {
		if n.decision == decisionDelete {
			doomed = append(doomed, n)
		}
	}
	if len(doomed) > 0 {
		return plan, fmt.Errorf("%d item(s) in %s are still marked delete; run `drive-cleanup archive --folder %s` first so they are out of the subtree instead of being evicted into the externals tree:\n%s",
			len(doomed), subtreeLabel(subtreePath, root), root.driveID, itemExamples(doomed))
	}

	// Refusal 2: externally-owned folders that still hold content. Moving one out
	// would take everything inside it — owned material included — along with it,
	// unless the caller has said that is what it wants.
	//
	// A folder that leaves does so as it stands, so everything below it travels
	// along rather than being evicted in its own right: leaving therefore covers
	// the descendants of a departing folder too, not just its children. Walking
	// shallowest-first is what makes that a single pass.
	leaving := make(map[int64]bool)
	for _, n := range nodes {
		if n.rowID == root.rowID {
			continue
		}
		if n.parentRowID.Valid && leaving[n.parentRowID.Int64] {
			if n.typ == typeFolder {
				leaving[n.rowID] = true
			}
			continue
		}
		if n.typ != typeFolder || !externallyOwned(n, me, internalDomains) {
			continue
		}
		if isLeafFolder(children[n.rowID]) {
			plan.folders = append(plan.folders, n)
			leaving[n.rowID] = true
			continue
		}
		plan.stuffed = append(plan.stuffed, stuffedFolder{
			node:  n,
			path:  pathBelowRoot(n, byRow, root.rowID),
			items: len(children[n.rowID]),
		})
		if allowUnownedFolders {
			plan.folders = append(plan.folders, n)
			leaving[n.rowID] = true
		}
	}
	if len(plan.stuffed) > 0 && !allowUnownedFolders {
		return plan, fmt.Errorf("%d externally-owned folder(s) in %s still hold content (each one listed above), so evicting them would take their contents along; either run `drive-cleanup reclaim-folders` for %s first (see --folder %s to scope it) so that nothing but the folders themselves is left to move, or re-run with --allow-unowned-folders to evict them as they stand, contents and all",
			len(plan.stuffed), subtreeLabel(subtreePath, root), ownerList(plan.stuffed), root.driveID)
	}

	for _, n := range nodes {
		if n.typ == typeFolder || n.rowID == root.rowID || !externallyOwned(n, me, internalDomains) {
			continue
		}
		if n.parentRowID.Valid && leaving[n.parentRowID.Int64] {
			continue
		}
		plan.files = append(plan.files, n)
	}
	return plan, nil
}

// pathBelowRoot names a node by its path below the subtree root, root name
// excluded, so a folder buried a few levels down can be recognised without
// looking its Drive ID up.
func pathBelowRoot(n evictNode, byRow map[int64]evictNode, rootRow int64) string {
	parts := []string{n.name}
	for cur := n; cur.rowID != rootRow && cur.parentRowID.Valid; {
		parent, ok := byRow[cur.parentRowID.Int64]
		if !ok || parent.rowID == rootRow {
			break
		}
		parts = append(parts, parent.name)
		cur = parent
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "/")
}

// reportStuffedFolders lists every externally-owned folder that still holds
// content — the whole list, not a sample — so what is at stake is visible
// before the run either refuses or takes the folders wholesale. allowed says
// which of the two is about to happen.
func reportStuffedFolders(stuffed []stuffedFolder, allowed bool) {
	if len(stuffed) == 0 {
		return
	}
	if allowed {
		fmt.Fprintf(os.Stderr, "%d externally-owned folder(s) still hold content; --allow-unowned-folders was passed, so each moves into the externals tree as it stands and everything inside it travels along:\n", len(stuffed))
	} else {
		fmt.Fprintf(os.Stderr, "%d externally-owned folder(s) still hold content:\n", len(stuffed))
	}
	for _, s := range stuffed {
		fmt.Fprintf(os.Stderr, "  %s (%s)  [owner: %s, holds %d item(s)]\n",
			s.path, s.node.driveID, ownerLabelOf(s.node), s.items)
	}
}

// subtreeLabel names the subtree in a message: its path under the crawl root
// when there is one, else the folder's own name.
func subtreeLabel(subtreePath string, root evictNode) string {
	if subtreePath != "" {
		return fmt.Sprintf("%q", subtreePath)
	}
	return fmt.Sprintf("%q", root.name)
}

// itemExamples renders up to maxBlockerExamples offending items as indented
// lines, with a count of whatever did not fit.
func itemExamples(nodes []evictNode) string {
	var b strings.Builder
	for i, n := range nodes {
		if i == maxBlockerExamples {
			fmt.Fprintf(&b, "  ... and %d more\n", len(nodes)-i)
			break
		}
		fmt.Fprintf(&b, "  %-10s %s (%s)  [owner: %s]\n", n.typ, n.name, n.driveID, ownerLabelOf(n))
	}
	return strings.TrimRight(b.String(), "\n")
}

// ownerList renders the distinct owners of the given folders, in first-seen
// order, for a message telling the user whom to run reclaim-folders for.
func ownerList(folders []stuffedFolder) string {
	seen := make(map[string]bool, len(folders))
	var owners []string
	for _, f := range folders {
		label := ownerLabelOf(f.node)
		if !seen[label] {
			seen[label] = true
			owners = append(owners, label)
		}
	}
	return strings.Join(owners, ", ")
}

func runEvictExternals(dbPath, cfgPath, folderID string, dryRun, allowUnownedFolders bool, maxErrors, concurrency int) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Externals.Root.validate("externals.root"); err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	crawlRoot, err := crawlRootDriveID(db)
	if err == sql.ErrNoRows {
		return fmt.Errorf("database is empty; run crawl first")
	}
	if err != nil {
		return err
	}
	// Evicting moves live files based on the snapshot; a config root that no
	// longer matches what was crawled means every ownership and parent decision
	// could be wrong. Same refusal as archive and reclaim-folders.
	if crawlRoot != cfg.Crawl.Root.ID {
		return fmt.Errorf("crawl root in config (%s, %q) does not match the root in the database (%s); crawl.root.id changed since the last crawl — re-run `drive-cleanup crawl` to rebuild the snapshot before evicting externals",
			cfg.Crawl.Root.ID, cfg.Crawl.Root.Name, crawlRoot)
	}
	if cfg.Externals.Root.ID == crawlRoot {
		return fmt.Errorf("externals.root.id equals crawl.root.id (%s); the externals folder must be a folder of its own", crawlRoot)
	}
	if cfg.Archive.Root.configured() && cfg.Externals.Root.ID == cfg.Archive.Root.ID {
		return fmt.Errorf("externals.root.id equals archive.root.id (%s); evicted files are being kept, archived ones are on their way out — they must not share a folder", cfg.Externals.Root.ID)
	}

	typ, err := nodeTypeByDriveID(db, folderID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("folder %s not found in the database; it must be a folder crawled under the crawl root", folderID)
	}
	if err != nil {
		return err
	}
	if typ != typeFolder {
		return fmt.Errorf("%s is a %s, not a folder", folderID, typ)
	}
	if inside, err := nodeInSubtree(db, crawlRoot, folderID); err != nil {
		return err
	} else if !inside {
		return fmt.Errorf("folder %s is not under the crawl root; evict-externals only acts on the crawled tree", folderID)
	}
	// An externals root inside the crawl root is crawled like anything else, so a
	// folder of it can be named here by mistake. Evicting out of the externals
	// tree into a deeper corner of itself is never what anyone means; the check is
	// free because the target has to be crawled to get this far.
	if inside, err := nodeInSubtree(db, cfg.Externals.Root.ID, folderID); err != nil {
		return err
	} else if inside {
		return fmt.Errorf("folder %s is inside the externals folder %s (%q); that tree is where evicted items live, not something to prepare for a shared drive",
			folderID, cfg.Externals.Root.ID, cfg.Externals.Root.Name)
	}
	subtreePath, err := subtreeRelativePath(db, folderID)
	if err != nil {
		return err
	}

	// A stale snapshot would hide both the items to evict and the content that
	// makes an unowned folder unsafe to move, so an incomplete crawl is fatal
	// here rather than a warning.
	if pending, err := countPendingFolders(db, folderID); err != nil {
		return err
	} else if pending > 0 {
		return fmt.Errorf("the crawl is incomplete (%d folder(s) under %s not fully listed); the database may be missing items. Re-run crawl first", pending, folderID)
	}

	nodes, err := subtreeNodes(db, folderID)
	if err != nil {
		return err
	}

	ctx, cancel := cancelOnSignal()
	defer cancel()

	// A dry run only reads, so request the narrower scope — previewing never
	// forces a write-scope re-consent.
	driveScope := drive.DriveScope
	if dryRun {
		driveScope = drive.DriveReadonlyScope
	}
	svc, err := newDriveService(ctx, driveScope)
	if err != nil {
		return err
	}
	about, err := svc.About.Get().Fields("user").Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("fetching current user info: %w", err)
	}
	me := about.User

	plan, err := planEviction(nodes, me, cfg.InternalDomains, subtreePath, allowUnownedFolders)
	// The folders that still hold content are worth seeing in full whether the
	// run is about to refuse over them or about to take them wholesale.
	reportStuffedFolders(plan.stuffed, allowUnownedFolders)
	if err != nil {
		return err
	}
	if len(plan.files)+len(plan.folders) == 0 {
		fmt.Fprintf(os.Stderr, "Nothing in %s is owned outside the org; it is ready to move to a shared drive.\n",
			subtreeLabel(subtreePath, nodes[0]))
		return nil
	}

	externalsFolder, err := getConfiguredFolder(ctx, svc, cfg.Externals.Root, "externals.root")
	if err != nil {
		return err
	}
	if externalsFolder.DriveId != "" {
		return fmt.Errorf("externals folder %s is in a shared drive (%s); it must be a regular My-Drive folder — evicted files are owned by third parties, which is exactly what a shared drive cannot hold", externalsFolder.Id, externalsFolder.DriveId)
	}
	// Inside the subtree being prepared, the externals tree would be dragged into
	// the shared drive along with it, taking every file this command just rescued.
	if externalsFolder.Id == folderID {
		return fmt.Errorf("externals folder %s (%q) is the folder you are preparing; it must be somewhere else", externalsFolder.Id, externalsFolder.Name)
	}
	if inside, err := folderInsideRoot(ctx, svc, externalsFolder, folderID); err != nil {
		return fmt.Errorf("checking the externals folder against %s: %w", folderID, err)
	} else if inside {
		return fmt.Errorf("externals folder %s (%q) is inside the folder you are preparing (%s); move it outside that subtree — inside, moving the subtree into a shared drive would take the evicted files along with it",
			externalsFolder.Id, externalsFolder.Name, folderID)
	}
	if inside, err := folderInsideRoot(ctx, svc, externalsFolder, crawlRoot); err != nil {
		return fmt.Errorf("checking the externals folder against the crawl root: %w", err)
	} else if !inside {
		log.Printf("NOTE externals folder %q (%s) is outside the crawl root, so no crawl visits it; evicted items keep their rows (they are exempt from stale-row pruning) but their new location is never refreshed",
			externalsFolder.Name, externalsFolder.Id)
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "DRY RUN: no changes will be made. Would evict %d file(s) and %d %s owned outside the org out of %s into %q, leaving %d shortcut(s) behind.\n",
			len(plan.files), len(plan.folders), plan.folderNoun(), subtreeLabel(subtreePath, nodes[0]), externalsFolder.Name, plan.shortcutCount())
	} else {
		fmt.Fprintf(os.Stderr, "About to evict %d file(s) and %d %s owned outside the org out of %s into %q (%s), replicating their folders (with their sharing) and leaving %d shortcut(s) behind.\n",
			len(plan.files), len(plan.folders), plan.folderNoun(), subtreeLabel(subtreePath, nodes[0]), externalsFolder.Name, externalsFolder.Id, plan.shortcutCount())
		if !promptYesNo("Continue? [y/N] ") {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return nil
		}
	}

	// All Drive calls below share this one limiter; it — not the worker count —
	// is the quota safety cap (see pack).
	limiter := rate.NewLimiter(rate.Limit(20), 20)
	rec := &opLog{db: db, account: me.EmailAddress, command: "evict-externals"}

	moveCtx, moveCancel := context.WithCancel(ctx)
	defer moveCancel()
	stats := &evictStats{errorBudget: &errorBudget{cmd: "evict-externals", maxErrors: maxErrors, cancel: moveCancel}}

	e := &evictor{
		db: db, svc: svc, limiter: limiter, rec: rec, me: me, stats: stats,
		externalsRootID:   externalsFolder.Id,
		externalsRootName: externalsFolder.Name,
		root:              replicaRef{driveID: externalsFolder.Id},
		verified:          make(map[string]replicaRef),
		carried:           plan.carriedDriveIDs(),
		receiving:         plan.receivingDriveIDs(),
	}

	// Resolve every replica folder up front, sequentially: the concurrent move
	// phase then only reads the cache, so a replica can never be created twice. A
	// dry run skips this entirely (it would create folders) and previews instead.
	if dryRun {
		e.preview(moveCtx, plan)
		fmt.Fprintf(os.Stderr, "\nWould evict %d file(s) and %d folder(s) into %q, creating %d shortcut(s) where they came from, and prepare %d replica folder(s) — each pointed back at the folder it mirrors, %d of them pointed at from it in turn — with %d grant(s) copied onto them.\n",
			len(plan.files), len(plan.folders), externalsFolder.Name, plan.shortcutCount(), stats.backLinks, stats.forwardLinks, stats.grants)
		return nil
	}

	rootRow, err := upsertReplicaRow(db, externalsFolder.Id, externalsFolder.Name, sql.NullInt64{}, me.EmailAddress)
	if err != nil {
		return fmt.Errorf("recording the externals root: %w", err)
	}
	e.root.rowID = rootRow
	parents := plan.parentDriveIDs()
	prog := newProgress()
	for i, parent := range parents {
		if err := moveCtx.Err(); err != nil {
			return err
		}
		if _, err := e.resolve(moveCtx, parent); err != nil {
			return fmt.Errorf("preparing the externals replica of folder %s: %w", parent, err)
		}
		prog.step("evict-externals: %d/%d replica folder(s) prepared", i+1, len(parents))
	}

	// Phase A: files, concurrently. Each one moves into its folder's replica and
	// then gets a shortcut left behind where it used to be.
	prog = newProgress()
	forEachConcurrent(moveCtx, concurrency, plan.files, func(n evictNode) {
		e.evictFile(moveCtx, n)
		prog.step("evict-externals: %d/%d file(s) evicted", stats.fileCount(), len(plan.files))
	})
	if ctx.Err() != nil {
		log.Printf("interrupted: %d file(s) and %d folder(s) evicted, %d skipped, %d failed",
			stats.files, stats.folders, stats.skipped, stats.failed)
		return ctx.Err()
	}
	if stats.aborted {
		return stats.err
	}

	// Phase B: the folders, sequentially — each one is checked live first, and a
	// folder that has gained content since the crawl is left alone rather than
	// dragging it out of the subtree. The folders --allow-unowned-folders cleared
	// are exempt from that check: content is expected there.
	prog = newProgress()
	for i, n := range plan.folders {
		if moveCtx.Err() != nil {
			break
		}
		e.evictFolder(moveCtx, n)
		prog.step("evict-externals: %d/%d folder(s) processed", i+1, len(plan.folders))
	}
	if ctx.Err() != nil {
		log.Printf("interrupted: %d file(s) and %d folder(s) evicted, %d skipped, %d failed",
			stats.files, stats.folders, stats.skipped, stats.failed)
		return ctx.Err()
	}
	if stats.aborted {
		return stats.err
	}

	log.Printf("done: %d file(s) and %d folder(s) evicted into %q, %d shortcut(s) created, %d replica folder(s) pointed back at their original, %d original folder(s) pointed at their replica, %d grant(s) copied onto replica folders, %d skipped, %d failed",
		stats.files, stats.folders, externalsFolder.Name, stats.shortcuts, stats.backLinks, stats.forwardLinks, stats.grants, stats.skipped, stats.failed)
	if stats.failed > 0 {
		return fmt.Errorf("%d item(s) failed; re-run evict-externals to retry", stats.failed)
	}
	fmt.Fprintf(os.Stderr, "%s now holds only items the org owns, as far as the snapshot knows. Re-crawl and re-run to confirm before moving it into a shared drive.\n",
		subtreeLabel(subtreePath, nodes[0]))
	return nil
}

// folderNoun describes the folders the plan moves. They are normally emptied
// leaf folders, but --allow-unowned-folders lets folders leave with their
// contents, and a message that called those "emptied" would be a lie.
func (p evictPlan) folderNoun() string {
	if len(p.stuffed) > 0 {
		return "folder(s) (contents included)"
	}
	return "emptied folder(s)"
}

// shortcutCount is how many shortcuts the plan would leave behind: one per
// evicted file, except for files that are themselves shortcuts (Drive shortcuts
// cannot point at other shortcuts) and any whose original folder is unrecorded,
// plus one per folder leaving with its contents. An emptied folder gets none —
// reclaim-folders' own link travels inside it.
func (p evictPlan) shortcutCount() int {
	n := 0
	for _, f := range p.files {
		if f.typ != typeShortcut && f.parentDriveID.Valid {
			n++
		}
	}
	for _, s := range p.stuffed {
		if s.node.parentDriveID.Valid {
			n++
		}
	}
	return n
}

// parentDriveIDs returns the distinct folders whose replicas the plan needs: the
// parent of every file, and the parent of every leaf folder.
func (p evictPlan) parentDriveIDs() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(n evictNode) {
		if p := n.parentDriveID; p.Valid && !seen[p.String] {
			seen[p.String] = true
			out = append(out, p.String)
		}
	}
	for _, n := range p.files {
		add(n)
	}
	for _, n := range p.folders {
		add(n)
	}
	return out
}

// receivingDriveIDs is the set of original folders that part with items of
// their own in this run — the parents in the plan. Their replicas are the ones
// that end up holding evicted items directly, as opposed to the ancestor
// replicas created merely to get to them.
func (p evictPlan) receivingDriveIDs() map[string]bool {
	parents := p.parentDriveIDs()
	out := make(map[string]bool, len(parents))
	for _, id := range parents {
		out[id] = true
	}
	return out
}

// evictor carries everything one run needs: the Drive client and its shared rate
// limiter, the audit-log recorder, who we are, and the replica folders resolved
// so far.
type evictor struct {
	db                *sql.DB
	svc               *drive.Service
	limiter           *rate.Limiter
	rec               *opLog
	me                *drive.User
	stats             *evictStats
	externalsRootID   string
	externalsRootName string

	// root is the externals root as a replica: its nodes row is only inserted on
	// a real run, so rowID is zero in a dry run.
	root replicaRef

	// verified maps an original folder's Drive ID to its live-verified replica
	// for this run. Written only by resolve, which runs sequentially before the
	// concurrent phase, and read-only thereafter.
	verified map[string]replicaRef

	// carried holds the Drive IDs of the folders --allow-unowned-folders cleared
	// to leave with their contents, which is what waives the live leaf check for
	// them — and only for them.
	carried map[string]bool

	// receiving holds the Drive IDs of the folders that part with items of their
	// own in this run, so a replica that only exists to hold other replicas can
	// be told apart from one that actually receives something. See
	// ensureForwardLink.
	receiving map[string]bool
}

// resolve returns the replica folder that the contents of the original folder
// parentDriveID belong in, creating any missing replicas along the way. The
// crawl root's replica is the externals root itself.
func (e *evictor) resolve(ctx context.Context, parentDriveID string) (replicaRef, error) {
	if ref, ok := e.verified[parentDriveID]; ok {
		return ref, nil
	}
	chain, err := folderChainToRoot(e.db, parentDriveID)
	if err != nil {
		return replicaRef{}, err
	}
	cur := e.root
	for _, folder := range chain {
		if cur, err = e.ensure(ctx, folder, cur); err != nil {
			return replicaRef{}, err
		}
	}
	e.verified[parentDriveID] = cur
	return cur, nil
}

// ensure returns the live replica of one original folder under parentReplica,
// resolving in order: the cached id (verified live; a trashed or deleted replica
// is re-created), an existing folder we own carrying the replica's name
// (adopted, so re-runs and crashes never duplicate), or a newly created one.
// Either way the original's sharing is copied onto it, which is a no-op once it
// is already there, and it ends up holding a shortcut back to the original —
// which in turn gets a shortcut pointing at the replica.
//
// The replica is named "(ext) <original>". Reuse does not depend on that name:
// the cached externals_folder_drive_id is looked up first, and the by-name
// search accepts a replica left by a run from before the prefix existed and
// renames it, rather than building a second tree beside it.
func (e *evictor) ensure(ctx context.Context, folder archiveTarget, parentReplica replicaRef) (replicaRef, error) {
	if ref, ok := e.verified[folder.driveID]; ok {
		return ref, nil
	}
	name := externalsReplicaName(folder.name)
	var replica *drive.File
	if folder.externalsFolder.Valid {
		f, err := getFileState(ctx, e.svc, e.limiter, folder.externalsFolder.String)
		switch {
		case isNotFound(err):
			// Cached replica no longer exists; fall through and re-create.
		case err != nil:
			return replicaRef{}, fmt.Errorf("verifying cached replica %s of %q: %w", folder.externalsFolder.String, folder.name, err)
		case !f.Trashed:
			replica = f
		}
	}
	if replica == nil {
		f, err := e.adoptReplica(ctx, parentReplica.driveID, name, folder.name)
		if err != nil {
			return replicaRef{}, err
		}
		replica = f
	}
	if replica == nil {
		f, err := e.rec.createFolder(ctx, e.svc, e.limiter, parentReplica.driveID, name)
		if err != nil {
			return replicaRef{}, fmt.Errorf("creating replica %q under %s: %w", name, parentReplica.driveID, err)
		}
		detailf("OK created replica %q (%s) under %s", name, f.Id, parentReplica.driveID)
		replica = f
	}
	// A replica from before the "(ext) " prefix — found by its cached id or by
	// its bare name — is renamed now, so the tree converges on one naming and the
	// next run recognises it either way.
	if replica.Name != "" && replica.Name != name {
		if err := e.rec.renameFile(ctx, e.svc, e.limiter, replica.Id, name); err != nil {
			return replicaRef{}, fmt.Errorf("renaming replica %q (%s) to %q: %w", replica.Name, replica.Id, name, err)
		}
		detailf("OK renamed replica %q (%s) -> %q", replica.Name, replica.Id, name)
		replica.Name = name
	}

	// The originals' sharing is what keeps an evicted file reachable by everyone
	// who could reach it before — including the people a folder gives more access
	// to than its parent does, since only the grants the replica does not already
	// inherit are created. A failure here is logged against the error budget but
	// does not stop the eviction: a file nobody but us can see beats a file
	// stranded in a subtree that cannot move.
	perms, copied, err := copyMissingPermissions(ctx, e.svc, e.limiter, e.rec,
		folder.driveID, folder.name, replica.Id, name, e.me, e.stats.fail)
	if err != nil {
		e.stats.fail("ERROR copying the sharing of %q (%s) onto its replica %s: %v", folder.name, folder.driveID, replica.Id, err)
	}
	e.stats.grant(copied)

	rowID, err := upsertReplicaRow(e.db, replica.Id, name, sql.NullInt64{Int64: parentReplica.rowID, Valid: true}, e.me.EmailAddress)
	if err != nil {
		return replicaRef{}, err
	}
	if perms != nil {
		if err := e.recordReplicaPermissions(replica.Id, perms); err != nil {
			return replicaRef{}, err
		}
	}
	if err := setExternalsFolder(e.db, folder.driveID, replica.Id); err != nil {
		return replicaRef{}, err
	}
	ref := replicaRef{driveID: replica.Id, rowID: rowID}
	e.ensureBackLink(ctx, folder, ref)
	if e.givesUpItems(folder) {
		e.ensureForwardLink(ctx, folder, ref)
	}
	e.verified[folder.driveID] = ref
	return ref, nil
}

// adoptReplica looks for a replica of an original folder already sitting under
// parentReplicaID: one named name, or — for a tree built before the "(ext) "
// prefix — one named exactly like the original. Only a candidate we own counts;
// a folder that happens to share the name and belongs to somebody else is not
// our replica. Returns nil when there is nothing to adopt.
func (e *evictor) adoptReplica(ctx context.Context, parentReplicaID, name, originalName string) (*drive.File, error) {
	for _, candidate := range []string{name, originalName} {
		matches, err := findChildrenNamed(ctx, e.svc, e.limiter, parentReplicaID, candidate, folderMimeType,
			"id, name, owners(emailAddress, permissionId)")
		if err != nil {
			return nil, fmt.Errorf("looking up replica %q: %w", candidate, err)
		}
		for _, m := range matches {
			if ownedByAccount(m, e.me.EmailAddress) {
				return m, nil
			}
		}
	}
	return nil, nil
}

// ensureBackLink puts a shortcut to the original folder inside its replica, so
// anyone who lands among the evicted files can reach the canonical folder they
// came out of. Idempotent: a re-run adopts the shortcut an earlier run left.
//
// A failure is charged to the error budget but does not stop the run — the
// evictions are the point, a signpost is not.
func (e *evictor) ensureBackLink(ctx context.Context, folder archiveTarget, replica replicaRef) {
	name := externalsBackLinkName(folder.name)
	sc, err := e.ensureShortcut(ctx, replica.driveID, name, folder.driveID)
	if err != nil {
		e.stats.fail("ERROR creating the %q shortcut to %q (%s) inside its replica %s: %v",
			name, folder.name, folder.driveID, replica.driveID, err)
		return
	}
	e.stats.backLink()
	detailf("OK replica %s holds %q (%s) -> %q (%s)", replica.driveID, sc.Name, sc.Id, folder.name, folder.driveID)
	if err := e.recordLink(sc, folder.driveID, replica.rowID); err != nil {
		e.stats.fail("ERROR recording the %q shortcut (%s): %v", name, sc.Id, err)
	}
}

// givesUpItems reports whether an original folder parts with items of its own —
// in this run, or in an earlier one — rather than merely being an ancestor on
// the way to a folder that does. Only such a folder is worth a forward link: a
// replica holding nothing but other replicas has nothing to show somebody who
// followed a signpost to it.
func (e *evictor) givesUpItems(folder archiveTarget) bool {
	if e.receiving[folder.driveID] {
		return true
	}
	evicted, err := anyEvictedFrom(e.db, folder.driveID)
	if err != nil {
		log.Printf("WARN checking whether anything was evicted out of %q (%s) before: %v; leaving it unlinked to its replica",
			folder.name, folder.driveID, err)
		return false
	}
	return evicted
}

// ensureForwardLink puts a shortcut to the replica inside the original folder,
// so somebody standing where the files used to be can see where they went —
// the counterpart of ensureBackLink, pointing the other way. Only folders that
// give something up get one (see givesUpItems). It is created once: any
// shortcut already carrying the name is left as it is, whatever it points at,
// rather than a second one being piled on beside it.
//
// A failure is charged to the error budget but does not stop the run, for the
// same reason as the back-link: the evictions are the point, a signpost is not.
func (e *evictor) ensureForwardLink(ctx context.Context, folder archiveTarget, replica replicaRef) {
	name := externalsForwardLinkName(folder.name)
	sc, err := e.ensureNamedShortcut(ctx, folder.driveID, name, replica.driveID)
	if err != nil {
		e.stats.fail("ERROR creating the %q shortcut to the replica %s inside %q (%s): %v",
			name, replica.driveID, folder.name, folder.driveID, err)
		return
	}
	if sc.ShortcutDetails != nil && sc.ShortcutDetails.TargetId != replica.driveID {
		// Somebody else's link of that name, or one left pointing at a replica
		// that has since been re-created. Either way it is not ours to replace.
		log.Printf("WARN %q (%s) already holds a shortcut named %q pointing at %s, not at its replica %s; leaving it alone",
			folder.name, folder.driveID, name, sc.ShortcutDetails.TargetId, replica.driveID)
		return
	}
	e.stats.forwardLink()
	detailf("OK %q (%s) holds %q (%s) -> its replica %s", folder.name, folder.driveID, sc.Name, sc.Id, replica.driveID)
	if err := e.recordLink(sc, replica.driveID, folder.rowID); err != nil {
		e.stats.fail("ERROR recording the %q shortcut (%s): %v", name, sc.Id, err)
	}
}

// recordLink makes one of those signpost shortcuts a nodes row under the folder
// holding it, so the snapshot describes the tree as it really is.
func (e *evictor) recordLink(sc *drive.File, targetDriveID string, parentRow int64) error {
	tx, err := e.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, _, _, _, err := upsertNode(tx, node{
		driveID: sc.Id, name: sc.Name, typ: typeShortcut, mimeType: shortcutMimeType,
		ownerEmail: nullString(e.me.EmailAddress), ownerID: nullString(e.me.PermissionId),
		ownerDisplay: nullString(e.me.DisplayName), shortcutTarget: nullString(targetDriveID),
		parentID: sql.NullInt64{Int64: parentRow, Valid: true}, canEdit: true,
	}, true); err != nil {
		return err
	}
	return tx.Commit()
}

// recordReplicaPermissions writes a replica folder's sharing, as it stands after
// the copy, into folder_permissions — that table is what tells a later run (or a
// human) who could reach a folder.
func (e *evictor) recordReplicaPermissions(replicaDriveID string, perms []*drive.Permission) error {
	tx, err := e.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := replacePermissions(tx, replicaDriveID, permissionRows(perms)); err != nil {
		return err
	}
	return tx.Commit()
}

// evictFile moves one externally-owned file into its folder's replica and leaves
// a shortcut to it behind where it used to be. The move is optimistic (one
// files.update, no pre-read); a failure is diagnosed from the item's live state,
// archive-style.
func (e *evictor) evictFile(ctx context.Context, n evictNode) {
	replica, ok := e.verified[n.parentDriveID.String]
	if !ok { // resolved up front; missing means the pre-phase was interrupted
		return
	}
	err := e.rec.moveFile(ctx, e.svc, e.limiter, n.driveID, replica.driveID, n.parentDriveID.String)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		f, gerr := getFileState(ctx, e.svc, e.limiter, n.driveID)
		if ctx.Err() != nil {
			return
		}
		switch {
		case isNotFound(gerr):
			log.Printf("SKIP %q (%s): no longer exists", n.name, n.driveID)
			e.stats.skip()
			return
		case gerr != nil:
			e.stats.fail("ERROR %q (%s): move failed (%v) and live lookup failed (%v)", n.name, n.driveID, err, gerr)
			return
		case f.Trashed:
			log.Printf("SKIP %q (%s): trashed since the crawl", n.name, n.driveID)
			e.stats.skip()
			return
		case hasParent(f, replica.driveID):
			// Already in the replica — a crash between a previous run's move and
			// its bookkeeping. Finish the bookkeeping now.
			detailf("OK %q (%s): already in the externals replica", n.name, n.driveID)
		default:
			// The recorded parent is stale (the file moved since the crawl); retry
			// from its live parents.
			if merr := e.rec.moveFile(ctx, e.svc, e.limiter, n.driveID, replica.driveID, strings.Join(f.Parents, ",")); merr != nil {
				if ctx.Err() == nil {
					e.stats.fail("ERROR moving %q (%s) into the externals tree: %v", n.name, n.driveID, merr)
				}
				return
			}
			detailf("OK %q (%s) -> externals tree (from live parent %v)", n.name, n.driveID, f.Parents)
		}
	} else {
		detailf("OK %s %q (%s) -> externals tree", n.typ, n.name, n.driveID)
	}
	e.stats.file()

	// A shortcut in the file's old place keeps it reachable from where people
	// expect it. Drive shortcuts cannot point at other shortcuts, so an evicted
	// shortcut gets none — there is nothing to lose, since a shortcut is itself
	// only a pointer and whatever it pointed at has not moved.
	var shortcut *drive.File
	switch {
	case n.typ == typeShortcut:
		detailf("no shortcut left behind for %q (%s): it is itself a shortcut", n.name, n.driveID)
	case !n.parentDriveID.Valid:
		log.Printf("WARN no shortcut left behind for %q (%s): its original folder is not recorded", n.name, n.driveID)
	default:
		sc, err := e.ensureShortcut(ctx, n.parentDriveID.String, n.name, n.driveID)
		if err != nil {
			// Cosmetic next to the move: spend the budget on it but carry on to the
			// bookkeeping, so the snapshot still matches what really happened.
			e.stats.fail("ERROR creating a shortcut to %q (%s) in the folder it came from (%s): %v", n.name, n.driveID, n.parentDriveID.String, err)
		} else {
			shortcut = sc
			e.stats.shortcut()
			detailf("OK created shortcut %q (%s) in %s", sc.Name, sc.Id, n.parentDriveID.String)
		}
	}

	if err := e.record(n, replica, shortcut); err != nil {
		e.stats.fail("ERROR recording the eviction of %q (%s): %v", n.name, n.driveID, err)
	}
}

// evictFolder moves one externally-owned leaf folder into its parent's replica,
// as it stands. A live check comes first: the snapshot said the folder was empty
// (or held nothing but a shortcut), and if that has changed since the crawl the
// folder is left alone rather than carrying content out of the subtree. A folder
// --allow-unowned-folders cleared skips the check — it was known to hold content
// and was accepted on those terms.
func (e *evictor) evictFolder(ctx context.Context, n evictNode) {
	replica, ok := e.verified[n.parentDriveID.String]
	if !ok {
		return
	}
	children, err := listChildren(ctx, e.svc, e.limiter, n.driveID, "nextPageToken, files(id, name, mimeType)")
	if err != nil {
		if isNotFound(err) {
			log.Printf("SKIP folder %q (%s): no longer exists", n.name, n.driveID)
			e.stats.skip()
			return
		}
		if ctx.Err() == nil {
			e.stats.fail("ERROR listing folder %q (%s) before evicting it: %v", n.name, n.driveID, err)
		}
		return
	}
	if !liveLeaf(children) {
		if !e.carried[n.driveID] {
			log.Printf("SKIP folder %q (%s): it holds %d item(s) on Drive, so evicting it would take them out of the subtree; re-crawl and run reclaim-folders for %s, or re-run with --allow-unowned-folders",
				n.name, n.driveID, len(children), ownerLabelOf(n))
			e.stats.skip()
			return
		}
		log.Printf("NOTE folder %q (%s) holds %d item(s) on Drive; --allow-unowned-folders was passed, so they leave the subtree with it",
			n.name, n.driveID, len(children))
	}

	if err := e.rec.moveFile(ctx, e.svc, e.limiter, n.driveID, replica.driveID, n.parentDriveID.String); err != nil {
		if ctx.Err() != nil {
			return
		}
		f, gerr := getFileState(ctx, e.svc, e.limiter, n.driveID)
		switch {
		case ctx.Err() != nil:
			return
		case isNotFound(gerr):
			log.Printf("SKIP folder %q (%s): no longer exists", n.name, n.driveID)
			e.stats.skip()
			return
		case gerr != nil:
			e.stats.fail("ERROR folder %q (%s): move failed (%v) and live lookup failed (%v)", n.name, n.driveID, err, gerr)
			return
		case f.Trashed:
			log.Printf("SKIP folder %q (%s): trashed since the crawl", n.name, n.driveID)
			e.stats.skip()
			return
		case hasParent(f, replica.driveID):
			detailf("OK folder %q (%s): already in the externals replica", n.name, n.driveID)
		default:
			if merr := e.rec.moveFile(ctx, e.svc, e.limiter, n.driveID, replica.driveID, strings.Join(f.Parents, ",")); merr != nil {
				if ctx.Err() == nil {
					e.stats.fail("ERROR moving folder %q (%s) into the externals tree: %v", n.name, n.driveID, merr)
				}
				return
			}
			detailf("OK folder %q (%s) -> externals tree (from live parent %v)", n.name, n.driveID, f.Parents)
		}
	} else {
		detailf("OK folder %q (%s) -> externals tree", n.name, n.driveID)
	}
	e.stats.folder()

	// An emptied folder needs no shortcut: reclaim-folders already left a
	// "(new) <name>" link inside it pointing at the folder that took its contents
	// over, and that link travels with it. A folder leaving with its contents has
	// no such link, and what left is real material, so it gets a shortcut in its
	// old place like an evicted file does. Either way links and bookmarks aimed
	// at the folder keep working — its Drive ID does not change when it moves.
	var shortcut *drive.File
	if e.carried[n.driveID] {
		switch {
		case !n.parentDriveID.Valid:
			log.Printf("WARN no shortcut left behind for folder %q (%s): its original folder is not recorded", n.name, n.driveID)
		default:
			sc, err := e.ensureShortcut(ctx, n.parentDriveID.String, n.name, n.driveID)
			if err != nil {
				// Cosmetic next to the move: charge the budget for it but carry on to
				// the bookkeeping, so the snapshot still matches what really happened.
				e.stats.fail("ERROR creating a shortcut to folder %q (%s) in the folder it came from (%s): %v", n.name, n.driveID, n.parentDriveID.String, err)
			} else {
				shortcut = sc
				e.stats.shortcut()
				detailf("OK created shortcut %q (%s) in %s", sc.Name, sc.Id, n.parentDriveID.String)
			}
		}
	}

	if err := e.record(n, replica, shortcut); err != nil {
		e.stats.fail("ERROR recording the eviction of folder %q (%s): %v", n.name, n.driveID, err)
	}
}

// liveLeaf is isLeafFolder over a live Drive listing: nothing, or nothing but a
// single shortcut.
func liveLeaf(children []*drive.File) bool {
	switch len(children) {
	case 0:
		return true
	case 1:
		return children[0].MimeType == shortcutMimeType
	default:
		return false
	}
}

// ensureShortcut returns the shortcut named name inside parentID that points at
// targetID, creating it if it is not there yet — so a re-run adopts the one an
// earlier run left instead of piling up duplicates.
func (e *evictor) ensureShortcut(ctx context.Context, parentID, name, targetID string) (*drive.File, error) {
	matches, err := findChildrenNamed(ctx, e.svc, e.limiter, parentID, name, shortcutMimeType,
		"id, name, mimeType, shortcutDetails(targetId)")
	if err != nil {
		return nil, err
	}
	for _, m := range matches {
		if isShortcutTo(m, targetID) {
			return m, nil
		}
	}
	return e.rec.createShortcut(ctx, e.svc, e.limiter, parentID, name, targetID)
}

// ensureNamedShortcut returns the shortcut named name inside parentID, creating
// one pointing at targetID when there is none. Unlike ensureShortcut it accepts
// a match whatever it points at: for a signpost placed in a folder that is not
// ours, one link of that name is enough, and a second one beside it — or a
// stranger's link overwritten — would be worse than the stale pointer.
func (e *evictor) ensureNamedShortcut(ctx context.Context, parentID, name, targetID string) (*drive.File, error) {
	matches, err := findChildrenNamed(ctx, e.svc, e.limiter, parentID, name, shortcutMimeType,
		"id, name, mimeType, shortcutDetails(targetId)")
	if err != nil {
		return nil, err
	}
	for _, m := range matches {
		if isShortcutTo(m, targetID) {
			return m, nil
		}
	}
	if len(matches) > 0 {
		return matches[0], nil
	}
	return e.rec.createShortcut(ctx, e.svc, e.limiter, parentID, name, targetID)
}

// record writes one eviction into the snapshot, in a single transaction: the
// moved item is stamped with the folder it came out of and reparented under its
// replica, and the shortcut left in its place (when there is one) becomes a
// nodes row in the folder it came from.
func (e *evictor) record(n evictNode, replica replicaRef, shortcut *drive.File) error {
	tx, err := e.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := markEvicted(tx, n.driveID, n.parentDriveID.String, replica.rowID); err != nil {
		return err
	}
	if shortcut != nil && n.parentRowID.Valid {
		if _, _, _, _, err := upsertNode(tx, node{
			driveID: shortcut.Id, name: shortcut.Name, typ: typeShortcut, mimeType: shortcutMimeType,
			ownerEmail: nullString(e.me.EmailAddress), ownerID: nullString(e.me.PermissionId),
			ownerDisplay: nullString(e.me.DisplayName), shortcutTarget: nullString(n.driveID),
			parentID: n.parentRowID, canEdit: true,
		}, true); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// preview reports what a dry run would do, without creating a replica folder or
// moving anything. Destinations come from the snapshot's paths.
//
// Counting the sharing that would be recreated takes a little care. On a real
// run each replica is created inside its parent replica and so inherits
// everything the parent provides, which is why only a small delta ever gets
// copied — but in a dry run those parent replicas do not exist to be asked. So
// the run is simulated instead: start from what the externals root itself
// provides and walk down the chain, adding each folder's own extra grants to the
// running set as it goes. What each level adds is what a real run would create
// there. An ancestor shared by several files is measured once.
func (e *evictor) preview(ctx context.Context, plan evictPlan) {
	destination := func(n evictNode) string {
		rel, err := subtreeRelativePath(e.db, n.parentDriveID.String)
		if err != nil || rel == "" {
			return e.externalsRootName
		}
		return e.externalsRootName + "/" + rel
	}
	for _, n := range plan.files {
		log.Printf("WOULD move %s %q (%s), owned by %s, into %q and leave a shortcut to it behind",
			n.typ, n.name, n.driveID, ownerLabelOf(n), destination(n))
	}
	carried := plan.carriedDriveIDs()
	for _, n := range plan.folders {
		if carried[n.driveID] {
			log.Printf("WOULD move folder %q (%s), owned by %s, into %q with everything inside it and leave a shortcut to it behind",
				n.name, n.driveID, ownerLabelOf(n), destination(n))
			continue
		}
		log.Printf("WOULD move emptied folder %q (%s), owned by %s, into %q (no shortcut: reclaim-folders' own link travels with it)",
			n.name, n.driveID, ownerLabelOf(n), destination(n))
	}

	rootPerms, err := listPermissions(ctx, e.svc, e.limiter, e.externalsRootID)
	if err != nil {
		log.Printf("WARN listing the sharing of %q (%s): %v; not previewing what would be copied onto replica folders",
			e.externalsRootName, e.externalsRootID, err)
		return
	}
	rootProvides := roleRanks(rootPerms)
	// provides maps an original folder to what its replica would end up
	// providing, inherited grants included.
	provides := make(map[string]map[string]int)
	for _, parent := range plan.parentDriveIDs() {
		if ctx.Err() != nil {
			return
		}
		chain, err := folderChainToRoot(e.db, parent)
		if err != nil {
			log.Printf("WARN previewing the replica chain of %s: %v", parent, err)
			continue
		}
		have := rootProvides
		for _, folder := range chain {
			if known, seen := provides[folder.driveID]; seen {
				have = known
				continue
			}
			perms, err := listPermissions(ctx, e.svc, e.limiter, folder.driveID)
			if err != nil {
				log.Printf("WARN previewing the sharing of %q (%s): %v", folder.name, folder.driveID, err)
				break // without this folder's grants the levels below cannot be estimated
			}
			next := make(map[string]int, len(have)+len(perms))
			for k, rank := range have {
				next[k] = rank
			}
			copied := 0
			for _, p := range perms {
				if !shouldCopyPermission(p, e.me) {
					continue
				}
				key, rank := permissionKey(p), permissionRoleRank(p.Role)
				if next[key] >= rank {
					continue // the replica would already provide at least this much
				}
				next[key] = rank
				copied++
			}
			if copied > 0 {
				log.Printf("WOULD copy %d grant(s) from %q (%s) onto its replica under %q",
					copied, folder.name, folder.driveID, e.externalsRootName)
				e.stats.grant(copied)
			}
			log.Printf("WOULD make sure the replica %q holds a %q shortcut to %q (%s)",
				externalsReplicaName(folder.name), externalsBackLinkName(folder.name), folder.name, folder.driveID)
			e.stats.backLink()
			if e.givesUpItems(folder) {
				log.Printf("WOULD make sure %q (%s) holds a %q shortcut to its replica %q",
					folder.name, folder.driveID, externalsForwardLinkName(folder.name), externalsReplicaName(folder.name))
				e.stats.forwardLink()
			}
			provides[folder.driveID] = next
			have = next
		}
	}
}

// roleRanks reduces a permission list to the best role rank each grantee holds,
// the form permission sets are compared in.
func roleRanks(perms []*drive.Permission) map[string]int {
	out := make(map[string]int, len(perms))
	for _, p := range perms {
		if rank := permissionRoleRank(p.Role); rank > out[permissionKey(p)] {
			out[permissionKey(p)] = rank
		}
	}
	return out
}
