import { useEffect, useRef, useState } from 'react';
import { withServerAuthHeaders } from '../lib/server-auth';
import type { PreviewTarget } from '../lib/preview';

type Tools = { editors: { id: string; name: string }[]; path: string; fileManagerLabel: string; canOpenTerminal?: boolean; canOpenFolder?: boolean };
export function StackOpenMenu({ projectPath, target, disabled, onTerminal, projectName }: {
  projectPath: string; target?: PreviewTarget; disabled: boolean; onTerminal?: () => void; projectName?: string;
}) {
  const projectRoot = projectName !== undefined;
  const name = projectName || target?.name || 'stack';
  const [open, setOpen] = useState(false);
  const [tools, setTools] = useState<Tools | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const close = () => { setOpen(false); trigger.current?.focus(); };
  useEffect(() => { setOpen(false); setTools(null); }, [target?.target, projectPath]);
  useEffect(() => {
    if (!open || (!target && !projectRoot)) return;
    const abort = new AbortController();
    setTools(null); setError(''); setCopied(false);
    void fetch('/api/preview', { method: 'POST', headers: withServerAuthHeaders({ 'Content-Type': 'application/json' }), signal: abort.signal,
      body: JSON.stringify({ path: projectPath, target: target?.target, projectRoot, action: 'tools' }) }).then(async (response) => {
      if (!response.ok) throw new Error('Could not load local tools. Restart Glowbom OSS if you just updated it.');
      const data = await response.json() as Tools;
      if (!abort.signal.aborted) setTools(data);
    }).catch((err: unknown) => { if (!abort.signal.aborted) setError(err instanceof Error ? err.message : 'Could not load tools.'); });
    const outside = (event: PointerEvent) => { if (!root.current?.contains(event.target as Node)) setOpen(false); };
    document.addEventListener('pointerdown', outside);
    return () => { abort.abort(); document.removeEventListener('pointerdown', outside); };
  }, [open, target?.target, projectPath, projectRoot]);
  const launch = async (editor: string) => {
    if (!target && !projectRoot) return;
    setBusy(true); setError('');
    try {
      const response = await fetch('/api/preview', { method: 'POST', headers: withServerAuthHeaders({ 'Content-Type': 'application/json' }),
        body: JSON.stringify({ path: projectPath, target: target?.target, projectRoot, action: editor === 'terminal' ? 'terminal' : 'open', editor }) });
      if (!response.ok) throw new Error((await response.text()).trim());
      close();
    } catch (err) { setError(err instanceof Error ? err.message : 'Could not open application.'); }
    finally { setBusy(false); }
  };
  return <div className="stack-open" ref={root} onKeyDown={(event) => { if (event.key === 'Escape') { event.preventDefault(); close(); } }} onBlur={(event) => { if (!event.currentTarget.contains(event.relatedTarget as Node)) setOpen(false); }}>
    <button ref={trigger} className="button secondary tiny stack-folder-button" type="button" disabled={disabled || (!target && !projectRoot)} aria-expanded={open} aria-label={`Open ${name} in…`} title={`Open ${name} in…`} onClick={() => setOpen((value) => !value)}>
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M3 7V5a1 1 0 0 1 1-1h5l2 3h9a1 1 0 0 1 1 1v11a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V7Z"/><path d="M3 8h18"/></svg><span aria-hidden="true">⌄</span>
    </button>
    {open ? <div className="stack-open-panel" role="region" aria-label={`Open ${name} in`}>
      <strong>{name}</strong><span className="meta stack-open-path">{projectRoot ? projectPath : target?.directory + '/'}</span>
      {!projectRoot && (target?.url ? <a href={target.url} target="_blank" rel="noopener noreferrer" onClick={close}>Open in Browser ↗</a> : <button type="button" disabled title="Start preview first">Open in Browser ↗</button>)}
      <button type="button" disabled={!(projectRoot ? tools?.canOpenTerminal : target?.canOpenTerminal) || busy} onClick={() => { if (projectRoot) void launch('terminal'); else { close(); onTerminal?.(); } }}>Open in Terminal</button>
      <button type="button" disabled={!tools || !(projectRoot ? tools?.canOpenFolder : target?.canOpenFolder) || busy} onClick={() => void launch('folder')}>{tools?.fileManagerLabel || 'Open in File Manager'}</button>
      {!tools && !error ? <p className="meta" role="status">Checking installed tools…</p> : null}
      {tools?.editors.length ? <><hr/><span className="meta">Open with</span>{tools.editors.map((editor) => <button key={editor.id} type="button" disabled={busy} onClick={() => void launch(editor.id)}>{editor.name}</button>)}</> : null}
      <hr/><button type="button" disabled={!tools || busy} onClick={async () => { if (!tools) return; try { await navigator.clipboard.writeText(tools.path); setCopied(true); } catch { setError('Could not copy the folder path.'); } }}>{copied ? 'Copied!' : 'Copy folder path'}</button>
      {error ? <p className="error-inline" role="alert">{error}</p> : null}
    </div> : null}
  </div>;
}
