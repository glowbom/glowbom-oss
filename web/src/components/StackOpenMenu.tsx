import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { withServerAuthHeaders } from '../lib/server-auth';
import type { PreviewTarget } from '../lib/preview';
import { stackMenuPosition } from '../lib/stack-menu-position';

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
  const [usePopover, setUsePopover] = useState(() => typeof HTMLElement !== 'undefined' && typeof HTMLElement.prototype.showPopover === 'function');
  const panelID = useId();
  const focusFirst = useRef(false);
  const root = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const placeMenu = () => {
    const button = trigger.current;
    const menu = panel.current;
    if (!button || !menu) return;
    const rect = button.getBoundingClientRect();
    const visual = window.visualViewport;
    const viewport = { width: visual?.width || window.innerWidth, height: visual?.height || window.innerHeight, left: visual?.offsetLeft || 0, top: visual?.offsetTop || 0 };
    if (rect.bottom <= viewport.top || rect.top >= viewport.top + viewport.height || rect.right <= viewport.left || rect.left >= viewport.left + viewport.width) { setOpen(false); return; }
    const position = stackMenuPosition(rect, viewport, menu.scrollHeight);
    Object.assign(menu.style, { left: `${position.left}px`, top: `${position.top}px`, width: `${position.width}px`, maxHeight: `${position.maxHeight}px` });
  };
  const contains = (node: Node | null) => !!node && (!!root.current?.contains(node) || !!panel.current?.contains(node));
  const close = () => { setOpen(false); trigger.current?.focus(); };
  useEffect(() => { setOpen(false); setTools(null); setError(''); }, [target?.target, projectPath]);
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
    const outside = (event: PointerEvent) => { if (!contains(event.target as Node)) setOpen(false); };
    document.addEventListener('pointerdown', outside);
    return () => { abort.abort(); document.removeEventListener('pointerdown', outside); };
  }, [open, target?.target, projectPath, projectRoot]);
  useLayoutEffect(() => {
    const menu = panel.current;
    if (!open || !menu || !usePopover) return;
    try { menu.showPopover(); }
    catch { setUsePopover(false); return; }
    return () => { if (menu.isConnected && menu.matches(':popover-open')) menu.hidePopover(); };
  }, [open, usePopover]);
  useLayoutEffect(() => {
    if (!open) return;
    placeMenu();
    if (focusFirst.current) {
      const first = panel.current?.querySelector<HTMLElement>('button:not(:disabled), a[href]');
      if (first) { first.focus(); focusFirst.current = false; }
    }
    const scrolled = (event: Event) => { if (!(event.target instanceof Node) || !panel.current?.contains(event.target)) placeMenu(); };
    window.addEventListener('resize', placeMenu);
    window.addEventListener('scroll', scrolled, true);
    window.visualViewport?.addEventListener('resize', placeMenu);
    window.visualViewport?.addEventListener('scroll', placeMenu);
    return () => {
      window.removeEventListener('resize', placeMenu);
      window.removeEventListener('scroll', scrolled, true);
      window.visualViewport?.removeEventListener('resize', placeMenu);
      window.visualViewport?.removeEventListener('scroll', placeMenu);
    };
  }, [open, usePopover, tools, error]);
  const shownPath = tools?.path || (projectRoot ? projectPath : target?.directory?.startsWith('/') ? target.directory : '');
  const canOpenFolder = !!(tools && (projectRoot ? tools.canOpenFolder : target?.canOpenFolder || tools.canOpenFolder));
  const canOpenTerminal = !!(tools && (projectRoot ? tools.canOpenTerminal : target?.canOpenTerminal || tools.canOpenTerminal));
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
  const options = open ? <div className="stack-open-panel" id={panelID} ref={panel} popover={usePopover ? 'manual' : undefined} role="region" aria-label={`Open ${name} in`} onKeyDown={event => {
    if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
    const items = [...event.currentTarget.querySelectorAll<HTMLElement>('button:not(:disabled), a[href]')];
    if (!items.length) return;
    event.preventDefault();
    const current = items.indexOf(document.activeElement as HTMLElement);
    const next = event.key === 'Home' ? 0 : event.key === 'End' ? items.length - 1 : (current + (event.key === 'ArrowUp' ? -1 : 1) + items.length) % items.length;
    items[next]?.focus();
  }}>
      <strong>{name}</strong><span className="meta stack-open-path">{shownPath || (target?.directory ? `${target.directory}/` : '')}</span>
      {!projectRoot && (target?.url ? <a href={target.url} target="_blank" rel="noopener noreferrer" onClick={close}>Open in Browser ↗</a> : <button type="button" disabled title="Start preview first">Open in Browser ↗</button>)}
      <button type="button" disabled={!canOpenTerminal || busy} onClick={() => { if (projectRoot) void launch('terminal'); else { close(); onTerminal?.(); } }}>Open in Terminal</button>
      <button type="button" disabled={!canOpenFolder || busy} onClick={() => void launch('folder')}>{tools?.fileManagerLabel || 'Open in File Manager'}</button>
      {!tools && !error ? <p className="meta" role="status">Checking installed tools…</p> : null}
      {tools?.editors.length ? <><hr/><span className="meta">Open with</span>{tools.editors.map((editor) => <button key={editor.id} type="button" disabled={busy} onClick={() => void launch(editor.id)}>{editor.name}</button>)}</> : null}
      <hr/><button type="button" disabled={!shownPath || busy} onClick={async () => { if (!shownPath) return; try { await navigator.clipboard.writeText(shownPath); setCopied(true); } catch { setError('Could not copy the folder path.'); } }}>{copied ? 'Copied!' : 'Copy folder path'}</button>
      {error ? <p className="error-inline" role="alert">{error}</p> : null}
    </div> : null;
  return <div className="stack-open" ref={root} onKeyDown={(event) => {
    if (event.key === 'Escape' && open) { event.preventDefault(); event.stopPropagation(); close(); }
    if (event.target === trigger.current && event.key === 'ArrowDown') {
      event.preventDefault();
      const first = open ? panel.current?.querySelector<HTMLElement>('button:not(:disabled), a[href]') : null;
      if (first) first.focus();
      else { focusFirst.current = true; setOpen(true); }
    }
  }} onBlur={(event) => { if (!contains(event.relatedTarget as Node)) setOpen(false); }}>
    <button ref={trigger} className="button secondary tiny stack-folder-button" type="button" disabled={disabled || (!target && !projectRoot)} aria-expanded={open} aria-controls={open ? panelID : undefined} aria-label={`Open ${name} in…`} title={`Open ${name} in…`} onClick={() => { setError(''); focusFirst.current = false; setOpen((value) => !value); }}>
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M3 7V5a1 1 0 0 1 1-1h5l2 3h9a1 1 0 0 1 1 1v11a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V7Z"/><path d="M3 8h18"/></svg><span aria-hidden="true">⌄</span>
    </button>
    {options && (usePopover ? options : createPortal(options, root.current?.closest('dialog') || document.body))}
  </div>;
}
