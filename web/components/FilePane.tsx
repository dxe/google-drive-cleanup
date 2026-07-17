// The right-hand pane: the selected folder's direct files with per-file mark
// buttons, decision tallies, and bulk actions.
import { Decision, FileItem, fileStatus } from '@/lib/api';
import { ExtLink, MarkButtons } from './RowBits';

export type FilePaneProps = {
  path: string[] | null; // breadcrumb of the selected folder, null = nothing selected
  files: FileItem[] | null;
  focusId: string | null;
  onMark: (id: string, d: Decision) => void;
  onBulk: (d: Decision) => void;
  onFocus: (id: string) => void;
};

const typeIcon: Record<string, string> = {
  shortcut: '↪️',
  google_doc: '📄',
  binary: '📦',
};

export function FilePane({ path, files, focusId, onMark, onBulk, onFocus }: FilePaneProps) {
  if (!path) {
    return <div className="file-empty">Select a folder to see its files.</div>;
  }
  const counts = { keep: 0, delete: 0, undecided: 0 };
  for (const f of files ?? []) {
    if (f.decision === 'keep') counts.keep++;
    else if (f.decision === 'delete') counts.delete++;
    else counts.undecided++;
  }
  return (
    <>
      <div className="file-head">
        <div className="crumbs" title={path.join(' / ')}>
          {path.slice(0, -1).join(' / ')}
          {path.length > 1 ? ' / ' : ''}
          <b>{path[path.length - 1]}</b>
        </div>
        <div className="file-stats">
          <span className="stat k">✓ {counts.keep} keep</span>
          <span className="stat d">✕ {counts.delete} delete</span>
          <span className="stat u">? {counts.undecided} undecided</span>
          {(files?.length ?? 0) > 0 && (
            <span className="bulk">
              <button className="bk" onClick={() => onBulk('keep')} title="Mark every file in this folder Keep">
                All files ✓
              </button>
              <button className="bd" onClick={() => onBulk('delete')} title="Mark every file in this folder Delete">
                All files ✕
              </button>
              <button onClick={() => onBulk('')} title="Clear every file's decision">
                Clear
              </button>
            </span>
          )}
        </div>
      </div>
      <div className="file-list">
        {files === null ? (
          <div className="loading">Loading…</div>
        ) : files.length === 0 ? (
          <div className="file-empty">No files directly in this folder.</div>
        ) : (
          files.map((f) => (
            <div
              key={f.driveId}
              data-row-id={f.driveId}
              className={['row', `st-${fileStatus(f.decision)}`, focusId === f.driveId ? 'focused' : ''].join(' ')}
              onClick={() => onFocus(f.driveId)}
            >
              <span className="twisty" />
              <span className="icon">{typeIcon[f.type] ?? '📄'}</span>
              <span className="name" title={f.name}>
                {f.name}
              </span>
              {f.decision !== '' && (
                <span className={`chip chip-${f.decision}`}>{f.decision === 'keep' ? 'K' : 'D'}</span>
              )}
              <MarkButtons decision={f.decision} onMark={(d) => onMark(f.driveId, d)} />
              <ExtLink driveId={f.driveId} isFolder={false} />
            </div>
          ))
        )}
      </div>
    </>
  );
}
