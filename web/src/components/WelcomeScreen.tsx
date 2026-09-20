import { useEffect, useRef, useState } from 'react';
import { accountMessage, accountRequest, type AccountStatus, type LoginState } from '../lib/account';
import { AppearancePicker, type AppearanceProps } from './AppearancePicker';

export function WelcomeScreen({ onContinue, ...appearance }: AppearanceProps & { onContinue: () => void }) {
  const [account, setAccount] = useState<AccountStatus | null>(null);
  const [state, setState] = useState<LoginState>('idle');
  const [checking, setChecking] = useState(true);
  const [error, setError] = useState('');
  const [revision, setRevision] = useState(0);
  const controller = useRef<AbortController | null>(null);
  const pending = state === 'pending' || state === 'canceling';

  useEffect(() => {
    const abort = new AbortController();
    controller.current = abort;
    let timer: ReturnType<typeof setTimeout>;
    const check = async () => {
      try {
        const login = await accountRequest<{ state: LoginState }>('login', 'GET', abort.signal);
        if (abort.signal.aborted) return;
        setState(login.state);
        if (login.state === 'pending' || login.state === 'canceling') {
          setChecking(false);
          timer = setTimeout(check, 1200);
          return;
        }
        const status = await accountRequest<AccountStatus>('status', 'GET', abort.signal);
        if (abort.signal.aborted) return;
        setAccount(status);
        if (status.status === 'unavailable') setError(accountMessage(status.code));
        else if (login.state === 'failed' && status.status !== 'signed_in') setError('Sign-in did not finish. Try again, or continue without an account.');
        else setError('');
      } catch (error) {
        if (!abort.signal.aborted) {
          setState('idle');
          setError(error instanceof Error ? error.message : 'Could not connect to Glowbom. Please try again.');
        }
      } finally {
        if (!abort.signal.aborted) setChecking(false);
      }
    };
    void check();
    return () => { abort.abort(); clearTimeout(timer); };
  }, [revision]);

  const action = async (path: 'login' | 'login/cancel' | 'logout') => {
    setChecking(true);
    setError('');
    try {
      await accountRequest(path, 'POST', controller.current?.signal);
      if (controller.current?.signal.aborted) return;
      if (path === 'logout') setAccount(null);
      setRevision((value) => value + 1);
    } catch (error) {
      if (controller.current?.signal.aborted) return;
      setError(error instanceof Error ? error.message : 'Could not connect to Glowbom. Please try again.');
      setChecking(false);
    }
  };

  const signedIn = account?.status === 'signed_in';
  return <main className="welcome-screen">
    <div className="welcome-toolbar"><AppearancePicker {...appearance} /></div>
    <div className="welcome-brand">
      <img src={`${import.meta.env.BASE_URL}logo-svg.svg`} alt="Glowbom" />
      <p>Sketch to software</p>
    </div>
    <section className="welcome-card" aria-labelledby="welcome-title">
      <h1 id="welcome-title">Continue</h1>
      {signedIn && <div className="welcome-account"><span>{account.email || 'Signed in with Glowbom'}</span><span className="account-badge">{account.subscriptionStatus === 'premium' ? 'Premium' : account.subscriptionStatus === 'trialing' ? 'Trial' : 'Glowbom account'}</span></div>}
      <div className="welcome-status" role="status" aria-live="polite">
        {checking ? 'Checking your connection…' : pending ? 'Finish signing in in your browser.' : state === 'canceled' ? 'Sign-in canceled.' : ''}
      </div>
      {error && <p className="welcome-error" role="alert">{error}</p>}
      {signedIn ? <button className="welcome-primary" onClick={onContinue}>Continue <span aria-hidden="true">↗</span></button> :
        <button className="welcome-primary" disabled={checking || pending} onClick={() => void action('login')}>
          {pending ? 'Signing in with Glowbom…' : 'Continue with Glowbom'}<span aria-hidden="true">↗</span>
        </button>}
      {pending && <button className="welcome-secondary" disabled={checking} onClick={() => void action('login/cancel')}>Cancel sign-in</button>}
      {!pending && !signedIn && <button className="welcome-secondary" onClick={onContinue}>Continue without an account</button>}
      {signedIn && <button className="welcome-text-button" disabled={checking} onClick={() => void action('logout')}>Sign out</button>}
      {error && !pending && <button className="welcome-text-button" disabled={checking} onClick={() => { setChecking(true); setRevision((value) => value + 1); }}>Check again</button>}
    </section>
  </main>;
}
