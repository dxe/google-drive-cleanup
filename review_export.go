package main

// export-review renders the keep/delete decision state (nodes.decision, as
// marked in the review web UI) into a set of self-contained HTML files that
// can be emailed to teammates: the full crawled tree plus focused reports that
// surface likely-wrong decisions, all with the same red / green / yellow
// subtree coloring as the UI.

import (
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

var exportReviewCmd = &cobra.Command{
	Use:   "export-review",
	Short: "Write self-contained HTML reviews of keep/delete decisions",
	Long: `Write a set of self-contained HTML files into an output directory, each
showing the crawled tree with keep/delete decisions as marked in the review UI:
delete subtrees red, keep subtrees green, folders containing both yellow, pale
colors for partially-decided subtrees. All CSS/JS is inlined so the files can be
sent to teammates as-is. The files written are:

  review-all.html             the whole crawled tree
  review-delete-recents.html  files marked delete but modified recently
  review-keep-old.html        files marked keep but not touched in years
  review-undecided-old.html   undecided files not modified recently

The "recent" window and "old" threshold are configurable (see --recent-months
and --old-years). config.json's "keep-recent-after" additionally floors what the
delete-recents report calls recent — matching what 'keep-recent' would mark — so
files last modified on or before that date are left out of it. The focused
reports only include files whose last_modified is known (recorded by 'crawl').`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		cfgPath, _ := cmd.Flags().GetString("config")
		outDir, _ := cmd.Flags().GetString("out-dir")
		recentMonths, _ := cmd.Flags().GetInt("recent-months")
		oldYears, _ := cmd.Flags().GetInt("old-years")
		return runExportReview(dbPath, cfgPath, outDir, recentMonths, oldYears)
	},
}

func init() {
	exportReviewCmd.Flags().String("out-dir", "out", "directory to write the review HTML files into")
	exportReviewCmd.Flags().Int("recent-months", 6, `"recently modified" window, in months, for the delete-recents and undecided-old reports`)
	exportReviewCmd.Flags().Int("old-years", 2, `age threshold, in years, for the keep-old report`)
}

func runExportReview(dbPath, cfgPath, outDir string, recentMonths, oldYears int) error {
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// The archive tree holds already-soft-deleted items; keep it out of the
	// reports like the review UI does.
	archiveRootID, err := optionalArchiveRootID(cfgPath)
	if err != nil {
		return err
	}
	roots, err := loadReviewForest(db, archiveRootID)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return fmt.Errorf("database has no crawled nodes; run crawl first")
	}

	keepAfter, hasKeepAfter, err := optionalKeepRecentAfter(cfgPath)
	if err != nil {
		return err
	}

	now := time.Now()
	recentCutoff := now.AddDate(0, -recentMonths, 0)
	oldCutoff := now.AddDate(-oldYears, 0, 0)
	// The delete-recents report exists to catch files keep-recent would have
	// rescued, so it uses keep-recent's window — floored by keep-recent-after —
	// rather than the plain months window the undecided-old report uses.
	deleteRecentCutoff, deleteRecentPhrase := recentWindow(now, recentMonths, keepAfter, hasKeepAfter)

	// modifiedAfter/modifiedBefore only match files whose last_modified is known
	// and valid; a file with no recorded time never lands in a focused report.
	modifiedAfter := func(t time.Time) func(*reviewNode) bool {
		return func(n *reviewNode) bool {
			mt, ok := n.modTime()
			return ok && mt.After(t)
		}
	}
	modifiedBefore := func(t time.Time) func(*reviewNode) bool {
		return func(n *reviewNode) bool {
			mt, ok := n.modTime()
			return ok && mt.Before(t)
		}
	}
	decided := func(dec string, when func(*reviewNode) bool) func(*reviewNode) bool {
		return func(n *reviewNode) bool { return n.decision == dec && when(n) }
	}

	reports := []struct {
		file        string
		title       string
		description string
		roots       []*reviewNode
		expanded    bool
	}{
		{
			file:        "review-all.html",
			title:       "keep / delete plan",
			description: "The whole crawled tree with every keep/delete decision.",
			roots:       roots,
		},
		{
			file:        "review-delete-recents.html",
			title:       "delete, but recently modified",
			description: fmt.Sprintf("Files marked delete despite being modified %s.", deleteRecentPhrase),
			roots:       filterReviewForest(roots, decided(decisionDelete, modifiedAfter(deleteRecentCutoff))),
			expanded:    true,
		},
		{
			file:        "review-keep-old.html",
			title:       "keep, but old",
			description: fmt.Sprintf("Files marked keep despite not being modified in over %s.", yearsPhrase(oldYears)),
			roots:       filterReviewForest(roots, decided(decisionKeep, modifiedBefore(oldCutoff))),
			expanded:    true,
		},
		{
			file:        "review-undecided-old.html",
			title:       "undecided and old",
			description: fmt.Sprintf("Undecided files not modified in the last %s.", monthsPhrase(recentMonths)),
			roots:       filterReviewForest(roots, decided(decisionNone, modifiedBefore(recentCutoff))),
			expanded:    true,
		},
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", outDir, err)
	}
	for _, r := range reports {
		data := buildReviewExport(r.roots, r.title, r.description, r.expanded)
		path := filepath.Join(outDir, r.file)
		if err := writeReviewReport(path, data); err != nil {
			return err
		}
		fmt.Println(path)
	}
	return nil
}

