import { useEffect, useRef, useState } from 'react';
import { copyLocalBackendAccessToken, withServerAuthHeaders } from '../lib/server-auth';
import { readBuzzSessionResponse, type BuzzSession } from '../lib/buzz-session';
import { BuzzAvatar } from './BuzzAvatar';
import { loadBuzzSetup, saveBuzzSetup, shortenBuzzId } from '../lib/buzz-setup';

import { BuzzLiveSettings } from './BuzzLiveSettings';

type Action = 'check' | 'connect' | 'refresh' | 'disconnect';

export function BuzzMembersPanel({ elevenLabsKey = '' }: { elevenLabsKey?: string }) {
  const [savedSetup] = useState(loadBuzzSetup);
  const [relayUrl, setRelayUrl] = useState(savedSetup.relayUrl);
  const [channelId, setChannelId] = useState(savedSetup.channelId);
  const [editingChannel, setEditingChannel] = useState(false);
  const [privateKey, setPrivateKey] = useState('');
  const [authTag, setAuthTag] = useState('');
  const [session, setSession] = useState<BuzzSession | null>(null);
  const [available, setAvailable] = useState(false);
  const [error, setError] = useState('');
  const [copyMessage, setCopyMessage] = useState('');
  const [avatarRevision, setAvatarRevision] = useState(0);
  const [action, setAction] = useState<Action | null>('check');
  const request = useRef<AbortController | null>(null);
  const connected = available && session?.connected === true;
  const connecting = available && session?.connecting === true && !connected;
  const busy = action !== null || (available && session?.connecting === true);

  useEffect(() => { saveBuzzSetup({ relayUrl, channelId }); }, [relayUrl, channelId]);

  useEffect(() => {
    void callSession('check');
    // Read cached status only. This does not contact Buzz or refresh the roster.
    const timer = window.setInterval(() => {
      if (!request.current && document.visibilityState === 'visible') void callSession('check', undefined, true);
    }, 5000);
    return () => { window.clearInterval(timer); request.current?.abort(); };
  }, []);

  function clearCredentials() {
    setPrivateKey('');
    setAuthTag('');
  }

  function acceptSession(data: BuzzSession) {
    setSession(data);
    setAvailable(true);
    if (data.connected) {
      setRelayUrl(data.relayUrl);
      setChannelId(data.channelId);
      clearCredentials();
    } else {
      setCopyMessage('');
    }
  }

  async function callSession(nextAction: Action, body?: string, background = false) {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    if (!background) { setAction(nextAction); setError(''); setCopyMessage(''); }
    const method = nextAction === 'check' ? 'GET' : nextAction === 'disconnect' ? 'DELETE' : 'POST';
    const fetchSession = async (verb: string, payload?: string, refresh = false) => {
      const response = await fetch('/api/buzz/session' + (refresh ? '/refresh' : ''), {
        method: verb, body: payload, signal: controller.signal, cache: 'no-store',
        headers: withServerAuthHeaders({ 'Content-Type': 'application/json' }),
      });
      return readBuzzSessionResponse(response);
    };
    try {
      const data = await fetchSession(method, body, nextAction === 'refresh');
      if (controller.signal.aborted) return;
      acceptSession(data);
      if (nextAction === 'connect' || nextAction === 'refresh') setAvatarRevision((revision) => revision + 1);
      // A background check must not erase a failed Connect/Refresh message.
      if (nextAction === 'check') setError((previous) => previous.startsWith('Connection status unavailable:') ? '' : previous);
    } catch (cause) {
      if (controller.signal.aborted) return;
      const message = cause instanceof TypeError ? 'Could not reach Glowbom. Check that the local backend is running.'
        : cause instanceof Error ? cause.message : 'The connection request failed.';
      let recovered = false;
      // A request may fail after the backend changed state. Read it back before
      // offering Connect again, including conflicts with another OSS window.
      if (nextAction !== 'check') {
        try {
          const current = await fetchSession('GET');
          if (controller.signal.aborted) return;
          acceptSession(current);
          recovered = true;
        } catch {
          if (controller.signal.aborted) return;
        }
      }
      if (!recovered) { setAvailable(false); setCopyMessage(''); }
      setError(recovered ? message : 'Connection status unavailable: ' + message);
    } finally {
      if (request.current === controller) { request.current = null; setAction(null); }
    }
  }

  function connect(event: React.FormEvent) {
    event.preventDefault();
    const body = JSON.stringify({ relayUrl: relayUrl.trim(), channelId: channelId.trim(), privateKey: privateKey.trim(), authTag: authTag.trim() });
    clearCredentials();
    void callSession('connect', body);
  }

  async function copyToken() {
    setCopyMessage('');
    try {
      await copyLocalBackendAccessToken();
      setCopyMessage('Copied. Paste it into Glowbom Live’s local backend access token field, then press New Game.');
    } catch (cause) {
      setCopyMessage(cause instanceof Error ? cause.message : 'Could not copy the local backend access token.');
    }
  }

  async function copyId(value: string, label: string) {
    try {
      await navigator.clipboard.writeText(value);
      setCopyMessage(`${label} copied.`);
    } catch { setCopyMessage(`Could not copy ${label.toLowerCase()}. Allow clipboard access and try again.`); }
  }

  const status = action === 'connect' ? 'Connecting...' : action === 'disconnect' ? 'Disconnecting...'
    : !available ? (action === 'check' ? 'Checking connection...' : 'Status unavailable')
      : connected ? 'Connected' : connecting ? 'Connecting...' : 'Disconnected';

  return (
    <section className="glowbom-live-connection" aria-label="Live connection">
      <div className="glowbom-live-status-row">
        <p className="glowbom-live-status" role="status"><span className={`glowbom-live-status-dot${connected ? ' is-connected' : ''}${busy ? ' is-busy' : ''}`} aria-hidden="true" />{status}</p>
        <button className="glowbom-live-icon" type="button" aria-label="Refresh connection status" title="Refresh connection status" disabled={busy} onClick={() => void callSession('check')}><RefreshIcon /></button>
      </div>
      <form className="glowbom-live-form" onSubmit={connect} autoComplete="off">
        <label htmlFor="buzz-relay">Server URL</label>
        <input id="buzz-relay" type="url" placeholder="https://your-community.example" required value={relayUrl} disabled={busy || connected || !available} onChange={(event) => setRelayUrl(event.target.value)} />
        <label htmlFor="buzz-channel">Channel ID</label>
        <div className="glowbom-live-input-row">
          <input id="buzz-channel" placeholder="Channel UUID" required value={editingChannel ? channelId : shortenBuzzId(channelId)} disabled={busy || connected || !available} onFocus={() => setEditingChannel(true)} onBlur={() => setEditingChannel(false)} onChange={(event) => setChannelId(event.target.value)} />
          {channelId && <button className="glowbom-live-icon" type="button" aria-label="Copy channel ID" title="Copy channel ID" onClick={() => void copyId(channelId, 'Channel ID')}><CopyIcon /></button>}
        </div>
        {!connected && <p className="glowbom-live-hint">Server and channel are remembered on this device.</p>}
        {available && !connected && !connecting && <>
          <label htmlFor="buzz-key">Identity private key</label>
          <input id="buzz-key" type="password" autoComplete="off" spellCheck={false} placeholder="Hex or nsec" required value={privateKey} disabled={busy} onChange={(event) => setPrivateKey(event.target.value)} />
          <details className="glowbom-live-disclosure glowbom-live-advanced">
            <summary>Advanced</summary>
            <div className="glowbom-live-disclosure-body">
              <label htmlFor="buzz-auth-tag">Owner-auth tag <span className="glowbom-live-optional">Optional</span></label>
              <input id="buzz-auth-tag" type="password" autoComplete="off" spellCheck={false} placeholder="JSON auth tag for an agent identity" value={authTag} disabled={busy} onChange={(event) => setAuthTag(event.target.value)} />
            </div>
          </details>
        </>}
        <p className="glowbom-live-hint">{connected
          ? 'Closing settings keeps you connected. Credentials stay in local backend memory until you disconnect or quit the backend.'
          : available ? 'Your private key is cleared from this form when you connect and is not saved to disk.'
            : 'Refresh to check the local backend before connecting.'}</p>
        <div className="glowbom-live-actions">
          {connected || connecting || action === 'connect'
            ? <button type="button" disabled={action === 'disconnect'} onClick={() => { clearCredentials(); void callSession('disconnect'); }}>{action === 'disconnect' ? 'Disconnecting…' : 'Disconnect'}</button>
            : available && <button className="glowbom-live-primary" type="submit" disabled={busy}>Connect</button>}
        </div>
      </form>
      {copyMessage && <p className="glowbom-live-hint" role="status">{copyMessage}</p>}
      {error && <p className="glowbom-live-error" role="alert">{error}</p>}
      {connected && session && <div className="glowbom-live-connected-settings">
        <details className="glowbom-live-disclosure">
          <summary>Live messages and speech</summary>
          <div className="glowbom-live-disclosure-body"><BuzzLiveSettings key={session.channelId} existingKey={elevenLabsKey} /></div>
        </details>
        <details className="glowbom-live-disclosure">
          <summary>Channel members <span className="glowbom-live-count">{session.members.length}</span></summary>
          <div className="glowbom-live-disclosure-body">
            <div className="glowbom-live-status-row"><p className="glowbom-live-hint">Members from the last refresh.</p><button className="glowbom-live-icon" type="button" aria-label="Refresh members" title="Refresh members" disabled={busy} onClick={() => void callSession('refresh')}><RefreshIcon /></button></div>
            {!session.members.length && <p className="glowbom-live-hint">No members returned. Check the channel ID and access for this identity.</p>}
            <ul className="buzz-members">
              {session.members.map((member) => <li key={member.pubkey}>
                <BuzzAvatar key={`${session.channelId}:${avatarRevision}`} member={member} relayUrl={session.relayUrl} />
                <div>
                  <strong>{member.displayName}</strong> <span className="glowbom-live-hint">{member.role}</span>
                  <button className="buzz-id-copy" type="button" aria-label={`Copy identity ID for ${member.displayName}`} onClick={() => void copyId(member.pubkey, 'Identity ID')}>{shortenBuzzId(member.pubkey)}</button>
                </div>
              </li>)}
            </ul>
          </div>
        </details>
        <details className="glowbom-live-disclosure">
          <summary>Connect the Live app</summary>
          <div className="glowbom-live-disclosure-body">
            <p className="glowbom-live-hint">This token gives Glowbom Live on this computer access to the local backend. It is separate from your identity private key.</p>
            <button type="button" className="glowbom-live-copy-token" disabled={action === 'disconnect'} onClick={() => void copyToken()}><CopyIcon />Copy access token</button>
          </div>
        </details>
      </div>}
    </section>
  );
}

function RefreshIcon() {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M20 7v5h-5M20 12a8 8 0 1 0-2.3 5.7M20 12l-3-5" /></svg>;
}

function CopyIcon() {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><rect x="8" y="8" width="12" height="12" rx="2" /><path d="M16 4H6a2 2 0 0 0-2 2v10" /></svg>;
}
