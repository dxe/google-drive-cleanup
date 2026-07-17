package main

// export-review renders the keep/delete decision state (nodes.decision, as
// marked in the review web UI) into one self-contained HTML file that can be
// emailed to teammates: the full crawled tree with the same red / green /
// yellow subtree coloring as the UI.

import (
	"fmt"
	"html/template"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

var exportReviewCmd = &cobra.Command{
	Use:   "export-review",
	Short: "Write a self-contained HTML review of keep/delete decisions",
	Long: `Write a single self-contained HTML file showing the whole crawled tree
with each item's keep/delete decision as marked in the review UI: delete
subtrees red, keep subtrees green, folders containing both yellow, pale colors
for partially-decided subtrees. All CSS/JS is inlined so the file can be sent
to teammates as-is.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		outPath, _ := cmd.Flags().GetString("out")
		return runExportReview(dbPath, outPath)
	},
}

func init() {
	exportReviewCmd.Flags().String("out", "out/review.html", "path of the HTML file to write")
}

func runExportReview(dbPath, outPath string) error {
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	roots, err := loadReviewForest(db)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return fmt.Errorf("database has no crawled nodes; run crawl first")
	}

	var data reviewExportData
	data.Roots = roots
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

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(outPath), err)
	}
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("creating %s: %w", outPath, err)
	}
	defer f.Close()
	if err := reviewExportTemplate.Execute(f, data); err != nil {
		return fmt.Errorf("rendering HTML: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Println(outPath)
	return nil
}

type reviewExportData struct {
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
<title>Drive cleanup review</title>
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
  .row:focus, .row:focus-within { outline: 2px solid #1a73e8; outline-offset: -2px; }
  .st-delete         { background: color-mix(in srgb, var(--red) 22%, transparent); }
  .st-partial-delete { background: color-mix(in srgb, var(--red) 9%, transparent); }
  .st-keep           { background: color-mix(in srgb, var(--green) 22%, transparent); }
  .st-partial-keep   { background: color-mix(in srgb, var(--green) 9%, transparent); }
  .st-mixed          { background: color-mix(in srgb, var(--yellow) 26%, transparent); }
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
<body>
<header>
  <h1>Drive cleanup review — keep / delete plan</h1>
  <div class="sub">
    Folders: <b>{{.FolderTotals.Keep}}</b> keep · <b>{{.FolderTotals.Delete}}</b> delete · <b>{{.FolderTotals.Undecided}}</b> undecided
    &nbsp;|&nbsp;
    Files: <b>{{.FileTotals.Keep}}</b> keep · <b>{{.FileTotals.Delete}}</b> delete · <b>{{.FileTotals.Undecided}}</b> undecided
  </div>
  <div class="legend">
    <span><span class="sw" style="background:color-mix(in srgb, var(--red) 22%, transparent)"></span>delete (whole subtree)</span>
    <span><span class="sw" style="background:color-mix(in srgb, var(--green) 22%, transparent)"></span>keep</span>
    <span><span class="sw" style="background:color-mix(in srgb, var(--yellow) 26%, transparent)"></span>mixed: contains keep and delete</span>
    <span><span class="sw" style="background:color-mix(in srgb, var(--red) 9%, transparent)"></span>/<span class="sw" style="background:color-mix(in srgb, var(--green) 9%, transparent); margin-left:.3rem"></span>partially decided</span>
    <span><span class="sw"></span>undecided</span>
  </div>
  <div class="hint">Folder counts are its contents: <span style="color:var(--green)">✓ keep</span> · <span style="color:var(--red)">✕ delete</span> · ? undecided.
    Click a ▶ or row to expand. Keyboard: ↑/↓ move, →/← expand/collapse, Enter/Space toggles. ↗ opens the item in Google Drive.</div>
</header>

<ul role="tree" aria-label="Cleanup review tree">
  {{range .Roots}}{{template "rnode" .}}{{end}}
</ul>

{{define "rnode"}}
<li role="treeitem"{{if .Children}} aria-expanded="false"{{end}}>
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

  var first = tree.querySelector('.row');
  if (first) first.tabIndex = 0;
})();
</script>
</body>
</html>
`