// buildReviewExport tallies the (possibly filtered) forest and packages it for
// the template.
func buildReviewExport(roots []*reviewNode, title, description string, expanded bool) reviewExportData {
	data := reviewExportData{Roots: roots, Title: title, Description: description, Expanded: expanded}
	var tally func(n *reviewNode)
	tally = func(n *reviewNode) {
		if n.typ == typeFolder {
			data.FolderTotals.add(n.decision, 1)
		} else {
			data.FileTotals.add(n.decision, 1)
		}
		for _, c := range n.children {
			tally(c)
		}
	}
	for _, r := range roots {
		tally(r)
	}
	return data
}

func writeReviewReport(path string, data reviewExportData) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer f.Close()
	if err := reviewExportTemplate.Execute(f, data); err != nil {
		return fmt.Errorf("rendering %s: %w", path, err)
	}
	return f.Close()
}

// filterReviewForest returns a pruned deep copy of roots that keeps only the
// files (non-folders) for which match is true, plus the folders on the path to
// them; folders with no surviving descendant are dropped. Subtree/directFiles
// tallies are recomputed on the copy so folder coloring and counts reflect the
// filtered set. Returns nil when nothing matches.
func filterReviewForest(roots []*reviewNode, match func(*reviewNode) bool) []*reviewNode {
	var out []*reviewNode
	for _, r := range roots {
		if c := filterReviewNode(r, match); c != nil {
			out = append(out, c)
		}
	}
	for _, r := range out {
		computeReviewCounts(r)
	}
	return out
}

func filterReviewNode(n *reviewNode, match func(*reviewNode) bool) *reviewNode {
	if n.typ != typeFolder {
		if match(n) {
			cp := *n
			cp.children = nil
			return &cp
		}
		return nil
	}
	var kids []*reviewNode
	for _, c := range n.children {
		if fc := filterReviewNode(c, match); fc != nil {
			kids = append(kids, fc)
		}
	}
	if len(kids) == 0 {
		return nil
	}
	cp := *n
	cp.children = kids
	cp.subtree = decisionCounts{}
	cp.directFiles = decisionCounts{}
	return &cp
}

// modTime parses the node's recorded last_modified (RFC3339). ok is false when
// it is NULL, empty, or unparseable.
func (n *reviewNode) modTime() (time.Time, bool) {
	if !n.lastModified.Valid || n.lastModified.String == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, n.lastModified.String)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// recentWindow resolves what "recently modified" means: the later of the
// months-back window and the config's keep-recent-after floor, plus a phrase
// naming it ("in the last 6 months" / "since 2026-08-17"). The floor lets an
// old file whose last_modified was bumped by mistake stay out of the recent
// set no matter how wide the window is.
func recentWindow(now time.Time, months int, after time.Time, hasAfter bool) (time.Time, string) {
	cutoff := now.AddDate(0, -months, 0)
	if hasAfter && after.After(cutoff) {
		return after, "since " + after.Format("2006-01-02")
	}
	return cutoff, "in the last " + monthsPhrase(months)
}

func monthsPhrase(m int) string {
	if m == 1 {
		return "month"
	}
	return fmt.Sprintf("%d months", m)
}

