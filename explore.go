package main

import (
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var exploreCmd = &cobra.Command{
	Use:   "explore-owned-files <account>",
	Short: "Write a self-contained, interactive HTML tree of everything an account owns",
	Long: `Write a single self-contained HTML file showing every file and folder
owned by <account> (a Google email or owner id) in the context of the folder
hierarchy that holds them. Ancestor folders are included so each owned item has
a path; folders owned by the account are bold and every folder shows a count of
the owned items beneath it. The tree is collapsible (collapsed by default) and
keyboard-navigable. All CSS/JS is inlined so the file can be emailed as-is.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		outDir, _ := cmd.Flags().GetString("out")
		return runExploreOwnedFiles(dbPath, args[0], outDir)
	},
}

func init() {
	exploreCmd.Flags().String("out", "out/explore-owned-files", "output directory for the generated HTML")
}

func runExploreOwnedFiles(dbPath, account, outDir string) error {
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	roots, displayName, err := ownedAndAncestors(db, account)
	if err != nil {
		return err
	}
	crawlRoot, err := crawlRootDriveID(db)
	if err != nil {
		return fmt.Errorf("fetching crawl root: %w", err)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", outDir, err)
	}
	outPath := filepath.Join(outDir, sanitizeFilename(account)+".html")
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("creating %s: %w", outPath, err)
	}
	defer f.Close()

	var totalFolders, totalFiles int
	for _, r := range roots {
		totalFolders += r.ownedFolders
		totalFiles += r.ownedFiles
		if r.owned {
			if r.typ == typeFolder {
				totalFolders++
			} else {
				totalFiles++
			}
		}
	}

	if err := exploreTemplate.Execute(f, exploreData{
		Account:      account,
		DisplayName:  displayName,
		TotalFolders: totalFolders,
		TotalFiles:   totalFiles,
		CrawlRoot:    crawlRoot,
		Roots:        roots,
	}); err != nil {
		return fmt.Errorf("rendering HTML: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Println(outPath)
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
	CrawlRoot    string
	Roots        []*exploreNode
}

// Exported accessors for html/template, which cannot read unexported fields.
func (n *exploreNode) Name() string             { return n.name }
func (n *exploreNode) Children() []*exploreNode { return n.children }
func (n *exploreNode) OwnedFolders() int        { return n.ownedFolders }
func (n *exploreNode) OwnedFiles() int          { return n.ownedFiles }
func (n *exploreNode) Owned() bool              { return n.owned }

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
  :root { color-scheme: light dark; }
  body { font-family: -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif;
         margin: 0; padding: 1.5rem; line-height: 1.5; }
  header { margin-bottom: 1rem; }
  h1 { font-size: 1.25rem; margin: 0 0 .25rem; }
  .sub { color: #666; font-size: .9rem; }
  .hint { color: #888; font-size: .8rem; margin-top: .5rem; }
  ul { list-style: none; margin: 0; padding-left: 1.25rem; }
  ul[role=tree] { padding-left: 0; }
  li { margin: 0; }
  .row { display: flex; align-items: center; gap: .35rem; padding: .1rem .35rem;
         border-radius: .3rem; cursor: default; outline: none; }
  .row:hover { background: #00000010; }
  /* focus-within so the row stays highlighted when its link has focus too */
  .row:focus, .row:focus-within { background: #1a73e8; color: #fff; }
  .row:focus .name, .row:focus-within .name { color: #fff; }
  .row:focus .twisty, .row:focus-within .twisty,
  .row:focus .counts, .row:focus-within .counts { color: #d6e4ff; }
  .twisty { width: 1rem; display: inline-block; text-align: center; color: #888;
            cursor: pointer; user-select: none; flex: none; }
  .twisty.leaf { visibility: hidden; }
  .icon { flex: none; }
  .name { text-decoration: none; color: inherit; }
  .name:hover { text-decoration: underline; }
  .owned > .row .name { font-weight: 700; }
  .counts { color: #777; font-size: .8rem; margin-left: .4rem; white-space: nowrap; }
  /* collapsed by default: hide child lists unless the li is expanded */
  li > ul { display: none; }
  li[aria-expanded=true] > ul { display: block; }
</style>
</head>
<body>
<header>
  <h1>Files &amp; folders owned by {{.Account}}{{if .DisplayName}} ({{.DisplayName}}){{end}}</h1>
  <div class="sub">📁 {{.TotalFolders}} folders &nbsp; 📄 {{.TotalFiles}} files owned. Bold = owned by this account.</div>
  <div class="hint">Click a ▶ to expand. Keyboard: ↑/↓ move, →/← expand/collapse, Enter or Space toggles.</div>
  <div class="hint"><a href="https://drive.google.com/drive/search?q=owner:me%20parent:{{.CrawlRoot}}%20-type:folders" target="_blank" rel="noopener">View my files in Drive</a></div>
</header>

<ul role="tree" aria-label="Owned files">
  {{range .Roots}}{{template "node" .}}{{end}}
</ul>

{{define "node"}}
<li role="treeitem"{{if .Owned}} class="owned"{{end}}{{if .Children}} aria-expanded="false"{{end}}>
  <div class="row" tabindex="-1">
    {{if .Children}}<span class="twisty" aria-hidden="true">▶</span>{{else}}<span class="twisty leaf" aria-hidden="true">▶</span>{{end}}
    <span class="icon">{{if isFolder .}}📁{{else}}📄{{end}}</span>
    <a class="name" href="{{driveURL .}}" target="_blank" rel="noopener">{{.Name}}</a>
    {{if isFolder .}}<span class="counts">📁 {{.OwnedFolders}} &nbsp; 📄 {{.OwnedFiles}}</span>{{end}}
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
    var twisty = e.target.closest('.twisty');
    if (twisty && !twisty.classList.contains('leaf')) {
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
        else window.open(row.querySelector('.name').href, '_blank');
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

  // Make the first root focusable to start.
  var first = tree.querySelector('.row');
  if (first) first.tabIndex = 0;
})();
</script>
</body>
</html>
`
