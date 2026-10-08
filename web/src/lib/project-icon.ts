import type { ProjectIconSource } from '../types/opencode';
import { openCodeApi, toErrorMessage } from './api';

type StoredSettings = Pick<Storage, 'getItem'>;
export type ProjectSetupMode = 'build' | 'edit';

export const imageSettingsChangedEvent = 'glowbom-image-settings-changed';
const sessionImageKeys: Record<string, string> = {};

export function notifyImageSettingsChanged() {
  if (typeof window !== 'undefined') window.dispatchEvent(new Event(imageSettingsChangedEvent));
}

export function rememberImageSessionKey(sourceId: string, value: string) {
  if (!['openai-api', 'gemini-api', 'xai-api'].includes(sourceId)) return;
  const key = sourceId === 'openai-api' ? platformOpenAIImageKey(value) : value.trim();
  if (key) sessionImageKeys[sourceId] = key;
  else delete sessionImageKeys[sourceId];
  notifyImageSettingsChanged();
}

export function readImageSessionKey(sourceId: string): string {
  return sessionImageKeys[sourceId] || '';
}

export async function loadProjectSetup(path: string, mode: ProjectSetupMode, signal: AbortSignal): Promise<{ showDialog: boolean; image?: string; error?: string } | null> {
  try {
    const icon = await openCodeApi.getProjectIcon(path, signal);
    if (signal.aborted) return null;
    if (!icon.success) throw new Error(icon.error || 'Could not check the project icon.');
    return { showDialog: mode === 'edit' || !(icon.exists || skippedProjectIcon(path)), image: icon.image };
  } catch (error) {
    if (signal.aborted) return null;
    if (mode === 'edit') return { showDialog: true, error: `${toErrorMessage(error, 'Could not load the current icon.')} You can still change the project name.` };
    throw error;
  }
}

export function platformOpenAIImageKey(value: unknown): string {
  if (typeof value !== 'string') return '';
  const key = value.trim();
  // ChatGPT OAuth tokens are JWTs and cannot authenticate platform image requests.
  return /^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/.test(key) ? '' : key;
}

export function readIconApiKeys(storage?: StoredSettings): Record<string, string> {
  try {
    const settings = storage || localStorage;
    const read = (key: string, legacy: string): Record<string, unknown> => {
      try { return JSON.parse(settings.getItem(key) ?? settings.getItem(legacy) ?? '{}') || {}; } catch { return {}; }
    };
    const imageSettings = read('glowbom_oss_image_provider_keys', 'glowby_oss_image_provider_keys');
    const providerSettings = read('glowbom_oss_provider_keys', 'glowby_oss_provider_keys');
    const keys = Object.fromEntries(['openai', 'gemini', 'xai'].map((provider) => {
      const generalKey = provider === 'openai' && providerSettings.openaiAuthMode === 'codex-jwt' ? '' : providerSettings[`${provider}Key`];
      const candidates = [imageSettings[`${provider}ImageKey`], generalKey]
        .map(value => provider === 'openai' ? platformOpenAIImageKey(value) : value);
      const key = candidates.find((value) => typeof value === 'string' && value.trim());
      return [`${provider}-api`, typeof key === 'string' ? key.trim() : ''];
    }));
    return storage ? keys : { ...keys, ...sessionImageKeys };
  } catch { return storage ? {} : { ...sessionImageKeys }; }
}

export function preferredIconSource(sources: ProjectIconSource[], keys: Record<string, string>, recommended = ''): string {
  const available = (source: ProjectIconSource) => source.available || (source.authType === 'api-key' && !!keys[source.id]);
  return sources.find((source) => source.authType === 'subscription' && source.available)?.id
    || sources.find((source) => source.id === recommended && available(source))?.id
    || sources.find(available)?.id
    || sources.find((source) => source.authType === 'api-key')?.id || '';
}

export function imageSourceAccessLabel(source: ProjectIconSource, keys: Record<string, string> = {}): string {
  if (source.authType === 'account') {
    if (source.available) return 'Signed in';
    if (imageSourceNeedsSignIn(source)) return 'Sign in to Glowbom';
    if (source.availabilityCode === 'account_busy') return 'Checking account';
    if (source.availabilityCode === 'cli_unavailable' || source.availabilityCode === 'cli_update_required') return 'Update required';
    return 'Check connection';
  }
  if (source.authType === 'subscription') {
    if (source.available) return 'Subscription';
    if (source.availabilityCode === 'codex_cli') return 'Install Codex';
    if (source.availabilityCode === 'codex_login') return 'Sign in to Codex';
    if (source.availabilityCode?.startsWith('codex_')) return 'Check Codex';
    return 'Not connected';
  }
  return source.available || keys[source.id] ? 'Configured API key' : 'Add API key';
}

export function imageSourceName(source: ProjectIconSource): string {
  return source.id === 'glowbom-api' ? 'Glowbom account' : source.label;
}

