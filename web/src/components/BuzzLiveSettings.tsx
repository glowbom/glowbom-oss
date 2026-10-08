import { useEffect, useState } from 'react';
import { withServerAuthHeaders } from '../lib/server-auth';

export function BuzzLiveSettings({ existingKey = '' }: { existingKey?: string }) {
  const [key, setKey] = useState('');
  const [status, setStatus] = useState('Connecting messages...');
  const [keyConfigured, setKeyConfigured] = useState(false);
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
        if (!controller.signal.aborted) { setStatus(`Messages: ${data.status}`); setKeyConfigured(data.speechAvailable === true); }
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
      setKeyConfigured(!!value.trim());
      setNotice(value.trim() ? 'ElevenLabs key saved on this computer. Select ElevenLabs in Glowbom Live to use it.' : 'ElevenLabs key cleared, including the saved copy.');
    } catch { setNotice('Could not update the ElevenLabs key. Check the local connection.'); }
    finally { setBusy(false); }
  }
  return <section className="glowbom-live-speech" aria-label="Live messages and speech">
    <p role="status">{status}</p>
    <p className="meta">Choose the default voice source in Glowbom Live, under Messages. ElevenLabs needs a key here. Local voice uses KittenTTS Mini or VoiceStudio on 127.0.0.1:3900, one service at a time. System voices use the device running Glowbom Live. Neither local nor system voice needs an ElevenLabs key.</p>
    <p className="meta">ElevenLabs: {keyConfigured ? 'key configured' : 'no key configured'}.</p>
    <label className="field-label" htmlFor="buzz-eleven-key">ElevenLabs API key for Live (optional)</label>
    <input id="buzz-eleven-key" className="input" type="password" autoComplete="off" value={key} onChange={(event) => setKey(event.target.value)} placeholder="Saved privately on this computer" />
    <div className="row">
      <button className="button secondary" disabled={busy || !key.trim()} onClick={() => void configure(key)}>Use key for Live</button>
      {existingKey.trim() && <button className="button secondary" disabled={busy} onClick={() => void configure(existingKey)}>Use existing ElevenLabs key</button>}
      {keyConfigured && <button className="button secondary" disabled={busy} onClick={() => void configure('')}>Clear ElevenLabs key</button>}
    </div>
    <p className="meta">The key is stored in a local backend file, not encrypted. On macOS/Linux, only your account can read it. Messages spoken with ElevenLabs use credits. Changing the key turns current speech off. Clearing it leaves any separately saved provider key unchanged.</p>
    {notice && <p role="status" className="meta">{notice}</p>}
  </section>;
}
