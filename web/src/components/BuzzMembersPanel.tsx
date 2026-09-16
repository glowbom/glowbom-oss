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
      const message = cause instanceof TypeError ? 'Could not reach the local backend. Check that OSS is running.'
        : cause instanceof Error ? cause.message : 'The Buzz request failed.';
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
    <details className="buzz-panel" onToggle={(event) => { if (!event.currentTarget.open) clearCredentials(); }}>
      <summary>Buzz connection <span className="meta">Preview</span></summary>
      <p className={`buzz-connection-status ${connected ? 'ok' : ''}`} role="status">{status}</p>
      <p className="meta">Connect one channel for OSS and Glowbom Live. Members are a snapshot; new messages can appear and speak in Glowbom Live.</p>
      <form onSubmit={connect} autoComplete="off">
        <label className="field-label" htmlFor="buzz-relay">Relay URL</label>
        <input className="input" id="buzz-relay" type="url" placeholder="https://your-community.example" required value={relayUrl} disabled={busy || connected || !available} onChange={(event) => setRelayUrl(event.target.value)} />
        <label className="field-label" htmlFor="buzz-channel">Channel ID</label>
        <div className="row">
          <input className="input buzz-channel-input" id="buzz-channel" placeholder="Channel UUID" required value={editingChannel ? channelId : shortenBuzzId(channelId)} disabled={busy || connected || !available} onFocus={() => setEditingChannel(true)} onBlur={() => setEditingChannel(false)} onChange={(event) => setChannelId(event.target.value)} />
          {channelId && <button className="button secondary" type="button" onClick={() => void copyId(channelId, 'Channel ID')}>Copy channel ID</button>}
        </div>
        <p className="meta">Relay URL and channel ID are remembered in this browser.</p>
        {available && !connected && !connecting && <>
          <label className="field-label" htmlFor="buzz-key">Identity private key</label>
          <input className="input" id="buzz-key" type="password" autoComplete="off" spellCheck={false} placeholder="Hex or nsec" required value={privateKey} disabled={busy} onChange={(event) => setPrivateKey(event.target.value)} />
          <label className="field-label" htmlFor="buzz-auth-tag">Owner-auth tag (optional)</label>
          <input className="input" id="buzz-auth-tag" type="password" autoComplete="off" spellCheck={false} placeholder="JSON auth tag for an agent identity" value={authTag} disabled={busy} onChange={(event) => setAuthTag(event.target.value)} />
        </>}
        <p className="meta">{connected
          ? 'Credentials stay in local backend memory until Disconnect or backend shutdown. Closing this page does not disconnect.'
          : available ? 'Your key is sent to the local backend and cleared from this form. It is not saved to disk.'
            : 'Check the local backend before connecting. No connection state has been confirmed.'}</p>
        <div className="row">
          {connected && <button className="button secondary" type="button" disabled={action === 'disconnect'} onClick={() => void copyToken()}>Copy local backend access token</button>}
          {connected || connecting || action === 'connect'
            ? <button className="button secondary" type="button" disabled={action === 'disconnect'} onClick={() => { clearCredentials(); void callSession('disconnect'); }}>{action === 'disconnect' ? 'Disconnecting...' : 'Disconnect'}</button>
            : available ? <button className="button secondary" type="submit" disabled={busy}>Connect</button>
              : <button className="button secondary" type="button" disabled={busy} onClick={() => void callSession('check')}>{busy ? 'Checking...' : 'Check connection'}</button>}
        </div>
      </form>
      {connected && <p className="meta">Copy this token into Glowbom Live on this computer. It grants access to the local OSS backend and is separate from your Buzz private key.</p>}
      {copyMessage && <p className="meta" role="status">{copyMessage}</p>}
      {error && <p className="error-inline" role="alert">{error}</p>}
      {connected && session && <div>
        <BuzzLiveSettings key={session.channelId} existingKey={elevenLabsKey} />
        <div className="row buzz-roster-heading">
          <p className="meta">{session.members.length} channel members</p>
          <button className="button secondary" type="button" disabled={busy} onClick={() => void callSession('refresh')}>{action === 'refresh' || session.connecting ? 'Refreshing...' : 'Refresh members'}</button>
        </div>
        {!session.members.length && <p className="meta">No roster returned. Check the channel ID and access for this identity.</p>}
        <ul className="buzz-members">
          {session.members.map((member) => <li key={member.pubkey}>
            <BuzzAvatar key={`${session.channelId}:${avatarRevision}`} member={member} relayUrl={session.relayUrl} />
            <div>
              <strong>{member.displayName}</strong> <span className="meta">{member.role}</span>
              <button className="buzz-id-copy" type="button" aria-label={`Copy identity ID for ${member.displayName}`} onClick={() => void copyId(member.pubkey, 'Identity ID')}>{shortenBuzzId(member.pubkey)}</button>
            </div>
          </li>)}
        </ul>
      </div>}
    </details>
  );
}
