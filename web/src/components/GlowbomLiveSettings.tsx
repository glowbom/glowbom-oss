import { useCallback, useEffect, useId, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { BuzzMembersPanel } from './BuzzMembersPanel';
import { currentLiveDownload, GLOWBOM_LIVE_DOWNLOADS, GLOWBOM_LIVE_PAGE, type LiveDownloadTarget } from '../lib/glowbom-live-downloads';
import { getLiveAppStatus, launchLiveApp, liveAppUpdateAvailable, LiveAppLaunchError, LiveAppStatusError } from '../lib/glowbom-live-app';
import { openGlowbomLiveDownload } from '../lib/downloads';
import './glowbom-live-settings.css';

export function GlowbomLiveSettings({ onClose, elevenLabsKey = '' }: { onClose(): void; elevenLabsKey?: string }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const title = useId();
  const description = useId();
  const downloads = useRef<HTMLDetailsElement>(null);
  const [downloadTarget, setDownloadTarget] = useState<LiveDownloadTarget | null>(null);
  const [installation, setInstallation] = useState<'checking' | 'installed' | 'update' | 'download' | 'unknown'>('checking');
  const alive = useRef(false);
  const statusRequest = useRef<AbortController | null>(null);
  const [statusError, setStatusError] = useState('');
  const [downloading, setDownloading] = useState(false);
  const downloadBusy = useRef(false);
  const launchRequest = useRef<AbortController | null>(null);
  const [launching, setLaunching] = useState(false);
  const [launchError, setLaunchError] = useState('');

  const checkInstallation = useCallback(async () => {
    if (statusRequest.current) return;
    const controller = new AbortController();
    statusRequest.current = controller;
    const timeout = window.setTimeout(() => controller.abort(), 5000);
    try {
      const status = await getLiveAppStatus(controller.signal);
      if (alive.current && statusRequest.current === controller) {
        setInstallation(liveAppUpdateAvailable(status) ? 'update' : status.installed ? 'installed' : 'download'); setStatusError('');
      }
    } catch (error) {
      if (alive.current && statusRequest.current === controller) {
        setInstallation('unknown');
        setStatusError(error instanceof LiveAppStatusError ? error.message : 'Could not check whether Glowbom Live is installed. Try again.');
      }
    } finally {
      window.clearTimeout(timeout);
      if (statusRequest.current === controller) statusRequest.current = null;
    }
  }, []);

  useEffect(() => {
    alive.current = true;
    void checkInstallation();
    const refresh = () => { if (!document.hidden) void checkInstallation(); };
    window.addEventListener('focus', refresh);
    document.addEventListener('visibilitychange', refresh);
    return () => {
      alive.current = false;
      statusRequest.current?.abort(); statusRequest.current = null;
      launchRequest.current?.abort();
      window.removeEventListener('focus', refresh);
      document.removeEventListener('visibilitychange', refresh);
    };
  }, [checkInstallation]);

  async function getLiveApp(target: LiveDownloadTarget | null) {
    if (downloadBusy.current) return;
    downloadBusy.current = true; setDownloading(true); setLaunchError('');
    if (downloads.current) downloads.current.open = false;
    try { await openGlowbomLiveDownload(target); }
    catch (error) { if (alive.current) setLaunchError(error instanceof Error ? error.message : 'Could not open the download. Please try again.'); }
    finally { downloadBusy.current = false; if (alive.current) setDownloading(false); }
  }

  async function openLiveApp() {
    if (launchRequest.current) return;
    const controller = new AbortController();
    launchRequest.current = controller;
    setLaunching(true);
    setLaunchError('');
    try {
      await launchLiveApp(controller.signal);
    } catch (error) {
      if (!controller.signal.aborted) {
        setLaunchError(error instanceof LiveAppLaunchError ? error.message : 'Could not open Glowbom Live. Try again.');
        if (error instanceof LiveAppLaunchError && error.code === 'not_installed') { setInstallation('download'); setStatusError(''); }
      }
    } finally {
      if (launchRequest.current === controller) launchRequest.current = null;
      if (!controller.signal.aborted) setLaunching(false);
    }
  }

  useEffect(() => {
    let active = true;
    void currentLiveDownload().then(target => { if (active) setDownloadTarget(target); });
    const closeDownloads = (event: PointerEvent) => {
      if (event.target instanceof Node && downloads.current && !downloads.current.contains(event.target)) downloads.current.open = false;
    };
    document.addEventListener('pointerdown', closeDownloads);
    return () => { active = false; document.removeEventListener('pointerdown', closeDownloads); };
  }, []);

  useEffect(() => {
    const element = dialog.current;
    const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    element?.showModal();
    return () => {
      element?.close();
      if (previousFocus?.isConnected) previousFocus.focus({ preventScroll: true });
    };
  }, []);

  return createPortal(<dialog ref={dialog} className="glowbom-live-settings" aria-labelledby={title} aria-describedby={description} onCancel={(event) => { event.preventDefault(); onClose(); }}>
    <header className="glowbom-live-header">
      <div>
        <div className="glowbom-live-title"><h2 id={title}>Glowbom Live</h2><span className="glowbom-live-preview">Preview</span></div>
        <p id={description}>Connect your community to Glowbom Live.</p>
      </div>
      <button className="glowbom-live-icon" type="button" aria-label="Close Glowbom Live settings" title="Close" onClick={onClose}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" aria-hidden="true"><path d="m6 6 12 12M18 6 6 18" /></svg></button>
    </header>
    <div className="glowbom-live-body"><BuzzMembersPanel elevenLabsKey={elevenLabsKey} /></div>
    <footer className="glowbom-live-footer">
      <div className="glowbom-live-downloads">
        {installation === 'checking' ? <button className="glowbom-live-app-action" type="button" disabled aria-label="Checking for Glowbom Live"><span className="glowbom-live-status-dot is-busy" aria-hidden="true" />Glowbom Live</button> : installation === 'installed' ? <button className="glowbom-live-app-action" type="button" disabled={launching} aria-busy={launching} onClick={() => void openLiveApp()}>
          {launching ? <span className="glowbom-live-status-dot is-busy" aria-hidden="true" /> : <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M14 4h6v6m0-6L10 14M10 4H4v16h16v-6" /></svg>}
          {launching ? 'Opening Glowbom Live…' : 'Open Glowbom Live'}
        </button> : installation === 'unknown' ? <button className="glowbom-live-app-action" type="button" onClick={() => { setInstallation('checking'); void checkInstallation(); }}>
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M20 7a9 9 0 1 0 1 9M20 3v5h-5" /></svg>Check for Glowbom Live
        </button> : <>
        <a className="glowbom-live-download" href={downloadTarget ? GLOWBOM_LIVE_DOWNLOADS[downloadTarget].href : GLOWBOM_LIVE_PAGE} target="_blank" rel="noopener noreferrer" aria-busy={downloading} onClick={(event) => { event.preventDefault(); void getLiveApp(downloadTarget); }} title={downloadTarget ? `Download for ${GLOWBOM_LIVE_DOWNLOADS[downloadTarget].label} (${GLOWBOM_LIVE_DOWNLOADS[downloadTarget].detail})` : 'View Glowbom Live downloads'}>
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M12 3v12m-4-4 4 4 4-4M4 16v4h16v-4" /></svg>
          {downloading ? 'Opening download…' : installation === 'update' ? 'Update Glowbom Live' : 'Get Glowbom Live'}
        </a>
        <details ref={downloads} className="glowbom-live-download-options" onKeyDown={event => {
          if (event.key === 'Escape' && event.currentTarget.open) {
            event.preventDefault(); event.stopPropagation(); event.currentTarget.open = false;
            event.currentTarget.querySelector('summary')?.focus();
          }
        }}>
          <summary aria-label="Other Glowbom Live downloads" title="Other downloads"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="m7 10 5 5 5-5" /></svg></summary>
          <div className="glowbom-live-download-menu">
            {Object.entries(GLOWBOM_LIVE_DOWNLOADS).map(([target, download]) => <a key={target} href={download.href} target="_blank" rel="noopener noreferrer" onClick={(event) => { event.preventDefault(); void getLiveApp(target as LiveDownloadTarget); }}><span>{download.label}</span><small>{download.detail}</small></a>)}
          </div>
        </details>
        </>}
      </div>
      <button type="button" onClick={onClose}>Done</button>
      {statusError && <p className="glowbom-live-launch-error" role="alert">{statusError}</p>}
      {launchError && <p className="glowbom-live-launch-error" role="alert">{launchError}</p>}
    </footer>
  </dialog>, document.body);
}
