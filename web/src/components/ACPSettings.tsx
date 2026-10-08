import { useEffect, useRef, useState } from 'react';
import { acpArguments, acpConnectionIDs, fetchACPConnections, saveACPConnections, testACPConnection, type ACPConnection, type ACPConnectionID, type ACPProbeResult } from '../lib/acp';
import './acp-settings.css';

type ConnectionDraft = { name: string; command: string; arguments: string; model: string };
const emptyDraft = (): ConnectionDraft => ({ name: '', command: '', arguments: '', model: '' });
const connectionDraft = (connection: ACPConnection): ConnectionDraft => ({ name: connection.name, command: connection.command, arguments: connection.args.join('\n'), model: connection.model || '' });
const initialDrafts = (): Record<ACPConnectionID, ConnectionDraft> => ({ 'acp-1': emptyDraft(), 'acp-2': emptyDraft(), 'acp-3': emptyDraft() });

export function ACPSettings() {
  const [connections, setConnections] = useState<ACPConnection[] | null>(null);
  const [drafts, setDrafts] = useState(initialDrafts);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState('');
  const [messages, setMessages] = useState<Partial<Record<ACPConnectionID, string>>>({});
  const [probes, setProbes] = useState<Partial<Record<ACPConnectionID, ACPProbeResult>>>({});
  const request = useRef<AbortController | null>(null);
  const busyRef = useRef(false);

  async function load() {
    if (busyRef.current) return;
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setError('');
    try {
      const saved = await fetchACPConnections(controller.signal);
      if (controller.signal.aborted) return;
      const next = initialDrafts();
      for (const connection of saved) next[connection.id] = connectionDraft(connection);
      setConnections(saved);
      setDrafts(next);
      setProbes({});
    } catch (err) {
      if (!controller.signal.aborted) setError(err instanceof Error ? err.message : 'Could not read the saved ACP connections.');
    }
  }

  useEffect(() => { void load(); return () => request.current?.abort(); }, []);

  function update(id: ACPConnectionID, field: keyof ConnectionDraft, value: string) {
    setDrafts(current => ({ ...current, [id]: { ...current[id], [field]: value } }));
    setMessages(current => ({ ...current, [id]: '' }));
    if (field === 'command' || field === 'arguments') setProbes(current => ({ ...current, [id]: undefined }));
  }

  async function act(id: ACPConnectionID, action: 'save' | 'test' | 'remove') {
    if (busyRef.current || connections === null) return;
    const draft = drafts[id];
    const connection: ACPConnection = { id, name: draft.name.trim(), command: draft.command.trim(), args: acpArguments(draft.arguments), ...(draft.model ? { model: draft.model } : {}) };
    const probe = probes[id];
    if (action === 'save' && draft.model && probe && !probe.authRequired && !probe.models?.some(model => model.id === draft.model)) return;
    const controller = new AbortController();
    request.current?.abort();
    request.current = controller;
    busyRef.current = true;
    setBusy(`${id}:${action}`);
    setError('');
    setMessages(current => ({ ...current, [id]: '' }));
    try {
      if (action === 'test') {
        setProbes(current => ({ ...current, [id]: undefined }));
        const result = await testACPConnection(connection, controller.signal);
        if (controller.signal.aborted) return;
        setProbes(current => ({ ...current, [id]: result }));
        const label = [result.name || connection.name, result.version].filter(Boolean).join(' ');
        setMessages(current => ({ ...current, [id]: result.authRequired
          ? `${label} responded. Sign in with the agent's own command before building.`
          : `${label} connected. ${result.images ? 'Image input supported.' : 'Text input only.'}` }));
      } else {
        const next = connections.filter(item => item.id !== id);
        if (action === 'save') next.push(connection);
        const saved = await saveACPConnections(next, controller.signal);
        if (controller.signal.aborted) return;
        setConnections(saved);
        setDrafts(current => ({ ...current, [id]: action === 'remove' ? emptyDraft() : connectionDraft(connection) }));
        if (action === 'remove') setProbes(current => ({ ...current, [id]: undefined }));
        setMessages(current => ({ ...current, [id]: action === 'remove' ? 'Connection removed.' : draft.model ? 'Saved. Build will use the selected model.' : 'Saved. Available in Build with the agent’s default model.' }));
      }
    } catch (err) {
      if (!controller.signal.aborted) setMessages(current => ({ ...current, [id]: err instanceof Error ? err.message : 'Could not update this connection.' }));
    } finally {
      busyRef.current = false;
      if (!controller.signal.aborted) setBusy('');
    }
  }

  return <section className="acp-settings" aria-labelledby="acp-settings-title">
    <h3 id="acp-settings-title">ACP connections <span>Optional</span></h3>
    <p>Add up to three installed agents that support Agent Client Protocol. Sign in with the agent first. Choose its default model or one it makes available.</p>
    {error && <p role="alert">{error} <button type="button" disabled={!!busy} onClick={() => void load()}>Check again</button></p>}
    {connections === null && !error && <p>Loading connections…</p>}
    {connections !== null && acpConnectionIDs.map((id, index) => {
      const draft = drafts[id];
      const saved = connections.find(connection => connection.id === id);
      const ready = !!draft.name.trim() && !!draft.command.trim();
      const probe = probes[id];
      const tested = !!probe && !probe.authRequired;
      const models = tested ? probe.models || [] : [];
      const selectedModelKnown = models.some(model => model.id === draft.model);
      const unavailable = !!draft.model && tested && !selectedModelKnown;
      return <details className="acp-connection" key={id}>
        <summary><strong>{saved?.name || `Connection ${index + 1}`}</strong><span>{saved ? 'Saved' : 'Not configured'}</span></summary>
        <div className="acp-connection-fields">
          <label htmlFor={`${id}-name`}>Name<input id={`${id}-name`} value={draft.name} maxLength={80} disabled={!!busy} onChange={event => update(id, 'name', event.target.value)} placeholder="My coding agent" autoComplete="off" /></label>
          <label htmlFor={`${id}-command`}>Executable<input id={`${id}-command`} value={draft.command} maxLength={4096} disabled={!!busy} onChange={event => update(id, 'command', event.target.value)} placeholder="agent or /path/to/agent" autoComplete="off" spellCheck={false} /></label>
          <p>Enter only the program name or path. Add its ACP launch arguments below.</p>
          <label htmlFor={`${id}-arguments`}>Arguments<textarea id={`${id}-arguments`} value={draft.arguments} maxLength={16416} rows={3} disabled={!!busy} onChange={event => update(id, 'arguments', event.target.value)} placeholder={'--acp'} autoComplete="off" spellCheck={false} /></label>
          <p>One literal argument per line. Blank lines are ignored. Do not add shell quotes, commands, or credentials.</p>
          <label htmlFor={`${id}-model`}>Model<select id={`${id}-model`} value={draft.model} disabled={!!busy || (!models.length && !draft.model)} aria-describedby={`${id}-model-help`} onChange={event => update(id, 'model', event.target.value)}>
            <option value="">Use agent default</option>
            {draft.model && !selectedModelKnown && <option value={draft.model}>{draft.model} ({unavailable ? 'unavailable' : 'not checked'})</option>}
            {models.map(model => <option key={model.id} value={model.id} title={model.description}>{model.name || model.id}</option>)}
          </select></label>
          <p id={`${id}-model-help`}>{unavailable ? models.length ? 'The selected model is unavailable. Choose another model or select Use agent default.' : 'Managed by agent. The selected model is unavailable. Select Use agent default.'
            : !tested ? draft.model ? 'Test connection to confirm the selected model or select Use agent default.' : 'Test connection for available models.'
            : models.length ? 'Build uses the selected model, or the agent’s default when no model is selected.' : 'Managed by agent. This agent did not report selectable models.'}</p>
          {tested && probe.model?.trim() && <p>Agent default: {probe.model.trim()}.</p>}
          <div className="acp-connection-actions">
            <button type="button" disabled={!!busy || !ready || unavailable} onClick={() => void act(id, 'save')}>{busy === `${id}:save` ? 'Saving…' : 'Save'}</button>
            <button type="button" disabled={!!busy || !ready} onClick={() => void act(id, 'test')}>{busy === `${id}:test` ? 'Testing…' : 'Test connection'}</button>
            {saved && <button type="button" disabled={!!busy} onClick={() => void act(id, 'remove')}>{busy === `${id}:remove` ? 'Removing…' : 'Remove'}</button>}
          </div>
          <p>Test connection starts the agent to check its connection. Saving does not start it.</p>
          {messages[id] && <p role="status">{messages[id]}</p>}
        </div>
      </details>;
    })}
  </section>;
}
