import { desktopConnection, withServerAuthHeaders } from './server-auth';
import { GLOWBOM_LIVE_VERSION } from './glowbom-live-downloads';

export type LiveAppStatus = { installed: boolean; platform: string; version?: string };

export function liveAppUpdateAvailable(status: LiveAppStatus): boolean {
  if (!status.installed || !status.version) return false;
  const match = /^(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/.exec(status.version);
  if (!match) return false;
  const installed = match.slice(1, 4).map(Number);
  if (!installed.every(Number.isSafeInteger)) return false;
  const available = GLOWBOM_LIVE_VERSION.split('.').map(Number);
  for (let index = 0; index < 3; index++) {
    const current = installed[index];
    const next = available[index];
    if (current === undefined || next === undefined) return false;
    if (current !== next) return current < next;
  }
  return Boolean(match[4]);
}
export class LiveAppLaunchError extends Error {
  constructor(message: string, readonly code = '') { super(message); }
}
export class LiveAppStatusError extends Error {
  constructor(message: string, readonly code = '') { super(message); }
}

function desktopBridge() {
  return typeof window === 'undefined' ? undefined : (window as Window & { __TAURI__?: { core?: { invoke<T>(command: string, args?: Record<string, unknown>): Promise<T> } } }).__TAURI__?.core;
}

export async function getLiveAppStatus(signal?: AbortSignal): Promise<LiveAppStatus> {
  const bridge = desktopBridge();
  if (bridge?.invoke) {
    try {
      const status = await bridge.invoke('live_app_status');
      signal?.throwIfAborted();
      return await readLiveAppStatus(Response.json(status));
    } catch (error) {
      signal?.throwIfAborted();
      if (error === 'discovery_failed') throw new LiveAppStatusError('Could not check whether Glowbom Live is installed. Try again.');
      throw new LiveAppStatusError('Reopen the latest Glowbom app to check for the installed app.', 'desktop_outdated');
    }
  }
  return readLiveAppStatus(await fetch('/api/live/app', { headers: withServerAuthHeaders(), signal, cache: 'no-store' }));
}

export async function launchLiveApp(signal?: AbortSignal): Promise<void> {
  const headers = withServerAuthHeaders();
  const bridge = desktopBridge();
  if (bridge?.invoke) {
    const authorization = headers.get('Authorization') || '';
    const token = authorization.startsWith('Bearer ') ? authorization.slice(7) : '';
    if (!token) throw new LiveAppLaunchError('Local app access was denied. Reopen Glowbom and try again.');
    let result: unknown;
    try {
      result = await bridge.invoke('open_live_app', { token, backendUrl: desktopConnection()?.backendUrl || import.meta.env.VITE_BACKEND_TARGET || 'http://127.0.0.1:4569' });
    } catch (error) {
      signal?.throwIfAborted();
      const code = typeof error === 'string' ? error : '';
      if (!['not_installed', 'already_running', 'launch_failed', 'unsupported_platform', 'invalid_connection'].includes(code)) {
        throw new LiveAppLaunchError('Could not open Glowbom Live. Reopen the latest Glowbom app and try again.');
      }
      return readLiveAppLaunch(Response.json({ code }, { status: code === 'not_installed' ? 404 : 503 }));
    }
    signal?.throwIfAborted();
    return readLiveAppLaunch(Response.json(result));
  }
  return readLiveAppLaunch(await fetch('/api/live/app', { method: 'POST', headers, signal }));
}

export async function readLiveAppStatus(response: Response): Promise<LiveAppStatus> {
  if (response.status === 404 || response.status === 405) throw new LiveAppStatusError('Restart Glowbom after any active builds finish to check for the installed app.', 'backend_outdated');
  if (response.status === 401 || response.status === 403) throw new LiveAppStatusError('Local app access was denied. Reopen Glowbom and try again.', 'auth_required');
  const value: unknown = await response.json().catch(() => null);
  if (!response.ok || !value || typeof value !== 'object' || !('installed' in value) || typeof value.installed !== 'boolean' || !('platform' in value) || typeof value.platform !== 'string') {
    throw new LiveAppStatusError('Could not check whether Glowbom Live is installed. Try again.');
  }
  const version = 'version' in value && typeof value.version === 'string' && value.version.length <= 64 ? value.version : undefined;
  return { installed: value.installed, platform: value.platform, ...(version ? { version } : {}) };
}

export async function readLiveAppLaunch(response: Response): Promise<void> {
  const value: unknown = await response.json().catch(() => null);
  if (response.ok && value && typeof value === 'object' && 'success' in value && value.success === true) return;
  if (response.status === 401 || response.status === 403) {
    throw new LiveAppLaunchError('Local app access was denied. Reopen Glowbom and try again.');
  }
  const code = value && typeof value === 'object' && 'code' in value ? String(value.code) : '';
  if (code === 'not_installed') throw new LiveAppLaunchError('Glowbom Live could not be found. Install the app, then reopen these settings.', code);
  if (response.status === 404 || response.status === 405) throw new LiveAppLaunchError('Restart Glowbom after any active builds finish, then try again.');
  if (code === 'unsupported_platform') throw new LiveAppLaunchError('Opening Glowbom Live is not supported on this device yet.', code);
  if (code === 'local_backend_required') throw new LiveAppLaunchError('Open Glowbom on this computer to launch Glowbom Live.', code);
  if (code === 'already_running') throw new LiveAppLaunchError('Glowbom Live is already open. Close it and try again to connect automatically.', code);
  if (code === 'invalid_connection') throw new LiveAppLaunchError('Reopen Glowbom to reconnect the local app.', code);
  throw new LiveAppLaunchError('Could not open Glowbom Live. Try again.');
}