func yearsPhrase(y int) string {
	if y == 1 {
		return "year"
	}
	return fmt.Sprintf("%d years", y)
}

type reviewExportData struct {
	Title        string
	Description  string
	Expanded     bool
	Roots        []*reviewNode
	FileTotals   decisionCounts
	FolderTotals decisionCounts
}

// Exported accessors for html/template, which cannot read unexported fields.
func (n *reviewNode) Name() string            { return n.name }
func (n *reviewNode) Children() []*reviewNode { return n.children }
func (n *reviewNode) DriveID() string         { return n.driveID }
func (n *reviewNode) IsFolder() bool          { return n.typ == typeFolder }
func (n *reviewNode) Decision() string        { return n.decision }
func (n *reviewNode) IsShortcut() bool        { return n.typ == "shortcut" }

// Status returns the coloring class suffix: folders color by their subtree
// tally, files by their own decision.
func (n *reviewNode) Status() string {
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

// Desc returns the subtree tally excluding the node itself — the counts shown
// on a folder row.
func (n *reviewNode) Desc() decisionCounts {
	d := n.subtree
	d.add(n.decision, -1)
	return d
}

func reviewNodeDriveURL(n *reviewNode) string {
	if n.typ == typeFolder {
		return "https://drive.google.com/drive/folders/" + n.driveID
	}
	return "https://drive.google.com/open?id=" + n.driveID
}

var reviewExportTemplate = template.Must(template.New("review-export").Funcs(template.FuncMap{
	"driveURL": reviewNodeDriveURL,
}).Parse(reviewExportHTML))

// reviewExportHTML is one self-contained document (inline CSS + JS, no
// external references) so it works offline as an email attachment. Same ARIA
// tree + keyboard navigation as the explore-owned-files report.
const reviewExportHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Drive cleanup review — {{.Title}}</title>
<style>
  :root { color-scheme: light dark;
          --red: #e53935; --green: #43a047; --yellow: #f9a825; }
  body { font-family: -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif;
         margin: 0; padding: 1.5rem; line-height: 1.5; }
  header { margin-bottom: 1rem; }
  h1 { font-size: 1.25rem; margin: 0 0 .25rem; }
  .sub { color: #666; font-size: .9rem; }
  .desc { color: #444; font-size: .9rem; margin: .1rem 0 .3rem; }
  .empty { color: #666; font-style: italic; padding: 1rem .35rem; }
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
  .row:focus, .row:focus-within { outline: 2px solid #1a73e8; outline-offset: -2px; }
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
  .name { text-decoration: none; color: inherit; }
  a.name:hover { text-decoration: underline; }
  .chip { font-size: .65rem; font-weight: 700; text-transform: uppercase; border-radius: .25rem;
          padding: 0 .3rem; flex: none; letter-spacing: .03em; }
  .chip-keep   { background: var(--green); color: #fff; }
  .chip-delete { background: var(--red); color: #fff; }
  .counts { color: #777; font-size: .78rem; margin-left: .4rem; white-space: nowrap;
            font-variant-numeric: tabular-nums; }
  .counts .ck { color: var(--green); }
  .counts .cd { color: var(--red); }
  .counts .cu { color: #888; }
  .ext-link { color: #888; display: inline-flex; align-items: center; margin-left: .2rem;
              opacity: .55; flex: none; text-decoration: none; }
  .ext-link:hover { opacity: 1; }
  li > ul { display: none; }
  li[aria-expanded=true] > ul { display: block; }
</style>
</head>
<body{{if .Expanded}} data-expand-all{{end}}>
<header>
  <h1>Drive cleanup review — {{.Title}}</h1>
  {{if .Description}}<div class="desc">{{.Description}}</div>{{end}}
  <div class="sub">
    Folders: <b>{{.FolderTotals.Keep}}</b> keep · <b>{{.FolderTotals.Delete}}</b> delete · <b>{{.FolderTotals.Undecided}}</b> undecided
    &nbsp;|&nbsp;
    Files: <b>{{.FileTotals.Keep}}</b> keep · <b>{{.FileTotals.Delete}}</b> delete · <b>{{.FileTotals.Undecided}}</b> undecided
  </div>
  <div class="legend">
    <span><span class="sw" style="background:color-mix(in srgb, var(--red) 22%, transparent)"></span>delete (whole subtree)</span>
    <span><span class="sw" style="background:color-mix(in srgb, var(--green) 22%, transparent)"></span>keep</span>
    <span><span class="sw" style="background:color-mix(in srgb, var(--yellow) 26%, transparent)"></span>mixed: keep and delete, nothing left undecided</span>
    <span><span class="sw" style="background:color-mix(in srgb, var(--yellow) 11%, transparent)"></span>partly mixed: keep and delete, some still undecided</span>
    <span><span class="sw" style="background:color-mix(in srgb, var(--red) 9%, transparent)"></span>/<span class="sw" style="background:color-mix(in srgb, var(--green) 9%, transparent); margin-left:.3rem"></span>partially decided</span>
    <span><span class="sw"></span>undecided</span>
  </div>
  <div class="hint">Folder counts are its contents: <span style="color:var(--green)">✓ keep</span> · <span style="color:var(--red)">✕ delete</span> · ? undecided.
    Click a ▶ or row to expand. Keyboard: ↑/↓ move, →/← expand/collapse, Enter/Space toggles. ↗ opens the item in Google Drive.</div>
</header>

{{if .Roots}}
<ul role="tree" aria-label="Cleanup review tree">
  {{range .Roots}}{{template "rnode" .}}{{end}}
</ul>
{{else}}
<p class="empty">No matching files.</p>
{{end}}

{{define "rnode"}}
<li role="treeitem"{{if .Children}} aria-expanded="false"{{if gt (len .Children) 6}} data-many{{end}}{{end}}>
  <div class="row st-{{.Status}}" tabindex="-1">
    {{if .Children}}<span class="twisty" aria-hidden="true">▶</span>{{else}}<span class="twisty leaf" aria-hidden="true">▶</span>{{end}}
    <span class="icon">{{if .IsFolder}}📁{{else if .IsShortcut}}↪️{{else}}📄{{end}}</span>
    {{if .IsFolder}}<span class="name">{{.Name}}</span>{{else}}<a class="name" href="{{driveURL .}}" target="_blank" rel="noopener">{{.Name}}</a>{{end}}
    {{if .Decision}}<span class="chip chip-{{.Decision}}">{{.Decision}}</span>{{end}}
    {{if .IsFolder}}{{with .Desc}}<span class="counts"><span class="ck">✓{{.Keep}}</span> <span class="cd">✕{{.Delete}}</span> <span class="cu">?{{.Undecided}}</span></span>{{end}}{{end}}
    <a class="ext-link" href="{{driveURL .}}" target="_blank" rel="noopener" title="Open in Google Drive" tabindex="-1"><svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/></svg></a>
  </div>
  {{if .Children}}
  <ul role="group">
    {{range .Children}}{{template "rnode" .}}{{end}}
  </ul>
  {{end}}
</li>
{{end}}

<script>
(function () {
  var tree = document.querySelector('ul[role=tree]');
  if (!tree) return;

  function rows() {
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
    return li(row).querySelector(':scope > ul > li > .row');
  }
  function parentRow(row) {
    var p = li(row).parentElement.closest('li');
    return p ? p.querySelector(':scope > .row') : null;
  }

  tree.addEventListener('click', function (e) {
    var row = e.target.closest('.row');
    if (!row) return;
    if (e.target.closest('a')) { focusRow(row); return; }
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
      case 'ArrowDown': if (idx < visible.length - 1) focusRow(visible[idx + 1]); break;
      case 'ArrowUp':   if (idx > 0) focusRow(visible[idx - 1]); break;
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
        break;
      case 'Home': focusRow(visible[0]); break;
      case 'End':  focusRow(visible[visible.length - 1]); break;
      default: return;
    }
    e.preventDefault();
  });

  if (document.body.hasAttribute('data-expand-all')) {
    Array.prototype.forEach.call(tree.querySelectorAll('li[aria-expanded]'), function (li) {
      // Leave folders with many children (data-many) collapsed by default.
      if (!li.hasAttribute('data-many')) li.setAttribute('aria-expanded', 'true');
    });
  }

  var first = tree.querySelector('.row');
  if (first) first.tabIndex = 0;
})();
</script>
</body>
</html>
`
