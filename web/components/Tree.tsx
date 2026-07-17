// The folder tree pane. Only expanded branches render, so the DOM stays small
// even with thousands of folders.
import { Decision, TreeFolder, folderStatus } from '@/lib/api';
import { ExtLink, MarkButtons } from './RowBits';

export type TreeProps = {
  roots: TreeFolder[];
  expanded: Set<string>;
  selectedId: string | null;
  focusId: string | null;
  onToggle: (id: string) => void;
  onSelect: (id: string) => void;
  onMark: (id: string, d: Decision) => void;
};

export function Tree(props: TreeProps) {
  return (
    <div role="tree" aria-label="Folders">
      {props.roots.map((f) => (
        <TreeRow key={f.driveId} folder={f} depth={0} {...props} />
      ))}
    </div>
  );
}

function TreeRow({ folder, depth, ...props }: TreeProps & { folder: TreeFolder; depth: number }) {
  const { driveId, name, decision, folders, subtree } = folder;
  const open = props.expanded.has(driveId);
  const hasKids = folders.length > 0;
  const desc = {
    keep: subtree.keep - (decision === 'keep' ? 1 : 0),
    del: subtree.delete - (decision === 'delete' ? 1 : 0),
    undecided: subtree.undecided - (decision === '' ? 1 : 0),
  };
  return (
    <div role="none">
      <div
        role="treeitem"
        aria-expanded={hasKids ? open : undefined}
        aria-selected={props.selectedId === driveId}
        data-row-id={driveId}
        className={[
          'row',
          `st-${folderStatus(subtree)}`,
          props.focusId === driveId ? 'focused' : '',
          props.selectedId === driveId ? 'selected' : '',
        ].join(' ')}
        style={{ paddingLeft: `${0.35 + depth * 1.05}rem` }}
        onClick={() => props.onSelect(driveId)}
      >
        <span
          className="twisty"
          onClick={(e) => {
            e.stopPropagation();
            if (hasKids) props.onToggle(driveId);
          }}
        >
          {hasKids ? (open ? '▼' : '▶') : ''}
        </span>
        <span className="icon">📁</span>
        <span className="name" title={name}>
          {name}
        </span>
        {decision !== '' && (
          <span className={`chip chip-${decision}`}>{decision === 'keep' ? 'K' : 'D'}</span>
        )}
        <span className="counts" title="Contents: keep / delete / undecided">
          {desc.keep > 0 && <span className="ck">✓{desc.keep} </span>}
          {desc.del > 0 && <span className="cd">✕{desc.del} </span>}
          {desc.undecided > 0 && <span>?{desc.undecided}</span>}
        </span>
        <MarkButtons decision={decision} onMark={(d) => props.onMark(driveId, d)} />
        <ExtLink driveId={driveId} isFolder />
      </div>
      {open &&
        folders.map((c) => (
          <TreeRow key={c.driveId} folder={c} depth={depth + 1} {...props} />
        ))}
    </div>
  );
}
