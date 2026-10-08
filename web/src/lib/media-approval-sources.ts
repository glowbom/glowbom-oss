import { openCodeApi } from './api';
import { loadStudioImageCapabilities, type StudioImageSource } from './studio-image-settings';
import { loadStudioProviderKey, studioProviderKeySource } from './studio-providers';
import { loadStudioVideoKey, loadStudioVideoSources, type StudioVideoSource } from './studio-video';
import type { ProjectIconSource } from '../types/opencode';

export async function loadMediaApprovalImageSources(signal?: AbortSignal): Promise<{ sources: ProjectIconSource[]; imageSources: StudioImageSource[] }> {
  const [response, capabilities] = await Promise.all([openCodeApi.getIconSources(signal), loadStudioImageCapabilities(signal)]);
  const imageSources = capabilities.map((source) => ({ ...source }));
  const checks = await Promise.allSettled(imageSources.filter((source) => studioProviderKeySource(source.id)).map(async (source) => ({ id: source.id, status: await loadStudioProviderKey(source.id, signal) })));
  if (signal?.aborted) throw new DOMException('Source loading was cancelled.', 'AbortError');
  for (const check of checks) {
    // Secure storage is optional for explicit keys, environment keys, and account connections.
    if (check.status !== 'fulfilled' || !check.value.status.configured) continue;
    const source = imageSources.find((candidate) => candidate.id === check.value.id);
    if (source) source.connected = true;
  }
  return { imageSources, sources: response.sources.map((source) => ({ ...source, available: source.available || imageSources.some((candidate) => candidate.id === source.id && candidate.connected) })) };
}

export async function loadMediaApprovalVideoSources(signal?: AbortSignal): Promise<StudioVideoSource[]> {
  const sources = (await loadStudioVideoSources(signal)).map((source) => ({ ...source }));
  const checks = await Promise.allSettled(sources.filter((source) => source.requiresApiKey).map(async (source) => ({ id: source.id, saved: await loadStudioVideoKey(source.id, signal) })));
  if (signal?.aborted) throw new DOMException('Source loading was cancelled.', 'AbortError');
  for (const check of checks) {
    if (check.status !== 'fulfilled' || !check.value.saved) continue;
    const source = sources.find((candidate) => candidate.id === check.value.id);
    if (source) source.connected = true;
  }
  return sources;
}
