'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  Decision,
  FileItem,
  TreeFolder,
  TreeResponse,
  getFiles,
  getTree,
  mark,
  markMany,
  undo,
} from '@/lib/api';
import { Tree } from '@/components/Tree';
import { FilePane } from '@/components/FilePane';
import { ConfirmDialog, ConfirmState } from '@/components/ConfirmDialog';

type Pane = 'tree' | 'files';
type Focus = { pane: Pane; id: string };

export default function Page() {
  const [tree, setTree] = useState<TreeResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [selected, setSelected] = useState<string | null>(null);
  const [files, setFiles] = useState<FileItem[] | null>(null);
  const [focus, setFocus] = useState<Focus | null>(null);
  const [confirm, setConfirm] = useState<ConfirmState | null>(null);
  const [toast, setToast] = useState<string | null>(null);

  // ---- indices over the tree ----
  const index = useMemo(() => {
    const byId = new Map<string, { folder: TreeFolder; parent: string | null }>();
    const walk = (f: TreeFolder, parent: string | null) => {
      byId.set(f.driveId, { folder: f, parent });
      for (const c of f.folders) walk(c, f.driveId);
    };
    for (const r of tree?.roots ?? []) walk(r, null);
    return byId;
  }, [tree]);

  // Visible tree rows in visual order, for keyboard navigation.
  const visibleTreeIds = useMemo(() => {
    const out: string[] = [];
    const walk = (f: TreeFolder) => {
      out.push(f.driveId);
      if (expanded.has(f.driveId)) for (const c of f.folders) walk(c);
    };
    for (const r of tree?.roots ?? []) walk(r);
    return out;
  }, [tree, expanded]);

  const selectedFolder = selected ? (index.get(selected)?.folder ?? null) : null;

  const selectedPath = useMemo(() => {
    if (!selected || !index.has(selected)) return null;
    const path: string[] = [];
    for (let id: string | null = selected; id; id = index.get(id)?.parent ?? null) {
      path.unshift(index.get(id)!.folder.name);
    }
    return path;
  }, [selected, index]);

  // ---- data loading ----
  // Every file fetch takes a ticket; only the newest one may write to `files`.
  // Without this, a slow refresh triggered by marking folder A can land after
  // the user has already moved to folder B and paint A's files under B's name.
  const filesSeq = useRef(0);
  const selectedRef = useRef<string | null>(null);
  selectedRef.current = selected;

  const refresh = useCallback(async () => {
    const folderForFiles = selectedRef.current;
    const seq = ++filesSeq.current;
    try {
      const [t, f] = await Promise.all([
        getTree(),
        folderForFiles ? getFiles(folderForFiles) : Promise.resolve(null),
      ]);
      setTree(t);
      if (seq === filesSeq.current) setFiles(f);
      setError(null);
    } catch (e) {
      setError(String(e));
    }
  }, []);

  useEffect(() => {
    getTree().then(
      (t) => {
        setTree(t);
        if (t.roots.length > 0) {
          const rootId = t.roots[0].driveId;
          setExpanded(new Set([rootId]));
          setFocus({ pane: 'tree', id: rootId });
        }
      },
      (e) => setError(String(e)),
    );
  }, []);

  const selectFolder = useCallback(
    (id: string) => {
      setSelected(id);
      selectedRef.current = id;
      setFocus({ pane: 'tree', id });
      setFiles(null);
      const seq = ++filesSeq.current;
      getFiles(id).then(
        (f) => {
          if (seq === filesSeq.current) setFiles(f);
        },
        (e) => setError(String(e)),
      );
    },
    [],
  );

  // ---- toast ----
  const toastTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const showToast = useCallback((msg: string) => {
    setToast(msg);
    if (toastTimer.current) clearTimeout(toastTimer.current);
    toastTimer.current = setTimeout(() => setToast(null), 4000);
  }, []);

  // ---- marking ----
  const askConfirm = useCallback(
    (message: string, options: ConfirmState['options']) =>
      new Promise<string | null>((resolve) => {
        setConfirm({
          message,
          options,
          resolve: (v) => {
            setConfirm(null);
            resolve(v);
          },
        });
      }),
    [],
  );

  const doMark = useCallback(
    async (id: string, decision: Decision) => {
      try {
        let res = await mark({ driveId: id, decision });
        if (res.needsConfirm) {
          let onConflict: string | null;
          if (decision === 'delete') {
            onConflict = await askConfirm(
              `${res.conflictKeeps} item(s) under this folder are marked Keep. ` +
                'Deleting the folder overwrites them to Delete.',
              [{ label: 'Overwrite to Delete', value: 'overwrite', kind: 'danger' }],
            );
          } else {
            onConflict = await askConfirm(
              `${res.conflictDeletes} item(s) under this folder are marked Delete. ` +
                'Keep them as Delete, or overwrite everything to Keep?',
              [
                { label: 'Keep them as Delete', value: 'preserve', kind: 'primary' },
                { label: 'Overwrite all to Keep', value: 'overwrite' },
              ],
            );
          }
          if (!onConflict) return;
          res = await mark({ driveId: id, decision, onConflict: onConflict as 'overwrite' | 'preserve' });
        }
        if (res.clearedAncestors > 0) {
          showToast(
            `${res.clearedAncestors} folder(s) above were un-marked from Delete and re-decided from their contents.`,
          );
        }
        await refresh();
      } catch (e) {
        setError(String(e));
      }
    },
    [askConfirm, refresh, showToast],
  );

  const doBulk = useCallback(
    async (decision: Decision) => {
      if (!files || files.length === 0) return;
      try {
        const res = await markMany({ driveIds: files.map((f) => f.driveId), decision });
        if (res.clearedAncestors > 0) {
          showToast(
            `${res.clearedAncestors} folder(s) above were un-marked from Delete and re-decided from their contents.`,
          );
        }
        await refresh();
      } catch (e) {
        setError(String(e));
      }
    },
    [files, refresh, showToast],
  );

  const doUndo = useCallback(async () => {
    try {
      const res = await undo();
      showToast(`Undid: ${res.undone} (${res.changed} item(s) restored)`);
      await refresh();
    } catch (e) {
      setError(String(e));
    }
  }, [refresh, showToast]);

  // ---- keyboard ----
  // The handler reads the latest state through a ref so one document-level
  // listener survives re-renders.
  const snap = useRef({ tree, expanded, selected, files, focus, confirm, visibleTreeIds });
  snap.current = { tree, expanded, selected, files, focus, confirm, visibleTreeIds };

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const s = snap.current;
      if (s.confirm) return; // dialog owns the keyboard
      const t = e.target as HTMLElement;
      if (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA') return;

      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'z') {
        e.preventDefault();
        doUndo();
        return;
      }
      if (e.metaKey || e.ctrlKey || e.altKey) return;

      const pane: Pane = s.focus?.pane ?? 'tree';
      const rows = pane === 'tree' ? s.visibleTreeIds : (s.files ?? []).map((f) => f.driveId);
      const idx = s.focus ? rows.indexOf(s.focus.id) : -1;
      const focusRow = (i: number) => {
        const id = rows[Math.max(0, Math.min(rows.length - 1, i))];
        if (id) setFocus({ pane, id });
      };
      const markFocused = (d: Decision) => {
        if (!s.focus) return;
        doMark(s.focus.id, d);
        // Advance to the next visible row so rapid triage flows downward.
        if (idx >= 0 && idx < rows.length - 1) focusRow(idx + 1);
      };

      switch (e.key) {
        case 'Tab': {
          const next: Pane = pane === 'tree' ? 'files' : 'tree';
          const nextRows = next === 'tree' ? s.visibleTreeIds : (s.files ?? []).map((f) => f.driveId);
          if (nextRows.length > 0) setFocus({ pane: next, id: nextRows[0] });
          break;
        }
        case 'ArrowDown':
          focusRow(idx < 0 ? 0 : idx + 1);
          break;
        case 'ArrowUp':
          focusRow(idx < 0 ? 0 : idx - 1);
          break;
        case 'ArrowRight': {
          if (pane !== 'tree' || !s.focus) return;
          const node = s.tree && findFolder(s.tree.roots, s.focus.id);
          if (node && node.folders.length > 0) {
            if (!s.expanded.has(s.focus.id)) {
              setExpanded((prev) => new Set(prev).add(s.focus!.id));
            } else {
              setFocus({ pane, id: node.folders[0].driveId });
            }
          }
          break;
        }
        case 'ArrowLeft': {
          if (pane !== 'tree' || !s.focus) return;
          if (s.expanded.has(s.focus.id)) {
            setExpanded((prev) => {
              const n = new Set(prev);
              n.delete(s.focus!.id);
              return n;
            });
          } else {
            // jump to parent
            const parent = parentOf(s.tree, s.focus.id);
            if (parent) setFocus({ pane, id: parent });
          }
          break;
        }
        case 'Enter':
          if (pane === 'tree' && s.focus) selectFolder(s.focus.id);
          break;
        case 'k':
          markFocused('keep');
          break;
        case 'd':
          markFocused('delete');
          break;
        case 'u':
        case 'Backspace':
          markFocused('');
          break;
        default:
          return;
      }
      e.preventDefault();
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [doMark, doUndo, selectFolder]);

  // Keep the focused row in view.
  useEffect(() => {
    if (!focus) return;
    const el = document.querySelector(`[data-row-id="${CSS.escape(focus.id)}"]`);
    el?.scrollIntoView({ block: 'nearest' });
  }, [focus]);

  if (error && !tree) {
    return (
      <div className="loading">
        Could not reach the review API: {error}
        <br />
        Start it with: <code>drive-cleanup review</code>
      </div>
    );
  }
  if (!tree) return <div className="loading">Loading tree…</div>;

  const ft = tree.fileTotals;
  const dt = tree.folderTotals;
  return (
    <div className="app">
      <header className="topbar">
        <h1>Drive cleanup review</h1>
        <span className="totals">
          <span>
            📁 <b className="k">{dt.keep}</b> / <b className="d">{dt.delete}</b> / {dt.undecided} todo
          </span>
          <span>
            📄 <b className="k">{ft.keep}</b> / <b className="d">{ft.delete}</b> / {ft.undecided} todo
          </span>
        </span>
        <span className="spacer" />
        <span className="help">↑↓ move · →← expand · Enter open · k keep · d delete · u clear · Tab pane · ⌘Z undo</span>
        <button className="undo-btn" disabled={!tree.undoLabel} onClick={doUndo} title="Undo the last action (⌘Z)">
          ↩ Undo{tree.undoLabel ? `: ${tree.undoLabel}` : ''}
        </button>
      </header>
      <div className="legendbar" aria-label="Folder colour legend">
        <span><i className="sw st-keep" />keep</span>
        <span><i className="sw st-partial-keep" />partly kept, rest undecided</span>
        <span><i className="sw st-delete" />delete</span>
        <span><i className="sw st-partial-delete" />partly deleted, rest undecided</span>
        <span><i className="sw st-mixed" />mixed keep &amp; delete, all decided</span>
        <span><i className="sw st-partial-mixed" />mixed keep &amp; delete, some undecided</span>
        <span><i className="sw st-todo" />undecided</span>
      </div>
      <main className="main">
        <section className={`pane tree-pane ${focus?.pane === 'files' ? 'pane-inactive' : ''}`}>
          <Tree
            roots={tree.roots}
            expanded={expanded}
            selectedId={selected}
            focusId={focus?.pane === 'tree' ? focus.id : null}
            onToggle={(id) =>
              setExpanded((prev) => {
                const n = new Set(prev);
                if (n.has(id)) n.delete(id);
                else n.add(id);
                return n;
              })
            }
            onSelect={selectFolder}
            onMark={doMark}
          />
        </section>
        <section className={`pane file-pane ${focus?.pane === 'tree' ? 'pane-inactive' : ''}`}>
          <FilePane
            path={selectedPath}
            owner={selectedFolder?.owner ?? ''}
            ownerEmail={selectedFolder?.ownerEmail ?? ''}
            files={files}
            focusId={focus?.pane === 'files' ? focus.id : null}
            onMark={doMark}
            onBulk={doBulk}
            onFocus={(id) => setFocus({ pane: 'files', id })}
          />
        </section>
      </main>
      {confirm && <ConfirmDialog state={confirm} />}
      {toast && <div className="toast">{toast}</div>}
      {error && tree && <div className="toast">⚠ {error}</div>}
    </div>
  );
}

function findFolder(roots: TreeFolder[], id: string): TreeFolder | null {
  for (const r of roots) {
    if (r.driveId === id) return r;
    const hit = findFolder(r.folders, id);
    if (hit) return hit;
  }
  return null;
}

function parentOf(tree: TreeResponse | null, id: string): string | null {
  if (!tree) return null;
  let found: string | null = null;
  const walk = (f: TreeFolder, parent: string | null): boolean => {
    if (f.driveId === id) {
      found = parent;
      return true;
    }
    return f.folders.some((c) => walk(c, f.driveId));
  };
  tree.roots.some((r) => walk(r, null));
  return found;
}
