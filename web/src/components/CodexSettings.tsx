import { useEffect, useRef, useState } from 'react';
import { codexRequest, type CodexLogin, type CodexStatus } from '../lib/codex';
import { openInBrowser } from '../lib/desktop-links';
import { isDesktopApp } from '../lib/downloads';

export function CodexSettings({ revision = 0 }: { revision?: number }) {
  const [status, setStatus] = useState<CodexStatus | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [login, setLogin] = useState<CodexLogin | null>(null);
  const request = useRef<AbortController | null>(null);
  const requestAction = useRef<'login' | 'restart' | null>(null);
  const loginRef = useRef<CodexLogin | null>(null);
  const popup = useRef<Window | null>(null);
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      if (requestAction.current !== 'login') request.current?.abort();
      popup.current?.close();
      if (loginRef.current) void codexRequest('login/cancel', { loginId: loginRef.current.loginId }).catch(() => {});
    };
  }, []);
  useEffect(() => {
    const controller = new AbortController();
    let checking = false;
    async function refresh() {
      if (checking) return;
      checking = true;
      try {
        const next = await codexRequest<CodexStatus>('status', undefined, controller.signal);
        if (controller.signal.aborted) return;
        setStatus(next);
        if (loginRef.current && next.loginPending === false && next.error) {
          loginRef.current = null; setLogin(null); popup.current?.close();
          setError(next.error);
        }
        if (next.connected && loginRef.current) {
          loginRef.current = null; setLogin(null); popup.current?.close();
          setError('');
          window.dispatchEvent(new Event('focus'));
        }
      } catch (err) {
        if (!controller.signal.aborted) setError(err instanceof Error ? err.message : 'Could not check Codex.');
      } finally { checking = false; }
    }
    void refresh();
    const timer = window.setInterval(() => { if (!document.hidden) void refresh(); }, login ? 1500 : 5000);
    return () => { controller.abort(); window.clearInterval(timer); };
  }, [revision, login?.loginId]);

  async function act(action: 'login' | 'restart') {
    if (busy) return;
    const controller = new AbortController(); request.current = controller; requestAction.current = action;
    setBusy(true); setError('');
    if (action === 'login' && !isDesktopApp()) {
      popup.current = window.open('about:blank', '_blank');
      if (popup.current) popup.current.opener = null;
    }
    try {
      if (action === 'restart') {
        const next = await codexRequest<CodexStatus>('restart', {}, controller.signal);
        if (!controller.signal.aborted) setStatus(next);
      }
      else {
        const next = await codexRequest<CodexLogin>('login', {}, controller.signal);
        if (!mounted.current) {
          if (next.loginId) await codexRequest('login/cancel', { loginId: next.loginId });
          return;
        }
        if (controller.signal.aborted) return;
        if (!next.loginId || !next.authorizationURL || new URL(next.authorizationURL).protocol !== 'https:') throw new Error('Codex sign-in could not start. Try again.');
        loginRef.current = next;
        setLogin(next);
        if (isDesktopApp()) openInBrowser(next.authorizationURL);
        else if (popup.current) popup.current.location.href = next.authorizationURL;
      }
    } catch (err) {
      if (mounted.current && !controller.signal.aborted) { popup.current?.close(); setError(err instanceof Error ? err.message : 'Could not connect to Codex.'); }
    } finally { requestAction.current = null; if (mounted.current) setBusy(false); }
  }
  async function cancelLogin() {
    if (!login || busy) return;
    setBusy(true);
    try {
      await codexRequest('login/cancel', { loginId: login.loginId });
      loginRef.current = null;
      if (mounted.current) setLogin(null);
      popup.current?.close();
    } catch (err) { if (mounted.current) setError(err instanceof Error ? err.message : 'Could not cancel sign-in.'); }
    finally { if (mounted.current) setBusy(false); }
  }
  return <section aria-label="Codex connection">
    <div className="agent-runtime-row">
      <strong>Codex{status?.version && <small className="codex-version"> {status.version}</small>}</strong>
      <span className={status?.connected ? 'agent-runtime-ready' : undefined}>{!status ? 'Checking…' : !status.installed ? 'Not installed' : status.busy ? 'Working' : status.connected ? 'Connected' : 'Sign in to chat and build'}</span>
    </div>
    {status && !status.installed && <p>Install Codex with <code>npm install -g @openai/codex</code>, then check again. <a href="https://developers.openai.com/codex/cli" target="_blank" rel="noreferrer">Setup instructions ↗</a></p>}
    {status?.installed && <div className="agent-runtime-row">
      {!status.connected && !login && <button type="button" disabled={busy || status.busy} onClick={() => void act('login')}>{busy ? 'Connecting…' : 'Sign in with ChatGPT'}</button>}
      <button type="button" disabled={busy || status.busy || !!login} title={status.busy ? 'Stop or finish Codex tasks before restarting.' : undefined} onClick={() => void act('restart')}>{busy && !login ? 'Please wait…' : 'Restart server'}</button>
    </div>}
    {login && <p><a href={login.authorizationURL} target="_blank" rel="noreferrer">Open sign-in ↗</a> <button type="button" disabled={busy} onClick={() => void cancelLogin()}>Cancel</button></p>}
    {(error || status?.error) && <p role="alert">{error || status?.error}</p>}
  </section>;
}

export function CodexConnection({ onClose }: { onClose(): void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => { dialog.current?.showModal(); return () => dialog.current?.close(); }, []);
  return <dialog ref={dialog} className="voice-settings" aria-label="Codex connection" onCancel={event => { event.preventDefault(); onClose(); }}>
    <header><h2>Codex</h2></header>
    <div className="voice-settings-body"><CodexSettings /></div>
    <footer><button type="button" onClick={onClose}>Done</button></footer>
  </dialog>;
}
