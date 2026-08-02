// Client for the Go review API (drive-cleanup review), reached via the
// same-origin /api/* rewrite in next.config.mjs.

export type Decision = '' | 'keep' | 'delete';

export type Counts = { keep: number; delete: number; undecided: number };

export type TreeFolder = {
  driveId: string;
  name: string;
  decision: Decision;
  /** Original owner (before any ownership transfer); '' when unrecorded. */
  owner: string;
  ownerEmail: string;
  /** Direct (non-folder) children by decision. */
  files: Counts;
  /** The folder itself plus every descendant, by decision. */
  subtree: Counts;
  folders: TreeFolder[];
};

export type TreeResponse = {
  roots: TreeFolder[];
  undoLabel: string;
  fileTotals: Counts;
  folderTotals: Counts;
};

export type FileItem = {
  driveId: string;
  name: string;
  type: string;
  decision: Decision;
  /** Original owner (before any ownership transfer); '' when unrecorded. */
  owner: string;
  ownerEmail: string;
  /** Recorded last-content-change time (RFC3339); '' when unrecorded. */
  lastModified: string;
};

export type MarkResult = {
  needsConfirm: boolean;
  conflictKeeps: number;
  conflictDeletes: number;
  changed: number;
  clearedAncestors: number;
};

export type UndoResult = { undone: string; changed: number };

async function request<T>(url: string, init?: RequestInit): Promise<T> {
  const res = await fetch(url, init);
  if (!res.ok) {
    throw new Error(await res.text());
  }
  return res.json();
}

export function getTree(): Promise<TreeResponse> {
  return request('/api/tree');
}

export function getFiles(folder: string): Promise<FileItem[]> {
  return request(`/api/files?folder=${encodeURIComponent(folder)}`);
}

export function mark(req: {
  driveId: string;
  decision: Decision;
  onConflict?: 'overwrite' | 'preserve';
}): Promise<MarkResult> {
  return request('/api/mark', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
  });
}

export function markMany(req: { driveIds: string[]; decision: Decision }): Promise<MarkResult> {
  return request('/api/mark-many', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
  });
}

export function undo(): Promise<UndoResult> {
  return request('/api/undo', { method: 'POST' });
}

/** Display status for a folder, derived from its subtree tally (self included). */
export function folderStatus(c: Counts): string {
  if (c.keep > 0 && c.delete > 0) return 'mixed';
  if (c.delete > 0 && c.undecided === 0) return 'delete';
  if (c.keep > 0 && c.undecided === 0) return 'keep';
  if (c.delete > 0) return 'partial-delete';
  if (c.keep > 0) return 'partial-keep';
  return 'todo';
}

export function fileStatus(decision: Decision): string {
  return decision === '' ? 'todo' : decision;
}

/**
 * Owner label for a node: the display name, falling back to the email, then to
 * a dash when the snapshot recorded neither.
 */
export function ownerLabel(owner: string, ownerEmail: string): string {
  return owner || ownerEmail || '—';
}

/** Recorded modification time as YYYY-MM-DD; '' when missing or unparseable. */
export function modDate(lastModified: string): string {
  if (!lastModified) return '';
  const t = new Date(lastModified);
  if (Number.isNaN(t.getTime())) return '';
  return t.toISOString().slice(0, 10);
}

export function driveUrl(driveId: string, isFolder: boolean): string {
  return isFolder
    ? `https://drive.google.com/drive/folders/${driveId}`
    : `https://drive.google.com/open?id=${driveId}`;
}
