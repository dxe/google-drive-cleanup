package main

// The gen-evicted-files-request-emails subcommand: draft one email per external
// owner asking them to hand their files over.
//
// evict-externals moves externally-owned files out of a subtree bound for a
// shared drive and into the externals tree (config externals.root), leaving each
// file in an "(ext) <original>" replica of the folder it came from. That keeps
// the files visible, but they are still theirs, so the subtree they came from
// can never be fully ours until their owners move them. The replica each file
// landed in holds a "((new)) <original>" shortcut pointing at the folder that
// took over, so the move an owner has to make is a single drag, in place, with
// no navigating: open the replica, drag your files onto the shortcut.
//
// This command writes the email that asks for exactly that. It reads only the
// database — no Drive calls — grouping every externally-owned file under the
// externals root by the folder it currently sits in, and writing one plain-text
// file per owner to out/evicted-files-request-emails/<email>.txt. Each folder
// the owner has files in is listed by its Drive URL, with a sample of the
// filenames beneath it so they can tell at a glance what is being asked for.
//
// Only replica folders are asked about, and a replica is recognised by holding
// the "((new)) <original>" shortcut — the one thing the email tells its reader
// to drag onto. The externals tree can hold folders that are not replicas: an
// externally-owned folder evicted whole (evict-externals --allow-unowned-folders)
// arrives with its contents and no back-link inside it, so there is nothing in
// it for its owner to drag to and no draft should claim otherwise. Files in such
// a folder are counted and reported instead — they still need dealing with, just
// not by this email.
//
// "Externally owned" means what it means everywhere else in this tool: an owner
// that is not on one of the configured internal-domains. The shortcuts
// evict-externals itself leaves in the tree are skipped whoever owns them —
// they are our signposts, not anybody's files.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var genEvictedFilesRequestEmailsCmd = &cobra.Command{
	Use:   "gen-evicted-files-request-emails",
	Short: "Draft one email per external owner asking them to move their evicted files",
	Long: `Write one plain-text email per external owner, asking them to move the files
of theirs that evict-externals parked in the externals tree (config
externals.root).

For every externally-owned file sitting in a replica folder under the externals
root, that folder is listed by its Drive URL, together with a sample of that
owner's filenames in it. The ask is the same for every folder listed: drag your
files onto the "((new)) ..." shortcut sitting beside them, which points at the
folder that took over from the original.

Only replica folders are listed, and a replica is one holding that "((new)) ..."
shortcut — the very thing the email says to drag onto. A folder without one
cannot be asked about, since there would be nowhere to drag to: an
externally-owned folder that evict-externals moved out whole
(--allow-unowned-folders) is such a folder. Externally-owned files inside one are
counted and reported on stderr rather than written into a draft.

One file is written per owner, named for their email:

    out/evicted-files-request-emails/<email>.txt

Nothing is sent — these are drafts to review and send yourself.

"Externally owned" means an owner that is not on one of the configured
internal-domains. Folders are not listed as items: evict-externals only moves an
externally-owned folder once reclaim-folders has emptied it, so there is nothing
in one worth asking for. Files whose owner has no recorded email address cannot
be written to a per-email draft; they too are counted and reported on stderr.

This reads only the database; re-run crawl first if it is stale.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		outDir, _ := cmd.Flags().GetString("out")
		maxFiles, _ := cmd.Flags().GetInt("max-files")
		return runGenEvictedFilesRequestEmails(dbPath, cfgPath, outDir, maxFiles)
	},
}

func init() {
	genEvictedFilesRequestEmailsCmd.Flags().String("out", "out/evicted-files-request-emails", "output directory for the generated emails")
	genEvictedFilesRequestEmailsCmd.Flags().Int("max-files", 5, "how many filenames to sample per folder")
}

// evictedFileGroup is one folder in one owner's email: the folder their files
// sit in, and their files in it.
type evictedFileGroup struct {
	folderDriveID string
	// folderPath is the folder's path from the externals root, used only to sort
	// the groups so a draft reads top-down and regenerates identically.
	folderPath string
	files      []string
}

// externalsSubtreeNode is a row of the externals tree as this command reads it.
type externalsSubtreeNode struct {
	rowID       int64
	driveID     string
	name        string
	typ         string
	mimeType    string
	ownerEmail  sql.NullString
	parentRowID sql.NullInt64
	// targetDriveID is a shortcut's target; empty for everything else. A
	// back-link with no target points nowhere, so the folder holding it is not a
	// replica anyone can be told to drag into.
	targetDriveID sql.NullString
}

func runGenEvictedFilesRequestEmails(dbPath, cfgPath, outDir string, maxFiles int) error {
	if maxFiles <= 0 {
		return fmt.Errorf("--max-files must be a positive number of filenames")
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Externals.Root.validate("externals.root"); err != nil {
		return err
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	nodes, err := externalsSubtree(db, cfg.Externals.Root.ID)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("externals root %s (%s) is not in the database; re-run crawl",
			cfg.Externals.Root.ID, cfg.Externals.Root.Name)
	}

	byOwner, unaddressable, outsideReplica := groupEvictedFilesByOwner(nodes, cfg.InternalDomains, cfg.Externals.Root.ID)
	if len(byOwner) == 0 {
		fmt.Fprintf(os.Stderr, "No externally-owned files in a replica folder under %s. Nothing to write.\n",
			cfg.Externals.Root.Name)
		reportUndraftable(unaddressable, outsideReplica)
		return nil
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", outDir, err)
	}

	emails := make([]string, 0, len(byOwner))
	for email := range byOwner {
		emails = append(emails, email)
	}
	sort.Strings(emails)

	var totalFiles int
	for _, email := range emails {
		groups := byOwner[email]
		for _, g := range groups {
			totalFiles += len(g.files)
		}
		outPath := filepath.Join(outDir, sanitizeFilename(email)+".txt")
		if err := os.WriteFile(outPath, []byte(renderEvictedFilesEmail(email, groups, maxFiles)), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", outPath, err)
		}
		detailf("wrote %s (%d folder(s))", outPath, len(groups))
		fmt.Println(outPath)
	}

	fmt.Fprintf(os.Stderr, "Wrote %d email(s) in %s covering %d evicted file(s).\n",
		len(emails), outDir, totalFiles)
	reportUndraftable(unaddressable, outsideReplica)
	return nil
}

// reportUndraftable names the externally-owned files no draft could cover. Both
// kinds still need dealing with, so they are said out loud rather than dropped
// on the floor.
func reportUndraftable(unaddressable, outsideReplica int) {
	if unaddressable > 0 {
		fmt.Fprintf(os.Stderr,
			"%d externally-owned file(s) have no recorded owner email and were left out; "+
				"re-run crawl if their ownership should be known.\n", unaddressable)
	}
	if outsideReplica > 0 {
		fmt.Fprintf(os.Stderr,
			"%d externally-owned file(s) are in a folder holding no %q shortcut and were left out; "+
				"there is nowhere in such a folder for their owner to drag them to. Run "+
				"reclaim-folders on the folder and re-evict, or move them by hand.\n",
			outsideReplica, extBackLinkPrefix+"...")
	}
}

// externalsSubtree reads every node under the externals root, folders included,
// so the caller can both pick out the files and walk each file's parent chain
// for a path.
func externalsSubtree(db *sql.DB, rootDriveID string) ([]externalsSubtreeNode, error) {
	rows, err := db.Query(`
		WITH RECURSIVE subtree(id, depth) AS (
			SELECT id, 0 FROM nodes WHERE drive_id = ?
			UNION ALL
			SELECT n.id, s.depth + 1 FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		SELECT n.id, n.drive_id, n.name, n.type, n.mime_type, n.owner_email, n.parent_id,
		       n.shortcut_target_id
		FROM nodes n
		JOIN subtree s ON s.id = n.id
		ORDER BY s.depth, n.id`, rootDriveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []externalsSubtreeNode
	for rows.Next() {
		var n externalsSubtreeNode
		if err := rows.Scan(&n.rowID, &n.driveID, &n.name, &n.typ, &n.mimeType,
			&n.ownerEmail, &n.parentRowID, &n.targetDriveID); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// groupEvictedFilesByOwner buckets the externally-owned files in the externals
// tree by owner email and then by the replica folder they sit in. It returns
// those buckets, each sorted by folder path with filenames sorted within a
// folder, plus the two counts of files it could not put in a draft: those with
// no owner email to address, and those in a folder that is not a replica.
//
// Only replica folders are considered, because only they hold the "((new)) ..."
// shortcut the email tells its reader to drag onto — see isReplicaFolder.
//
// Skipped, whoever owns them: folders (an evicted one is empty by construction —
// see the command's Long), the externals root itself, and the "((new)) ..." /
// "(external files) ..." shortcuts evict-externals leaves behind, which are our
// own signposts rather than anybody's files.
func groupEvictedFilesByOwner(nodes []externalsSubtreeNode, internalDomains []string, rootDriveID string) (byOwner map[string][]evictedFileGroup, unaddressable, outsideReplica int) {
	byRowID := make(map[int64]externalsSubtreeNode, len(nodes))
	for _, n := range nodes {
		byRowID[n.rowID] = n
	}

	replicas := replicaFolderRowIDs(nodes)

	// grouped is owner email -> folder drive id -> that owner's files in it.
	grouped := map[string]map[string]*evictedFileGroup{}

	for _, n := range nodes {
		if n.typ == typeFolder || n.driveID == rootDriveID {
			continue
		}
		if isExternalsBackLink(n.name, n.mimeType) || isExternalsForwardLink(n.name, n.mimeType) {
			continue
		}
		if isInternalEmail(n.ownerEmail, internalDomains) {
			continue
		}
		if !n.ownerEmail.Valid || strings.TrimSpace(n.ownerEmail.String) == "" {
			unaddressable++
			continue
		}
		parent, ok := externalsParentOf(byRowID, n)
		if !ok || !replicas[parent.rowID] {
			// Either no parent row at all (the recursive walk means this should not
			// happen) or a folder with no "((new)) ..." shortcut in it — an
			// externally-owned folder evicted whole, say. Asking its owner to drag
			// onto a shortcut that is not there would send them in circles, so the
			// file is reported rather than drafted.
			outsideReplica++
			continue
		}
		email := n.ownerEmail.String
		folders, ok := grouped[email]
		if !ok {
			folders = map[string]*evictedFileGroup{}
			grouped[email] = folders
		}
		g, ok := folders[parent.driveID]
		if !ok {
			g = &evictedFileGroup{
				folderDriveID: parent.driveID,
				folderPath:    externalsPath(byRowID, parent),
			}
			folders[parent.driveID] = g
		}
		g.files = append(g.files, n.name)
	}

	out := make(map[string][]evictedFileGroup, len(grouped))
	for email, folders := range grouped {
		groups := make([]evictedFileGroup, 0, len(folders))
		for _, g := range folders {
			sort.Strings(g.files)
			groups = append(groups, *g)
		}
		// Path first so a draft reads as a walk down the tree; drive id breaks
		// the tie between two same-named folders so the output is stable.
		sort.Slice(groups, func(i, j int) bool {
			if groups[i].folderPath != groups[j].folderPath {
				return groups[i].folderPath < groups[j].folderPath
			}
			return groups[i].folderDriveID < groups[j].folderDriveID
		})
		out[email] = groups
	}
	return out, unaddressable, outsideReplica
}

// replicaFolderRowIDs picks out the folders in the externals tree that are
// replicas: the ones holding a "((new)) <original>" back-link pointing at the
// folder they mirror. That shortcut is what the email tells its reader to drag
// onto, so a folder without one is a folder nobody can be given instructions
// for — the externals root itself, and any externally-owned folder that
// evict-externals moved out whole rather than emptied first.
//
// A replica is recognised by what is in it rather than by its "(ext) " name or
// by the externals_folder_drive_id cached on the original, because the back-link
// is the thing the instructions actually depend on being there.
func replicaFolderRowIDs(nodes []externalsSubtreeNode) map[int64]bool {
	replicas := map[int64]bool{}
	for _, n := range nodes {
		if !isExternalsBackLink(n.name, n.mimeType) {
			continue
		}
		if !n.targetDriveID.Valid || n.targetDriveID.String == "" {
			continue // points nowhere; nothing to drag onto
		}
		if n.parentRowID.Valid {
			replicas[n.parentRowID.Int64] = true
		}
	}
	return replicas
}

// isExternalsForwardLink is isExternalsBackLink's sibling for the "(external
// files) <name>" shortcut evict-externals leaves in an original folder. Neither
// kind is a file anyone should be asked to move.
func isExternalsForwardLink(name, mimeType string) bool {
	return mimeType == shortcutMimeType && strings.HasPrefix(name, extForwardLinkPrefix)
}

// externalsParentOf resolves a node's parent within the externals subtree.
func externalsParentOf(byRowID map[int64]externalsSubtreeNode, n externalsSubtreeNode) (externalsSubtreeNode, bool) {
	if !n.parentRowID.Valid {
		return externalsSubtreeNode{}, false
	}
	p, ok := byRowID[n.parentRowID.Int64]
	return p, ok
}

// externalsPath renders a folder's path from the externals root ("/"-joined),
// for sorting only. It walks up through the subtree map, so it stops at the
// root, and guards against a cycle in the parent chain.
func externalsPath(byRowID map[int64]externalsSubtreeNode, n externalsSubtreeNode) string {
	var segments []string
	seen := map[int64]bool{}
	for cur := n; !seen[cur.rowID]; {
		seen[cur.rowID] = true
		segments = append(segments, cur.name)
		p, ok := externalsParentOf(byRowID, cur)
		if !ok {
			break
		}
		cur = p
	}
	for i, j := 0, len(segments)-1; i < j; i, j = i+1, j-1 {
		segments[i], segments[j] = segments[j], segments[i]
	}
	return strings.Join(segments, "/")
}

// renderEvictedFilesEmail renders one owner's draft. The recipient is a stranger
// being asked for a favour, so the body opens by saying who is asking and why
// they and not we have to do the moving. Each folder is named by its Drive URL
// alone — the ask is to open it and drag, so the URL is the only part of it the
// recipient needs — followed by up to maxFiles of their filenames in it, and a
// count of the rest when there are more.
func renderEvictedFilesEmail(email string, groups []evictedFileGroup, maxFiles int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "To: %s\n", email)
	fmt.Fprintf(&b, "Subject: Please move your files to our Shared Drive\n")
	fmt.Fprintf(&b, "Body:\n\n")
	fmt.Fprintf(&b, "Hi,\n\n")
	fmt.Fprintf(&b, "I'm Alex from DxE tech team. I'm trying to help migrate DxE's files to Shared Drives.\n\n")
	fmt.Fprintf(&b, "You own some files that we would like to get moved over, but couldn't because files can only be moved by their owners.\n\n")
	folders := "the folders"
	if len(groups) == 1 {
		folders = "the folder"
	}
	fmt.Fprintf(&b, "If you're willing, please take a look at %s listed below, and drag the files you own to the %q shortcut you see in the same folder.\n", folders, extBackLinkPrefix+"...")
	for _, g := range groups {
		fmt.Fprintf(&b, "\n~~ %s ~~\n", driveFolderURL(g.folderDriveID))
		fmt.Fprintf(&b, "files you own in this folder:\n")
		shown := g.files
		if len(shown) > maxFiles {
			shown = shown[:maxFiles]
		}
		for _, name := range shown {
			fmt.Fprintf(&b, "%s\n", name)
		}
		if rest := len(g.files) - len(shown); rest > 0 {
			fmt.Fprintf(&b, "... and %d more\n", rest)
		}
	}
	fmt.Fprintf(&b, "\nThank you!\nAlex T\n")
	return b.String()
}