export function imageSourceOptionLabel(source: ProjectIconSource, keys: Record<string, string> = {}): string {
  const name = imageSourceName(source);
  return source.id === 'glowbom-api' ? name : `${name} · ${imageSourceAccessLabel(source, keys)}`;
}

export function imageSourceNeedsSignIn(source: ProjectIconSource): boolean {
  return source.authType === 'account' && !source.available && source.availabilityCode === 'sign_in_required';
}

export function imageSourceHelp(source: ProjectIconSource): string {
  if (source.authType === 'account') {
    if (source.available) return 'Uses your Glowbom account allowance.';
    if (imageSourceNeedsSignIn(source)) return 'Sign in through Account to use your Glowbom allowance.';
    if (source.availabilityCode === 'account_busy') return 'Your Glowbom account is busy. Wait a moment, then refresh sources.';
    if (source.availabilityCode === 'cli_unavailable' || source.availabilityCode === 'cli_update_required') return 'Update or install the Glowbom CLI, then refresh sources.';
    if (source.availabilityCode === 'backend_auth_required') return 'Restart Glowbom with glowbom start to reconnect your account securely.';
    return 'Could not check your Glowbom account. Refresh sources or open Account to check the connection.';
  }
  if (source.authType === 'subscription') {
    if (source.id === 'openai-subscription') {
      if (source.available) return 'Uses your connected ChatGPT subscription. ChatGPT limits apply.';
      switch (source.availabilityCode) {
        case 'codex_cli': return 'Install Codex, then restart Glowbom and refresh image sources.';
        case 'codex_login': return 'Sign in with ChatGPT using codex login, then refresh image sources.';
        case 'codex_capability': return 'This Codex connection does not support image generation. Check Codex or choose another source.';
        case 'codex_transport': return 'Check the ChatGPT image transport setting in your backend environment, then restart Glowbom.';
      }
      if (source.availabilityCode?.startsWith('codex_')) return 'Could not check Codex. Check its connection, then refresh image sources.';
      return 'Connect ChatGPT through OpenCode, then refresh image sources.';
    }
    return source.available ? 'Uses your connected subscription. Account limits apply.'
      : 'Connect your subscription through OpenCode, then refresh image sources.';
  }
  return 'Uses an API key. Provider charges apply.';
}

export function projectIconPrompt(name: string, context: string): string {
  return `Create an app icon for ${name.trim() || 'this project'}. ${context.trim().slice(0, 1000)}\nUse one clear focal symbol, bold readable shapes, high contrast, and clean edges. No text or device mockup. Fill a square 1024 by 1024 composition.`;
}

export type IconReference = { name: string; image: string };

export async function readIconReference(file: File): Promise<IconReference> {
  if (!['image/png', 'image/jpeg', 'image/webp'].includes(file.type)) throw new Error('Choose a PNG, JPEG, or WebP photo.');
  if (!file.size || file.size > 10 * 1024 * 1024) throw new Error('Choose a photo smaller than 10 MB.');
  const url = URL.createObjectURL(file);
  const photo = new Image();
  try {
    photo.src = url;
    await photo.decode();
    if (!photo.naturalWidth || !photo.naturalHeight) throw new Error('Empty image');
    const scale = Math.min(1, 1536 / Math.max(photo.naturalWidth, photo.naturalHeight));
    const canvas = document.createElement('canvas');
    canvas.width = Math.max(1, Math.round(photo.naturalWidth * scale));
    canvas.height = Math.max(1, Math.round(photo.naturalHeight * scale));
    const context = canvas.getContext('2d');
    if (!context) throw new Error('Canvas unavailable');
    context.drawImage(photo, 0, 0, canvas.width, canvas.height);
    const image = canvas.toDataURL('image/png');
    if (!image.startsWith('data:image/png;base64,')) throw new Error('Image unavailable');
    return { name: file.name || 'Camera photo', image };
  } catch {
    throw new Error('Could not read that photo. Try another PNG, JPEG, or WebP image.');
  } finally { URL.revokeObjectURL(url); }
}

const skipStorageKey = 'glowbom_icon_setup_skipped';
const skippedThisSession = new Set<string>();

export function skippedProjectIcon(path: string): boolean {
  if (skippedThisSession.has(path)) return true;
  try {
    const paths: unknown = JSON.parse(localStorage.getItem(skipStorageKey) || '[]');
    return Array.isArray(paths) && paths.includes(path);
  } catch { return false; }
}

export function skipProjectIcon(path: string) {
  skippedThisSession.add(path);
  try {
    const paths: unknown = JSON.parse(localStorage.getItem(skipStorageKey) || '[]');
    const previous = Array.isArray(paths) ? paths.filter((value): value is string => typeof value === 'string' && value !== path) : [];
    localStorage.setItem(skipStorageKey, JSON.stringify([...previous.slice(-99), path]));
  } catch { /* Keep the choice for this session when local storage is unavailable. */ }
}
