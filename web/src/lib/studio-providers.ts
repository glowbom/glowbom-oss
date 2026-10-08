import { withServerAuthHeaders } from './server-auth';
import { studioRequestDeadline } from './studio-deadline';
import { notifyImageSettingsChanged, readIconApiKeys, readImageSessionKey, rememberImageSessionKey } from './project-icon';

export type StudioAPIProvider = 'openai-api' | 'gemini-api' | 'xai-api';
export interface StudioProviderKeyStatus { configured: boolean; saved: boolean }
const savedProviderKeys = new Map<StudioAPIProvider, boolean>();

export function studioProviderKeyIsSaved(sourceId: string): boolean {
  const provider = studioProviderKeySource(sourceId);
  return !!provider && savedProviderKeys.get(provider) === true;
}

export function studioProviderKeySource(sourceId: string): StudioAPIProvider | undefined {
  if (sourceId === 'veo-api') return 'gemini-api';
  return sourceId === 'openai-api' || sourceId === 'gemini-api' || sourceId === 'xai-api' ? sourceId : undefined;
}

export function studioProviderName(sourceId: string): string {
  const names: Record<string, string> = { 'openai-api': 'OpenAI API', 'gemini-api': 'Google API', 'veo-api': 'Google API', 'xai-api': 'SpaceXAI API', 'openai-subscription': 'ChatGPT subscription', 'xai-subscription': 'Grok subscription', 'glowbom-api': 'Glowbom account' };
  return names[sourceId] || sourceId;
}

export function studioProviderKeyLabel(sourceId: string): string {
  return `${studioProviderName(sourceId).replace(/ API$/, '')} API key`;
}

export function rememberStudioProviderKey(sourceId: string, key: string): void {
  const provider = studioProviderKeySource(sourceId);
  if (provider) rememberImageSessionKey(provider, key);
}

export function studioProviderCredentials(sourceId: string): { apiKey?: string; useSavedKey?: boolean } {
  const provider = studioProviderKeySource(sourceId);
  if (!provider) return {};
  const sessionKey = readImageSessionKey(provider).trim();
  if (sessionKey) return { apiKey: sessionKey };
  if (savedProviderKeys.get(provider)) return { useSavedKey: true };
  const apiKey = readIconApiKeys()[provider]?.trim();
  return apiKey ? { apiKey } : {};
}

async function providerKeyRequest(sourceId: string, method: 'GET' | 'POST' | 'DELETE', key?: string, signal?: AbortSignal): Promise<StudioProviderKeyStatus> {
  const provider = studioProviderKeySource(sourceId);
  if (!provider) throw new Error('Choose an API provider to manage its key.');
  return studioRequestDeadline(async (boundedSignal) => {
    async function send(path: string, requestSource: string) {
      return fetch(`${path}${method === 'POST' ? '' : `?sourceId=${requestSource}`}`, {
        method, signal: boundedSignal, cache: 'no-store', headers: withServerAuthHeaders(method === 'POST' ? { 'Content-Type': 'application/json' } : undefined),
        ...(method === 'POST' ? { body: JSON.stringify({ sourceId: requestSource, key: key?.trim() }) } : {}),
      });
    }
    let response = await send('/api/studio/providers/key', provider);
    // Older local servers expose the same secure store through the video route.
    if (response.status === 404 && provider !== 'openai-api') response = await send('/api/studio/videos/key', provider === 'gemini-api' ? 'veo-api' : provider);
    if (!response.ok) {
      let message = (await response.text()).trim().slice(0, 2000) || 'Could not update the provider key.';
      if (key?.trim()) message = message.split(key.trim()).join('[redacted]');
      throw new Error(message);
    }
    const result = await response.json() as { configured?: boolean; saved?: boolean };
    return { configured: result.configured === true, saved: result.saved ?? result.configured === true };
  }, 30000, 'Checking the provider key took too long. Refresh and try again.', signal);
}

export async function loadStudioProviderKey(sourceId: string, signal?: AbortSignal): Promise<StudioProviderKeyStatus> {
  const result = await providerKeyRequest(sourceId, 'GET', undefined, signal), provider = studioProviderKeySource(sourceId)!;
  if (!signal?.aborted) savedProviderKeys.set(provider, result.saved);
  return result;
}

export async function saveStudioProviderKey(sourceId: string, key: string): Promise<void> {
  const result = await providerKeyRequest(sourceId, 'POST', key);
  if (!result.configured) throw new Error('Could not save this key securely. Unlock the system credential store and try again.');
  savedProviderKeys.set(studioProviderKeySource(sourceId)!, true);
  rememberStudioProviderKey(sourceId, ''); notifyImageSettingsChanged();
}

export async function removeStudioProviderKey(sourceId: string): Promise<void> {
  await providerKeyRequest(sourceId, 'DELETE');
  savedProviderKeys.delete(studioProviderKeySource(sourceId)!); notifyImageSettingsChanged();
}
