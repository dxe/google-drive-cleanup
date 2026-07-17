// Small shared row pieces: the mark buttons and the deliberately-subdued
// open-in-Drive link.
import { Decision, driveUrl } from '@/lib/api';

export function MarkButtons({
  onMark,
  decision,
}: {
  onMark: (d: Decision) => void;
  decision: Decision;
}) {
  const btn = (d: Decision, cls: string, label: string, title: string) => (
    <button
      className={cls}
      title={title}
      aria-pressed={decision === d}
      onClick={(e) => {
        e.stopPropagation();
        onMark(d);
      }}
    >
      {label}
    </button>
  );
  return (
    <span className="actions">
      {btn('keep', 'bk', '✓', 'Keep (k)')}
      {btn('delete', 'bd', '✕', 'Delete (d)')}
      {btn('', 'bc', '–', 'Clear decision (u)')}
    </span>
  );
}

export function ExtLink({ driveId, isFolder }: { driveId: string; isFolder: boolean }) {
  return (
    <a
      className="ext"
      href={driveUrl(driveId, isFolder)}
      target="_blank"
      rel="noopener noreferrer"
      title="Open in Google Drive"
      onClick={(e) => e.stopPropagation()}
    >
      <svg
        xmlns="http://www.w3.org/2000/svg"
        width="12"
        height="12"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="2.5"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
        <polyline points="15 3 21 3 21 9" />
        <line x1="10" y1="14" x2="21" y2="3" />
      </svg>
    </a>
  );
}
