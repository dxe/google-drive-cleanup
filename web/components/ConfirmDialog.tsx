// Modal used for the two conflict prompts (keep-over-delete, delete-over-keep).
import { useEffect } from 'react';

export type ConfirmOption = {
  label: string;
  value: string;
  kind?: 'primary' | 'danger';
};

export type ConfirmState = {
  message: string;
  options: ConfirmOption[];
  resolve: (value: string | null) => void;
};

export function ConfirmDialog({ state }: { state: ConfirmState }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        state.resolve(null);
      }
    };
    document.addEventListener('keydown', onKey, true);
    return () => document.removeEventListener('keydown', onKey, true);
  }, [state]);

  return (
    <div className="overlay" onClick={() => state.resolve(null)}>
      <div className="dialog" role="alertdialog" aria-modal="true" onClick={(e) => e.stopPropagation()}>
        <p>{state.message}</p>
        <div className="btns">
          <button onClick={() => state.resolve(null)}>Cancel</button>
          {state.options.map((o) => (
            <button key={o.value} className={o.kind ?? ''} onClick={() => state.resolve(o.value)}>
              {o.label}
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}
