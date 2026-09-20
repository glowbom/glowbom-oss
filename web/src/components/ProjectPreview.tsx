import { StackOpenMenu } from './StackOpenMenu';
import { useCallback, useEffect, useRef, useState } from 'react';
import { discoverStacks, formatPreviewCommand, parsePreviewCommand, previewRequest, type DiscoveredStack, type PreviewTarget } from '../lib/preview';
import { nextStackDirectory, type StackPreset } from '../lib/stack-presets';

import { ExistingStacks } from './ExistingStacks';
import { StackPresetPicker } from './StackPresetPicker';

const KIND_LABELS: Record<string, string> = { static: 'HTML', next: 'Next.js', vite: 'Vite', custom: 'Custom stack' };

function savedPreviewTarget(projectPath: string): string {
  try { return localStorage.getItem(`glowbom.previewTarget:${projectPath}`) || 'prototype'; }
  catch { return 'prototype'; }
}

export function ProjectPreview({ projectPath, runStatus, hidden, onTargetsChange, onStackAdded }: {
  projectPath: string; runStatus: string; hidden: boolean;
  onTargetsChange: (projectPath: string, targets: PreviewTarget[]) => void;
  onStackAdded: (targetID: string) => void;
}) {
  const [targets, setTargets] = useState<PreviewTarget[]>([]);
  const [selected, setSelected] = useState(() => savedPreviewTarget(projectPath));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [loadError, setLoadError] = useState('');
  const [frameVersion, setFrameVersion] = useState(0);
  const [phone, setPhone] = useState(false);
  const [editing, setEditing] = useState<string | null>(null);
  const [discovered, setDiscovered] = useState<DiscoveredStack[]>([]);
  const [discovering, setDiscovering] = useState(false);
  const [discoveryError, setDiscoveryError] = useState('');
  const [limited, setLimited] = useState(false);
  const [scanVersion, setScanVersion] = useState(0);
  const [showFields, setShowFields] = useState(false);
  const fieldsRef = useRef<HTMLDivElement>(null);
  const [connecting, setConnecting] = useState(false);
  const [advanced, setAdvanced] = useState(false);
  useEffect(() => {
    if (editing !== '') return;
    const abort = new AbortController();
    setDiscovering(true); setDiscoveryError('');
    void discoverStacks(projectPath, abort.signal).then((result) => {
      if (!abort.signal.aborted) { setDiscovered(result.apps); setLimited(result.limited); }
    }).catch((err: unknown) => {
      if (!abort.signal.aborted) setDiscoveryError(err instanceof Error ? err.message : 'Could not detect apps.');
    }).finally(() => { if (!abort.signal.aborted) setDiscovering(false); });
    return () => abort.abort();
  }, [editing, projectPath, scanVersion]);
  const [name, setName] = useState('');
  const [directory, setDirectory] = useState('');
  const [command, setCommand] = useState('');
  const [description, setDescription] = useState('');
  const [preset, setPreset] = useState('');
  useEffect(() => { if (showFields) { fieldsRef.current?.scrollIntoView({ block: 'nearest' }); fieldsRef.current?.querySelector('input')?.focus({ preventScroll: true }); } }, [showFields, preset]);
  const [previewMode, setPreviewMode] = useState<'auto' | 'command' | 'none'>('auto');
  const [previewNotes, setPreviewNotes] = useState('');
  const [savedMessage, setSavedMessage] = useState('');
  const sessions = useRef<PreviewTarget[]>([]);
  const mounted = useRef(true);
  const requestVersion = useRef(0);
  const mutating = useRef(false);
  const current = targets.find((target) => target.target === selected);
  const currentRevision = useRef('');
  const previousRun = useRef(runStatus);

  useEffect(() => {
    if (!targets.some((target) => target.target === selected)) return;
    try { localStorage.setItem(`glowbom.previewTarget:${projectPath}`, selected); }
    catch { /* Preview remains usable when browser storage is unavailable. */ }
  }, [projectPath, selected, targets]);

  const update = useCallback((next: PreviewTarget[]) => {
    sessions.current = next;
    setTargets(next);
    onTargetsChange(projectPath, next);
    setSelected((previous) => next.some((t) => t.target === previous) ? previous : next[0]?.target || 'prototype');
  }, [onTargetsChange, projectPath]);

  useEffect(() => {
    mounted.current = true;
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      const version = requestVersion.current;
      try {
        const next = await previewRequest(projectPath, 'inspect', {}, abort.signal);
        if (mounted.current && !mutating.current && version === requestVersion.current) { update(next); setLoadError(''); }
      } catch (err) {
        if (!abort.signal.aborted && mounted.current) setLoadError(err instanceof Error ? err.message : 'Could not load previews.');
      }
      if (!abort.signal.aborted) timer = setTimeout(() => void poll(), 1500);
    };
    void poll();
    return () => {
      mounted.current = false;
      abort.abort();
      clearTimeout(timer);
      for (const target of sessions.current) {
        if (target.id) void previewRequest(projectPath, 'stop', { target: target.target, id: target.id }).catch(() => {});
      }
    };
  }, [projectPath, update]);

  useEffect(() => {
    const revision = `${selected}:${current?.id || ''}:${current?.revision || ''}`;
    if (currentRevision.current && currentRevision.current !== revision && current?.kind === 'custom') setFrameVersion((v) => v + 1);
    currentRevision.current = revision;
  }, [selected, current?.id, current?.revision, current?.kind]);

  useEffect(() => {
    if (previousRun.current === 'running' && runStatus !== 'running') setFrameVersion((v) => v + 1);
    previousRun.current = runStatus;
  }, [runStatus]);

  const perform = async (action: 'start' | 'stop' | 'remove' | 'save' | 'terminal' | 'folder') => {
    setBusy(true);
    setError('');
    mutating.current = true;
    requestVersion.current += 1;
    try {
      if (action === 'save' || action === 'terminal') await previewRequest(projectPath, 'inspect', { requireStackCatalog: true });
      const options = action === 'save'
        ? { config: { id: editing || '', name: name.trim(), directory: directory.trim(), command: previewMode === 'command' ? parsePreviewCommand(command) : [], description: description.trim(), preset, previewMode, previewNotes: previewNotes.trim() } }
        : { target: selected, install: !!current?.needsInstall, id: current?.id };
      const next = await previewRequest(projectPath, action, options);
      if (!mounted.current) {
        for (const target of next) {
          if (target.id) void previewRequest(projectPath, 'stop', { target: target.target, id: target.id }).catch(() => {});
        }
        return;
      }
      update(next);
      if (action === 'save') {
        const savedID = editing || next[next.length - 1]?.target || 'prototype';
        setSelected(savedID);
        if (!editing) onStackAdded(savedID);
        setEditing(null);
        setSavedMessage(connecting ? 'App connected and selected for the next build. Your preview settings are saved; start preview when you are ready.' : editing ? 'Stack saved. Its description is included on every build that selects it.' : 'Stack saved and selected for the next build. Describe your app above and click Build. Choose additional targets under Project if needed.');
      }
      if (action !== 'folder' && action !== 'terminal') setFrameVersion((v) => v + 1);
    } catch (err) {
      if (mounted.current) setError(err instanceof Error ? err.message : 'Could not update preview.');
    } finally {
      requestVersion.current += 1;
      mutating.current = false;
      if (mounted.current) setBusy(false);
    }
  };

  const edit = (target?: PreviewTarget) => {
    setEditing(target?.target || '');
    setShowFields(!!target); setConnecting(false); setAdvanced(false);
    setName(target?.name || '');
    setDirectory(target?.directory || '');
    setCommand(formatPreviewCommand(target?.command || []));
    setDescription(target?.description || '');
    setPreset(target?.preset || '');
    setPreviewMode(target?.previewMode || (target?.command?.length ? 'command' : 'auto'));
    setPreviewNotes(target?.previewNotes || '');
    setSavedMessage('');
    setError('');
  };
  const choosePreset = (choice?: StackPreset) => {
    if (editing !== '') return;
    setShowFields(true); setConnecting(false); setAdvanced(false);
    setPreset(choice?.id || '');
    setName(choice?.name || '');
    setDescription(choice?.description || '');
    setDirectory(choice ? nextStackDirectory(choice.directory, [...targets.map((t) => t.directory), ...discovered.map((t) => t.directory)]) : '');
    setCommand(formatPreviewCommand(choice?.command || []));
    setPreviewMode(choice?.previewMode || 'auto');
    setPreviewNotes(choice?.previewNotes || '');
  };
  const connectExisting = (app: DiscoveredStack) => {
    const saved = targets.find((target) => target.target === app.target);
    if (app.buildTarget) {
      onStackAdded(app.buildTarget);
      if (saved) setSelected(saved.target);
      setEditing(null);
      setSavedMessage(`${app.name} selected for the next build. ${saved ? 'Its saved settings are unchanged.' : 'Use its native tools to run it.'}`);
      return;
    }
    setShowFields(true); setConnecting(true); setAdvanced(false);
    setName(app.name); setDirectory(app.directory); setPreset(app.preset || '');
    setDescription(`Work on the existing ${app.stack} app in ${app.directory}. Preserve its framework, dependencies, package manager, and existing behavior unless the requested change requires otherwise. Follow its existing project instructions.`);
    setPreviewMode(app.previewMode); setCommand(formatPreviewCommand(app.command || []));
    setPreviewNotes(app.previewMode === 'none' ? 'Use the project README to run this app. Configure a web server command if it provides a browser interface.' : 'Install the tools and dependencies required by this existing app before starting its preview.');
    setError('');
  };
  const active = current?.status === 'running' || current?.status === 'starting' || current?.status === 'installing';

  return (
    <section className="card preview-card" hidden={hidden} aria-label="Project preview">
      <div className="preview-heading">
        <div><h2>Preview</h2><p className="meta">See your project as it changes.</p></div>
        <button className="button secondary tiny" type="button" onClick={() => edit()} disabled={busy || runStatus === 'running'}>+ Add stack</button>
      </div>
      <div className="preview-toolbar">
        <div className="preview-stack-selector">
          <StackOpenMenu projectPath={projectPath} target={current} disabled={busy} onTerminal={() => void perform('terminal')} />
        <div className="preview-tabs" role="group" aria-label="Preview target">
          {targets.map((target) => (
            <button className={`preview-tab ${selected === target.target ? 'selected' : ''}`} type="button" aria-pressed={selected === target.target} key={target.target} onClick={() => setSelected(target.target)} disabled={busy}>
              {target.name}<span>{target.previewMode === 'none' ? 'Native / terminal' : KIND_LABELS[target.kind] || (target.description ? 'Ready to build' : 'Not available')}</span>
            </button>
          ))}
        </div>
        </div>
        <div className="preview-actions">
          {current?.url ? <a className="button secondary tiny" href={current.url} target="_blank" rel="noopener noreferrer">Open in Browser ↗</a> : null}
          {current?.previewMode !== 'none' ? <><button className="button secondary tiny" type="button" disabled={!current?.url} onClick={() => setFrameVersion((v) => v + 1)}>Refresh</button>
          <button className="button secondary tiny" type="button" disabled={!current} aria-pressed={phone} onClick={() => setPhone((v) => !v)}>{phone ? 'Desktop size' : 'Phone size'}</button>
          {active ? <button className="button secondary tiny" disabled={busy} type="button" onClick={() => void perform('stop')}>Stop preview</button> : (
            <button className="button tiny" disabled={busy || !current?.available} type="button" onClick={() => void perform('start')}>{busy ? 'Starting...' : current?.needsInstall ? 'Install & run' : 'Start preview'}</button>
          )}</> : null}
        </div>
      </div>
      {editing !== null ? (
        <form className="preview-config" onSubmit={(event) => { event.preventDefault(); void perform('save'); }}>
          <strong>{editing ? 'Edit stack' : 'Add a stack'}</strong>
          <p className="meta">{editing ? 'Update this stack’s saved instructions and preview settings. To add a different stack, use + Add stack.' : 'Choose a preset or describe your own stack. Edit saved stacks from their preview tiles.'}</p>
          {editing === '' && !showFields ? <>

            <section className="new-stack-section" aria-label="Stack catalog">
              <StackPresetPicker selected={connecting ? '__existing' : showFields ? preset : '__none'} disabled={busy} onChoose={choosePreset} existing={discovered} /></section>
            <ExistingStacks apps={discovered.filter((app) => !app.buildTarget && !app.target && !targets.some((target) => target.directory === app.directory))} loading={discovering} error={discoveryError} limited={limited} disabled={busy || runStatus === 'running'} onRefresh={() => setScanVersion((v) => v + 1)} onAdd={connectExisting} />
          </> : null}
          {showFields ? <div className="stack-fields" ref={fieldsRef}>
          {editing === '' ? <><button className="preview-text-button stack-back" type="button" disabled={busy} onClick={() => setShowFields(false)}>← Back to apps</button><h3>{connecting ? 'Connect existing app' : 'Set up your new app'}</h3></> : null}
          <label>Name<input className="input" required maxLength={80} value={name} onChange={(event) => setName(event.target.value)} placeholder="React experiment" /></label>
          <label>Describe your stack<textarea className="input stack-description" maxLength={8000} value={description} onChange={(event) => setDescription(event.target.value)} placeholder="Use PHP with server-rendered HTML and plain CSS. Match the prototype's design and behavior." /></label>
          <p className="meta">Saved with this project and sent to the agent on every build that includes this stack.</p>
          <label>Project folder<input className="input" required readOnly={connecting} value={directory} onChange={(event) => setDirectory(event.target.value)} placeholder="react, apps/my-app, or . for the project root" /></label>
          <details className="stack-advanced" open={advanced} onToggle={(e) => setAdvanced(e.currentTarget.open)}>
            <summary>Preview settings</summary>
            <label>Preview type<select className="input" value={previewMode} onChange={(e) => setPreviewMode(e.target.value as typeof previewMode)}>
              <option value="auto">Automatic (HTML, Next.js, Vite)</option><option value="command">Saved web server command</option><option value="none">No browser preview (native / terminal)</option>
            </select></label>
            {previewMode === 'command' ? <label>Launch command<input className="input" value={command} onChange={(event) => setCommand(event.target.value)} placeholder="bun run dev --host {host} --port {port}" required /></label> : null}
            <p className="meta">Automatic detects HTML, Next.js, or Vite. A saved web server command must include {'{host}'} and {'{port}'} in its command. The server must use these values. Starting it runs the project's code on this computer.</p>
            <label>Setup and preview notes<textarea className="input" maxLength={2000} value={previewNotes} onChange={(e) => setPreviewNotes(e.target.value)} /></label>
          </details>
          {previewNotes ? <p className="meta">{previewNotes}</p> : null}
          {preset === 'tauri-react' && !previewNotes ? <p className="meta">Preview shows the web interface. Open the native app separately with the project's tauri dev script after installing Rust and the platform tools.</p> : null}
          <div className="row"><button className="button tiny" disabled={busy || runStatus === 'running'} type="submit">{connecting ? 'Connect app' : 'Save stack'}</button><button className="button secondary tiny" disabled={busy} type="button" onClick={() => setEditing(null)}>Cancel</button></div>
          </div> : <button className="button secondary tiny" type="button" onClick={() => setEditing(null)}>Cancel</button>}
        </form>
      ) : null}
      {savedMessage && editing === null ? <p className="meta" role="status">{savedMessage}</p> : null}
      {error || loadError || current?.error ? <p className="error-inline" role="alert">{error || loadError || current?.error}</p> : null}
      <div className={`preview-stage ${phone ? 'phone' : ''}`}>
        {current?.previewMode !== 'none' && current?.status === 'running' && current.url ? (
          <iframe key={`${selected}:${current.id}:${frameVersion}`} src={current.url} title={`${current.name} preview`} sandbox="allow-scripts allow-forms allow-same-origin allow-downloads allow-popups allow-pointer-lock" allowFullScreen referrerPolicy="no-referrer" />
        ) : (
          <div className="preview-empty" role="status">
            <span className="preview-empty-icon" aria-hidden="true">▣</span>
            <strong>{current?.previewMode === 'none' ? 'Run this stack in its native tools or terminal' : current?.status === 'installing' ? 'Installing dependencies...' : current?.status === 'starting' ? 'Starting your app...' : current?.status === 'failed' ? 'Preview needs attention' : current?.available ? 'Your project, ready to preview' : current?.description ? 'Your stack is ready to build' : 'No preview here yet'}</strong>
            <p>{current?.previewMode === 'none' ? (current.previewNotes || current.reason || 'Build this target, then use its README to run it locally.') : current?.description && !current.available ? 'Choose this stack under Project build targets, then click Build to create the app. Its preview becomes available when the files are ready.' : current?.reason || (current?.needsInstall ? 'Install this app’s packages and run it locally with one click.' : active ? 'Startup progress appears in the logs below.' : 'Start the preview to see and use this version of your project.')}</p>
          </div>
        )}
      </div>
      {current?.previewNotes && editing === null && current.previewMode !== 'none' ? <p className="meta">{current.previewNotes}</p> : null}
      {current?.preset === 'tauri-react' && !current.previewNotes && editing === null ? <p className="meta">Tauri web preview. Native features need a separate desktop run using the project's tauri dev script.</p> : null}
      <div className="preview-footer">
        <span className="meta">{current?.directory || 'Choose a target'}{current ? ` · ${current.status}` : ''}</span>
        <div className="row">
          <span className="meta">{current?.previewMode === 'none' ? 'Native / terminal' : 'Local preview'}</span>
          {current?.target.startsWith('custom-') ? <><button className="preview-text-button" type="button" disabled={busy || runStatus === 'running'} onClick={() => edit(current)}>Edit</button><button className="preview-text-button" type="button" disabled={busy || runStatus === 'running'} onClick={() => void perform('remove')}>Remove</button></> : null}
        </div>
      </div>
      {current?.logs?.length ? <details className="preview-logs" open={current.status === 'failed'}><summary>Preview logs</summary><pre>{current.logs.join('\n')}</pre></details> : null}
    </section>
  );
}
