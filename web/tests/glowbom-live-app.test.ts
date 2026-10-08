import { afterEach, describe, expect, test } from 'bun:test';
import { getLiveAppStatus, launchLiveApp, liveAppUpdateAvailable, LiveAppLaunchError, readLiveAppLaunch, readLiveAppStatus } from '../src/lib/glowbom-live-app';
import { GLOWBOM_LIVE_VERSION } from '../src/lib/glowbom-live-downloads';

const originalWindow = Object.getOwnPropertyDescriptor(globalThis, 'window');
afterEach(() => {
  if (originalWindow) Object.defineProperty(globalThis, 'window', originalWindow);
  else Reflect.deleteProperty(globalThis, 'window');
});

function desktop(invoke: (command: string, args?: Record<string, unknown>) => Promise<unknown>) {
  Object.defineProperty(globalThis, 'window', { configurable: true, value: {
    __TAURI__: { core: { invoke } },
    location: { href: 'http://127.0.0.1:4572/' },
    sessionStorage: { getItem: () => 'fixture-local-token' },
  } });
}

describe('native Glowbom Live connection', () => {
  test('uses native installation detection without relying on the backend endpoint', async () => {
    desktop(async (command, args) => {
      expect(command).toBe('live_app_status');
      expect(args).toBeUndefined();
      return { installed: true, platform: 'darwin', launchSupported: true };
    });
    expect(await getLiveAppStatus()).toEqual({ installed: true, platform: 'darwin' });
  });

  test('passes the local connection to the fixed native launch command', async () => {
    desktop(async (command, args) => {
      expect(command).toBe('open_live_app');
      expect(args).toEqual({ token: 'fixture-local-token', backendUrl: 'http://127.0.0.1:4569' });
      return { success: true };
    });
    await expect(launchLiveApp()).resolves.toBeUndefined();
  });

  test('uses the packaged runtime connection instead of the development port and stored token', async () => {
    desktop(async (command, args) => {
      expect(command).toBe('open_live_app');
      expect(args).toEqual({ token: 'fresh-native-token', backendUrl: 'http://127.0.0.1:4587' });
      return { success: true };
    });
    Object.assign(window, { __GLOWBOM_DESKTOP__: { token: 'fresh-native-token', backendUrl: 'http://127.0.0.1:4587' } });
    await expect(launchLiveApp()).resolves.toBeUndefined();
  });

  test('missing native support is unknown, not an absent installation', async () => {
    desktop(async () => { throw 'Command live_app_status not found'; });
    await expect(getLiveAppStatus()).rejects.toMatchObject({ code: 'desktop_outdated' });
  });

  test('keeps native errors safe and handles an already open app', async () => {
    desktop(async () => { throw 'already_running'; });
    await expect(launchLiveApp()).rejects.toMatchObject({ code: 'already_running' });
    desktop(async () => { throw '/Users/private/secret'; });
    await expect(launchLiveApp()).rejects.toThrow('Could not open Glowbom Live. Reopen the latest Glowbom app and try again.');
  });
});

describe('Glowbom Live app responses', () => {
  test('preserves the installed version and tolerates older backends without it', async () => {
    expect(await readLiveAppStatus(Response.json({ installed: true, platform: 'darwin', version: '0.1.0' }))).toEqual({ installed: true, platform: 'darwin', version: '0.1.0' });
    expect(await readLiveAppStatus(Response.json({ installed: true, platform: 'darwin', version: null }))).toEqual({ installed: true, platform: 'darwin' });
  });
  test('offers an update for old installs without downgrading newer versions', () => {
    const installed = (version?: string) => ({ installed: true, platform: 'darwin', version });
    expect(liveAppUpdateAvailable(installed('0.1.0'))).toBe(true);
    expect(liveAppUpdateAvailable(installed('4.1.0'))).toBe(true);
    expect(liveAppUpdateAvailable(installed(`${GLOWBOM_LIVE_VERSION}-beta.1`))).toBe(true);
    expect(liveAppUpdateAvailable(installed(GLOWBOM_LIVE_VERSION))).toBe(false);
    expect(liveAppUpdateAvailable(installed(`${GLOWBOM_LIVE_VERSION}+build.2`))).toBe(false);
    expect(liveAppUpdateAvailable(installed('4.10.0'))).toBe(false);
    expect(liveAppUpdateAvailable(installed('5.0.0'))).toBe(false);
    expect(liveAppUpdateAvailable(installed())).toBe(false);
    expect(liveAppUpdateAvailable(installed('not a version'))).toBe(false);
    expect(liveAppUpdateAvailable({ ...installed('0.1.0'), installed: false })).toBe(false);
  });
  test('accepts installed and absent states from the local backend', async () => {
    expect(await readLiveAppStatus(Response.json({ installed: true, platform: 'darwin' }))).toEqual({ installed: true, platform: 'darwin' });
    expect(await readLiveAppStatus(Response.json({ installed: false, platform: 'linux' }))).toEqual({ installed: false, platform: 'linux' });
  });
  test('does not infer installation from missing or invalid status', async () => {
    await expect(readLiveAppStatus(Response.json({ installed: 'yes', platform: 'darwin' }))).rejects.toThrow('Could not check');
    await expect(readLiveAppStatus(new Response('not found', { status: 404 }))).rejects.toThrow('Restart Glowbom');
  });
  test('distinguishes a missing backend feature and denied access from an absent app', async () => {
    await expect(readLiveAppStatus(new Response('old server', { status: 405 }))).rejects.toMatchObject({ code: 'backend_outdated' });
    await expect(readLiveAppStatus(new Response('private details', { status: 403 }))).rejects.toMatchObject({ code: 'auth_required' });
    await expect(readLiveAppStatus(new Response('private details', { status: 503 }))).rejects.toThrow('Could not check whether Glowbom Live is installed. Try again.');
  });
  test('requires explicit launch success', async () => {
    await expect(readLiveAppLaunch(Response.json({ success: true }))).resolves.toBeUndefined();
    await expect(readLiveAppLaunch(Response.json({}))).rejects.toThrow('Could not open Glowbom Live. Try again.');
  });
  test('returns safe fixed errors without exposing backend details', async () => {
    await expect(readLiveAppLaunch(Response.json({ error: 'token=secret at /Users/private', code: 'launch_failed' }, { status: 503 }))).rejects.toThrow('Could not open Glowbom Live. Try again.');
    await expect(readLiveAppLaunch(new Response('<html>secret</html>', { status: 401 }))).rejects.toThrow('Local app access was denied.');
    await expect(readLiveAppLaunch(Response.json({ error: 'secret', code: 'already_running' }, { status: 409 }))).rejects.toThrow('Glowbom Live is already open.');
  });
  test('identifies a removed app so the UI can offer a download', async () => {
    try {
      await readLiveAppLaunch(Response.json({ code: 'not_installed' }, { status: 404 }));
      throw new Error('Launch should fail');
    } catch (error) {
      expect(error).toBeInstanceOf(LiveAppLaunchError);
      expect((error as LiveAppLaunchError).code).toBe('not_installed');
    }
    await expect(readLiveAppLaunch(new Response('not found', { status: 404 }))).rejects.toThrow('Restart Glowbom after any active builds finish');
  });
});
