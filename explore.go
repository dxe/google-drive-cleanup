package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var exploreCmd = &cobra.Command{
	Use:   "explore-owned-files [account]",
	Short: "Write a self-contained, interactive HTML tree of everything an account owns",
	Long: `Write a single self-contained HTML file showing every file and folder
owned by <account> (a Google email or owner id) in the context of the folder
hierarchy that holds them. Ancestor folders are included so each owned item has
a path; folders owned by the account are bold and every folder shows a count of
the owned items beneath it. Rows carry the same keep/delete coloring as
'export-review' — delete subtrees red, keep green, mixed yellow, pale for
partially-decided ones — and folders show a ✓/✕/? tally of the decisions beneath
them. The tree is collapsible (collapsed by default) and keyboard-navigable. All
CSS/JS is inlined so the file can be emailed as-is.

With no account argument, one HTML file is written per owner found in the
database (skipping owners with neither an email nor an owner id).

With --all-external (and no account argument), a single _all-external.html is
written instead, combining every account whose owner is not on a configured
internal domain into one tree with pooled file/folder counts. There, each folder
also gets a popover breaking its contents down per owner: how many folders and
files each one owns beneath it, and how many of those are keep / delete /
undecided.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		outDir, _ := cmd.Flags().GetString("out")
		allExternal, _ := cmd.Flags().GetBool("all-external")
		var account string
		if len(args) == 1 {
			account = args[0]
		}
		if allExternal && account != "" {
			return fmt.Errorf("--all-external combines every external account; do not also pass an account argument")
		}
		if allExternal {
			return runExploreAllExternal(dbPath, cfgPath, outDir)
		}
		return runExploreOwnedFiles(dbPath, account, outDir)
	},
}

func init() {
	exploreCmd.Flags().String("out", "out/explore-owned-files", "output directory for the generated HTML")
	exploreCmd.Flags().Bool("all-external", false, "write a single _all-external.html combining every account not on an internal domain")
}

// runExploreAllExternal writes one HTML file, _all-external.html, whose tree
// combines every account whose owner is not on a configured internal domain,
// with file/folder counts pooled across all of them.
func runExploreAllExternal(dbPath, cfgPath, outDir string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	crawlRoot, err := crawlRootDriveID(db)
	if err != nil {
		return fmt.Errorf("fetching crawl root: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", outDir, err)
	}

	roots, err := externalOwnedAndAncestors(db, cfg.InternalDomains)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return fmt.Errorf("no files or folders owned by an external account in the database")
	}
	// Per-folder owner breakdowns power the popover button on each folder; only
	// the all-external report shows them.
	buildOwnerBreakdowns(roots)

	outPath := filepath.Join(outDir, "_all-external.html")
	if err := renderOwnedFilesHTML(outPath, "all external accounts", "", crawlRoot, roots); err != nil {
		return err
	}
	fmt.Println(outPath)
	return nil
}

func runExploreOwnedFiles(dbPath, account, outDir string) error {
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	crawlRoot, err := crawlRootDriveID(db)
	if err != nil {
		return fmt.Errorf("fetching crawl root: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", outDir, err)
	}

	// A specific account: write its single file and we're done.
	if account != "" {
		outPath, err := writeOwnedFilesHTML(db, account, outDir, crawlRoot)
		if err != nil {
			return err
		}
		fmt.Println(outPath)
		return nil
	}

	// No account: one file per owner. Reuse the owners report for the list of
	// owner identifiers, skipping the "(unknown)" bucket (neither email nor id),
	// which ownedAndAncestors cannot match.
	owners, err := ownersReport(db, "")
	if err != nil {
		return err
	}
	var written int
	for _, oc := range owners {
		var acct string
		switch {
		case oc.email.Valid:
			acct = oc.email.String
		case oc.ownerID.Valid:
			acct = oc.ownerID.String
		default:
			continue
		}
		outPath, err := writeOwnedFilesHTML(db, acct, outDir, crawlRoot)
		if err != nil {
			return err
		}
		fmt.Println(outPath)
		written++
	}
	fmt.Fprintf(os.Stderr, "\nWrote %d file(s) to %s\n", written, outDir)
	return nil
}

// writeOwnedFilesHTML renders the owned-files HTML for a single account into
// outDir and returns the path written. crawlRoot is passed in so callers
// rendering many accounts look it up only once.
func writeOwnedFilesHTML(db *sql.DB, account, outDir, crawlRoot string) (string, error) {
	roots, displayName, err := ownedAndAncestors(db, account)
	if err != nil {
		return "", err
	}

	outPath := filepath.Join(outDir, sanitizeFilename(account)+".html")
	if err := renderOwnedFilesHTML(outPath, account, displayName, crawlRoot, roots); err != nil {
		return "", err
	}
	return outPath, nil
}

// renderOwnedFilesHTML writes the ownership tree in roots to outPath, tallying
// the pooled owned file/folder counts across every root, split by keep/delete/
// undecided decision. account and displayName populate the report header.
func renderOwnedFilesHTML(outPath, account, displayName, crawlRoot string, roots []*exploreNode) error {
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("creating %s: %w", outPath, err)
	}
	defer f.Close()

	// Only owned items are tallied: the ancestor folders in the tree are there
	// for context and belong to somebody else.
	var totalFolders, totalFiles int
	var folderTotals, fileTotals decisionCounts
	var tally func(n *exploreNode)
	tally = func(n *exploreNode) {
		if n.owned {
			if n.typ == typeFolder {
				totalFolders++
				folderTotals.add(n.decision, 1)
			} else {
				totalFiles++
				fileTotals.add(n.decision, 1)
			}
		}
		for _, c := range n.children {
			tally(c)
		}
	}
	for _, r := range roots {
		tally(r)
	}

	breakdowns, err := ownerBreakdownJSON(roots)
	if err != nil {
		return fmt.Errorf("encoding owner breakdowns: %w", err)
	}

	if err := exploreTemplate.Execute(f, exploreData{
		Account:      account,
		DisplayName:  displayName,
		TotalFolders: totalFolders,
		TotalFiles:   totalFiles,
		FolderTotals: folderTotals,
		FileTotals:   fileTotals,
		CrawlRoot:    crawlRoot,
		Roots:        roots,
		Breakdowns:   breakdowns,
	}); err != nil {
		return fmt.Errorf("rendering HTML: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}

// sanitizeFilename makes account safe as a single path component, keeping the
// readable parts of an email ("@" and ".") but replacing path separators,
// whitespace and control characters with "_".
func sanitizeFilename(account string) string {
	mapped := strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r <= ' ':
			return '_'
		default:
			return r
		}
	}, account)
	if mapped == "" {
		return "account"
	}
	return mapped
}

type exploreData struct {
	Account      string
	DisplayName  string
	TotalFolders int
	TotalFiles   int
	// FolderTotals/FileTotals split the owned folders and files above across
	// keep / delete / undecided.
	FolderTotals decisionCounts
	FileTotals   decisionCounts
	CrawlRoot    string
	Roots        []*exploreNode
	// Breakdowns is a JSON object mapping a folder's Drive ID to its per-owner
	// count rows, consumed by the inline popover script. It is "{}" for reports
	// that don't compute breakdowns (the per-account files).
	Breakdowns template.JS
}

// Exported accessors for html/template, which cannot read unexported fields.
func (n *exploreNode) Name() string             { return n.name }
func (n *exploreNode) Children() []*exploreNode { return n.children }
func (n *exploreNode) OwnedFolders() int        { return n.ownedFolders }
func (n *exploreNode) OwnedFiles() int          { return n.ownedFiles }
func (n *exploreNode) Owned() bool              { return n.owned }
func (n *exploreNode) DriveID() string          { return n.driveID }
func (n *exploreNode) HasBreakdown() bool       { return len(n.breakdown) > 0 }
func (n *exploreNode) Decision() string         { return n.decision }

// Status returns the coloring class suffix, matching export-review: folders
// color by the decision tally of the owned items beneath them, files by their
// own decision.
func (n *exploreNode) Status() string {
	if n.typ == typeFolder {
		return reviewStatus(n.subtree)
	}
	switch n.decision {
	case decisionKeep:
		return "keep"
	case decisionDelete:
		return "delete"
	default:
		return "todo"
	}
}

// Desc returns the subtree decision tally excluding the node itself — the counts
// shown on a folder row, decomposed per owner by that folder's popover.
func (n *exploreNode) Desc() decisionCounts {
	d := n.subtree
	if n.owned {
		d.add(n.decision, -1)
	}
	return d
}

// ownerBreakdownJSON encodes every folder's per-owner breakdown into a JSON
// object keyed by Drive ID, for the inline popover script. Folders without a
// breakdown (e.g. every folder in a per-account report) are omitted, so the
// result is "{}" when the feature is off. json.Marshal escapes <, > and & so the
// blob is safe to inline inside a <script> element.
func ownerBreakdownJSON(roots []*exploreNode) (template.JS, error) {
	byID := make(map[string][]personCount)
	var walk func(n *exploreNode)
	walk = func(n *exploreNode) {
		if len(n.breakdown) > 0 {
			byID[n.driveID] = n.breakdown
		}
		for _, c := range n.children {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	b, err := json.Marshal(byID)
	if err != nil {
		return "", err
	}
	return template.JS(b), nil
}

// driveURL returns the Drive web URL for a node so each row links to the live
// item: folders open the folder view, everything else opens by id.
func driveURL(n *exploreNode) string {
	if n.typ == typeFolder {
		return "https://drive.google.com/drive/folders/" + n.driveID
	}
	return "https://drive.google.com/open?id=" + n.driveID
}

func isFolder(n *exploreNode) bool { return n.typ == typeFolder }

var exploreTemplate = template.Must(template.New("explore").Funcs(template.FuncMap{
	"driveURL": driveURL,
	"isFolder": isFolder,
}).Parse(exploreHTML))

// exploreHTML is one self-contained document (inline CSS + JS, no external
// references) so it works offline as an email attachment. The tree is an ARIA
// tree widget: nested <ul>/<li>, collapsed by default, with roving-tabindex
// keyboard navigation wired up in the inline script.
const exploreHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Owned files — {{.Account}}</title>
<style>
  :root { color-scheme: light dark;
          --red: #e53935; --green: #43a047; --yellow: #f9a825; }
  body { font-family: -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif;
         margin: 0; padding: 1.5rem; line-height: 1.5; }
  header { margin-bottom: 1rem; }
  h1 { font-size: 1.25rem; margin: 0 0 .25rem; }
  .sub { color: #666; font-size: .9rem; }
  .hint { color: #888; font-size: .8rem; margin-top: .5rem; }
  .legend { display: flex; flex-wrap: wrap; gap: .4rem 1rem; margin-top: .6rem; font-size: .8rem; color: #555; }
  .legend .sw { display: inline-block; width: .85rem; height: .85rem; border-radius: .2rem;
                vertical-align: -.12rem; margin-right: .3rem; border: 1px solid #8884; }
  ul { list-style: none; margin: 0; padding-left: 1.25rem; }
  ul[role=tree] { padding-left: 0; }
  li { margin: 0; }
  .row { display: flex; align-items: center; gap: .35rem; padding: .1rem .35rem;
         border-radius: .3rem; cursor: pointer; outline: none; }
  .row:hover { filter: brightness(0.96); }
  /* An outline rather than a background, so the decision coloring stays visible.
     focus-within so the row stays highlighted when its link has focus too. */
  .row:focus, .row:focus-within { outline: 2px solid #1a73e8; outline-offset: -2px; }
  /* Decision coloring, identical to the export-review reports. */
  .st-delete         { background: color-mix(in srgb, var(--red) 22%, transparent); }
  .st-partial-delete { background: color-mix(in srgb, var(--red) 9%, transparent); }
  .st-keep           { background: color-mix(in srgb, var(--green) 22%, transparent); }
  .st-partial-keep   { background: color-mix(in srgb, var(--green) 9%, transparent); }
  .st-mixed          { background: color-mix(in srgb, var(--yellow) 26%, transparent); }
  .st-partial-mixed  { background: color-mix(in srgb, var(--yellow) 11%, transparent); }
  .st-todo           { background: transparent; }
  .twisty { width: 1rem; display: inline-block; text-align: center; color: #888;
            cursor: pointer; user-select: none; flex: none; }
  .twisty.leaf { visibility: hidden; }
  .icon { flex: none; }
  .name { text-decoration: none; color: inherit; cursor: pointer; }
  .name:hover { text-decoration: underline; }
  .owned > .row .name { font-weight: 700; }
  .chip { font-size: .65rem; font-weight: 700; text-transform: uppercase; border-radius: .25rem;
          padding: 0 .3rem; flex: none; letter-spacing: .03em; }
  .chip-keep   { background: var(--green); color: #fff; }
  .chip-delete { background: var(--red); color: #fff; }
  .ext-link { color: #888; display: inline-flex; align-items: center; margin-left: .2rem;
              opacity: .55; flex: none; text-decoration: none; }
  .ext-link:hover { opacity: 1; }
  .counts { color: #777; font-size: .8rem; margin-left: .4rem; white-space: nowrap;
            font-variant-numeric: tabular-nums; }
  .counts .ck { color: var(--green); }
  .counts .cd { color: var(--red); }
  .counts .cu { color: #888; }
  .owners-btn { color: #888; background: none; border: none; padding: 0 .15rem; margin-left: .1rem;
                display: inline-flex; align-items: center; opacity: .55; flex: none; cursor: pointer; }
  .owners-btn:hover { opacity: 1; }
  /* collapsed by default: hide child lists unless the li is expanded */
  li > ul { display: none; }
  li[aria-expanded=true] > ul { display: block; }
  /* per-owner breakdown popover, positioned in JS */
  #owner-popover { position: fixed; z-index: 10; max-width: min(92vw, 42rem); max-height: 60vh; overflow: auto;
                   background: Canvas; color: CanvasText; border: 1px solid #8888; border-radius: .4rem;
                   box-shadow: 0 4px 16px #0004; padding: .5rem .6rem; font-size: .85rem; }
  #owner-popover[hidden] { display: none; }
  #owner-popover h2 { font-size: .8rem; margin: 0 0 .35rem; font-weight: 600; }
  #owner-popover table { border-collapse: collapse; width: 100%; }
  #owner-popover th, #owner-popover td { text-align: left; padding: .1rem .5rem .1rem 0; white-space: nowrap; }
  #owner-popover td.num, #owner-popover th.num { text-align: right; padding-right: 0; padding-left: 1rem; font-variant-numeric: tabular-nums; }
  #owner-popover thead th { color: #888; font-weight: 500; border-bottom: 1px solid #8884; }
  /* Truncate long owner labels (title carries the full text) so the count
     columns always stay in view. */
  #owner-popover td.owner { max-width: 20rem; overflow: hidden; text-overflow: ellipsis; }
  #owner-popover .ck { color: var(--green); }
  #owner-popover .cd { color: var(--red); }
  #owner-popover .cu { color: #888; }
  /* Divider between the "what they own" and "how it's decided" column groups. */
  #owner-popover td.split, #owner-popover th.split { border-left: 1px solid #8884; padding-left: .55rem; }
</style>
</head>
<body>
<header>
  <h1>Files &amp; folders owned by {{.Account}}{{if .DisplayName}} ({{.DisplayName}}){{end}}</h1>
  <div class="sub">📁 {{.TotalFolders}} folders &nbsp; 📄 {{.TotalFiles}} files owned. Bold = owned by this account.</div>
  <div class="sub">
    Owned folders: <b>{{.FolderTotals.Keep}}</b> keep · <b>{{.FolderTotals.Delete}}</b> delete · <b>{{.FolderTotals.Undecided}}</b> undecided
    &nbsp;|&nbsp;
    Owned files: <b>{{.FileTotals.Keep}}</b> keep · <b>{{.FileTotals.Delete}}</b> delete · <b>{{.FileTotals.Undecided}}</b> undecided
  </div>
  <div class="legend">
    <span><span class="sw" style="background:color-mix(in srgb, var(--red) 22%, transparent)"></span>delete (whole subtree)</span>
    <span><span class="sw" style="background:color-mix(in srgb, var(--green) 22%, transparent)"></span>keep</span>
    <span><span class="sw" style="background:color-mix(in srgb, var(--yellow) 26%, transparent)"></span>mixed: keep and delete, nothing left undecided</span>
    <span><span class="sw" style="background:color-mix(in srgb, var(--yellow) 11%, transparent)"></span>partly mixed: keep and delete, some still undecided</span>
    <span><span class="sw" style="background:color-mix(in srgb, var(--red) 9%, transparent)"></span>/<span class="sw" style="background:color-mix(in srgb, var(--green) 9%, transparent); margin-left:.3rem"></span>partially decided</span>
    <span><span class="sw"></span>undecided</span>
  </div>
  <div class="hint">Folder rows count the owned items beneath them — 📁 folders, 📄 files — then how those
    same items are marked: <span style="color:var(--green)">✓ keep</span> · <span style="color:var(--red)">✕ delete</span> · ? undecided.
    Row colors follow that tally; only items shown in this report are counted.</div>
  <div class="hint">Click a ▶ or folder name to expand. Keyboard: ↑/↓ move, →/← expand/collapse, Enter or Space toggles.</div>
  <div class="hint"><a href="https://drive.google.com/drive/search?q=owner:me%20parent:{{.CrawlRoot}}%20-type:folders" target="_blank" rel="noopener">View my files in Drive</a></div>
</header>

<ul role="tree" aria-label="Owned files">
  {{range .Roots}}{{template "node" .}}{{end}}
</ul>

<div id="owner-popover" hidden role="dialog" aria-label="Owners of items in this folder"></div>
<script>var OWNER_BREAKDOWNS = {{.Breakdowns}};</script>

{{define "node"}}
<li role="treeitem"{{if .Owned}} class="owned"{{end}}{{if .Children}} aria-expanded="false"{{end}}>
  <div class="row st-{{.Status}}" tabindex="-1">
    {{if .Children}}<span class="twisty" aria-hidden="true">▶</span>{{else}}<span class="twisty leaf" aria-hidden="true">▶</span>{{end}}
    <span class="icon">{{if isFolder .}}📁{{else}}📄{{end}}</span>
    {{if isFolder .}}<span class="name">{{.Name}}</span><a class="ext-link" href="{{driveURL .}}" target="_blank" rel="noopener" title="Open in Google Drive" tabindex="-1"><svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/></svg></a>{{else}}<a class="name" href="{{driveURL .}}" target="_blank" rel="noopener">{{.Name}}</a>{{end}}
    {{if .Decision}}<span class="chip chip-{{.Decision}}">{{.Decision}}</span>{{end}}
    {{if isFolder .}}<span class="counts">📁 {{.OwnedFolders}} &nbsp; 📄 {{.OwnedFiles}}</span>{{with .Desc}}<span class="counts"><span class="ck">✓{{.Keep}}</span> <span class="cd">✕{{.Delete}}</span> <span class="cu">?{{.Undecided}}</span></span>{{end}}{{end}}
    {{if .HasBreakdown}}<button type="button" class="owners-btn" data-node="{{.DriveID}}" title="Show owners of items in this folder" aria-label="Show owners" tabindex="-1"><svg xmlns="http://www.w3.org/2000/svg" width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/></svg></button>{{end}}
  </div>
  {{if .Children}}
  <ul role="group">
    {{range .Children}}{{template "node" .}}{{end}}
  </ul>
  {{end}}
</li>
{{end}}

<script>
(function () {
  var tree = document.querySelector('ul[role=tree]');
  if (!tree) return;

  function rows() {
    // Visible rows in document order (a row is visible if no ancestor li is collapsed).
    return Array.prototype.filter.call(tree.querySelectorAll('.row'), function (row) {
      for (var li = row.closest('li'); li; li = li.parentElement.closest('li')) {
        var p = li.parentElement.closest('li');
        if (p && p.getAttribute('aria-expanded') === 'false') return false;
      }
      return true;
    });
  }

  function focusRow(row) {
    if (!row) return;
    rows().forEach(function (r) { r.tabIndex = -1; });
    Array.prototype.forEach.call(tree.querySelectorAll('.row'), function (r) { r.tabIndex = -1; });
    row.tabIndex = 0;
    row.focus();
  }

  function li(row) { return row.closest('li'); }
  function isExpandable(row) { return li(row).hasAttribute('aria-expanded'); }
  function isOpen(row) { return li(row).getAttribute('aria-expanded') === 'true'; }
  function setOpen(row, open) {
    if (isExpandable(row)) li(row).setAttribute('aria-expanded', open ? 'true' : 'false');
  }

  function firstChildRow(row) {
    var grp = li(row).querySelector(':scope > ul > li > .row');
    return grp || null;
  }
  function parentRow(row) {
    var p = li(row).parentElement.closest('li');
    return p ? p.querySelector(':scope > .row') : null;
  }

  tree.addEventListener('click', function (e) {
    var row = e.target.closest('.row');
    if (!row) return;
    var ownersBtn = e.target.closest('.owners-btn');
    if (ownersBtn) { toggleOwnerPopover(ownersBtn); focusRow(row); return; }
    if (e.target.closest('.ext-link')) { focusRow(row); return; }
    if (isExpandable(row)) {
      setOpen(row, !isOpen(row));
      e.preventDefault();
    }
    focusRow(row);
  });

  tree.addEventListener('keydown', function (e) {
    var row = e.target.closest('.row');
    if (!row) return;
    var visible = rows();
    var idx = visible.indexOf(row);
    switch (e.key) {
      case 'ArrowDown':
        if (idx < visible.length - 1) focusRow(visible[idx + 1]);
        break;
      case 'ArrowUp':
        if (idx > 0) focusRow(visible[idx - 1]);
        break;
      case 'ArrowRight':
        if (isExpandable(row) && !isOpen(row)) { setOpen(row, true); }
        else if (isExpandable(row)) { focusRow(firstChildRow(row)); }
        break;
      case 'ArrowLeft':
        if (isExpandable(row) && isOpen(row)) { setOpen(row, false); }
        else { focusRow(parentRow(row)); }
        break;
      case 'Enter':
      case ' ':
        if (isExpandable(row)) setOpen(row, !isOpen(row));
        else { var link = row.querySelector('a.name') || row.querySelector('.ext-link'); if (link) window.open(link.href, '_blank'); }
        break;
      case 'Home':
        focusRow(visible[0]);
        break;
      case 'End':
        focusRow(visible[visible.length - 1]);
        break;
      default:
        return;
    }
    e.preventDefault();
  });

  // --- Per-owner breakdown popover ---
  var popover = document.getElementById('owner-popover');
  var popBtn = null; // the button that opened the popover, if any

  function buildOwnerPopover(rowsData) {
    popover.textContent = '';
    var h = document.createElement('h2');
    h.textContent = rowsData.length + (rowsData.length === 1 ? ' external owner' : ' external owners') + ' in this folder';
    popover.appendChild(h);

    // Owner, then what they own by type, then how those items are decided.
    var COLS = [
      {head: '📁', key: 'folders', cls: '',   title: 'folders owned'},
      {head: '📄', key: 'files',   cls: '',   title: 'files owned'},
      {head: '✓',  key: 'keep',    cls: 'ck split', title: 'marked keep'},
      {head: '✕',  key: 'delete',  cls: 'cd', title: 'marked delete'},
      {head: '?',  key: 'undecided', cls: 'cu', title: 'undecided'}
    ];

    var table = document.createElement('table');
    var thead = document.createElement('thead');
    var htr = document.createElement('tr');
    var owner = document.createElement('th');
    owner.textContent = 'Owner';
    htr.appendChild(owner);
    COLS.forEach(function (c) {
      var th = document.createElement('th');
      th.textContent = c.head;
      th.title = c.title;
      th.className = 'num ' + c.cls;
      htr.appendChild(th);
    });
    thead.appendChild(htr);
    table.appendChild(thead);

    var tbody = document.createElement('tbody');
    rowsData.forEach(function (r) {
      var tr = document.createElement('tr');
      var name = document.createElement('td');
      name.className = 'owner';
      name.textContent = r.label;
      name.title = r.label;
      tr.appendChild(name);
      COLS.forEach(function (c) {
        var td = document.createElement('td');
        td.className = 'num ' + c.cls;
        td.textContent = r[c.key];
        tr.appendChild(td);
      });
      tbody.appendChild(tr);
    });
    table.appendChild(tbody);
    popover.appendChild(table);
  }

  function positionOwnerPopover(btn) {
    var r = btn.getBoundingClientRect();
    // Measure while shown but invisible so offsetWidth/Height are real.
    popover.style.visibility = 'hidden';
    popover.hidden = false;
    var pw = popover.offsetWidth, ph = popover.offsetHeight;
    var left = Math.max(8, Math.min(r.left, window.innerWidth - pw - 8));
    var top = r.bottom + 6;
    if (top + ph > window.innerHeight - 8) top = Math.max(8, r.top - ph - 6);
    popover.style.left = left + 'px';
    popover.style.top = top + 'px';
    popover.style.visibility = '';
  }

  function hideOwnerPopover() {
    popover.hidden = true;
    popBtn = null;
  }

  function toggleOwnerPopover(btn) {
    if (popBtn === btn && !popover.hidden) { hideOwnerPopover(); return; }
    var data = OWNER_BREAKDOWNS[btn.getAttribute('data-node')] || [];
    buildOwnerPopover(data);
    popBtn = btn;
    positionOwnerPopover(btn);
  }

  document.addEventListener('click', function (e) {
    if (popover.hidden) return;
    if (e.target.closest('.owners-btn')) return;  // the tree handler toggles these
    if (!e.target.closest('#owner-popover')) hideOwnerPopover();
  });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && !popover.hidden) {
      var btn = popBtn;
      hideOwnerPopover();
      if (btn) btn.closest('.row').focus();
    }
  });
  // Keep the popover pinned to its button as the tree scrolls or the window resizes.
  window.addEventListener('scroll', function () { if (popBtn) positionOwnerPopover(popBtn); }, true);
  window.addEventListener('resize', function () { if (popBtn) positionOwnerPopover(popBtn); });

  // Make the first root focusable to start.
  var first = tree.querySelector('.row');
  if (first) first.tabIndex = 0;
})();
</script>
</body>
</html>
`
