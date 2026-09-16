import { useEffect, useState } from 'react';
import { withServerAuthHeaders } from '../lib/server-auth';

export function BuzzLiveSettings({ existingKey = '' }: { existingKey?: string }) {
  const [key, setKey] = useState('');
  const [status, setStatus] = useState('Connecting messages...');
  const [available, setAvailable] = useState(false);
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    const controller = new AbortController();
    let pending = false;
    async function check() {
      if (pending) return;
      pending = true;
      try {
        const response = await fetch('/api/buzz/session/messages', { headers: withServerAuthHeaders(), signal: controller.signal, cache: 'no-store' });
        if (!response.ok) throw new Error();
        const data = await response.json();
        if (!controller.signal.aborted) { setStatus(`Messages: ${data.status}`); setAvailable(data.speechAvailable === true); }
      } catch { if (!controller.signal.aborted) setStatus('Messages unavailable. Check the backend.'); }
      finally { pending = false; }
    }
    void check();
    const timer = window.setInterval(check, 3000);
    return () => { controller.abort(); window.clearInterval(timer); };
  }, []);
  async function configure(value: string) {
    setBusy(true); setNotice(''); setKey('');
    try {
      const response = await fetch('/api/buzz/session/speech', { method: 'POST', headers: withServerAuthHeaders({ 'Content-Type': 'application/json' }), body: JSON.stringify({ key: value.trim(), remember: true }) });
      if (!response.ok) throw new Error();
      setAvailable(!!value.trim());
      setNotice(value.trim() ? 'Key saved on this computer. New Live sessions enable limited speech automatically.' : 'Speech key cleared, including the saved copy.');
    } catch { setNotice('Could not update speech. Check the local connection.'); }
    finally { setBusy(false); }
  }
  return <section aria-label="Live messages and speech">
    <p role="status">{status}</p>
    <p className="meta">ElevenLabs: {available ? 'key configured' : 'no key configured'}. New Live sessions start with limited speech when a key is configured.</p>
    <label className="field-label" htmlFor="buzz-eleven-key">ElevenLabs API key for Live</label>
    <input id="buzz-eleven-key" className="input" type="password" autoComplete="off" value={key} onChange={(event) => setKey(event.target.value)} placeholder="Saved privately on this computer" />
    <div className="row">
      <button className="button secondary" disabled={busy || !key.trim()} onClick={() => void configure(key)}>Use key for Live</button>
      {existingKey.trim() && <button className="button secondary" disabled={busy} onClick={() => void configure(existingKey)}>Use existing ElevenLabs key</button>}
      {available && <button className="button secondary" disabled={busy} onClick={() => void configure('')}>Clear speech key</button>}
    </div>
    <p className="meta">The key is stored in a local backend file, not encrypted. On macOS/Linux, only your account can read it. Speech sends messages to ElevenLabs and uses credits. Changing the key turns current speech off. Clear speech key removes the saved key; any separately saved provider key stays unchanged.</p>
    {notice && <p role="status" className="meta">{notice}</p>}
  </section>;
}
