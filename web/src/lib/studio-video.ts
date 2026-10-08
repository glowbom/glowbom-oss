import { withServerAuthHeaders } from './server-auth';
import { studioRequestDeadline } from './studio-deadline';
import { loadStudioProviderKey, rememberStudioProviderKey, removeStudioProviderKey, saveStudioProviderKey, studioProviderCredentials, studioProviderKeySource } from './studio-providers';

export type StudioVideoSourceID = 'xai-api' | 'veo-api' | 'xai-subscription';
export interface StudioVideoOptions { sourceId: StudioVideoSourceID; modelId: string; resolution: string; durationSeconds: number; aspectRatio: string }
export interface StudioVideoModel {
  id: string; name: string; sourceId: StudioVideoSourceID; resolutions: readonly string[]; aspectRatios: readonly string[];
  defaultDurationSeconds: number; defaultResolution: string; minDurationSeconds: number; maxDurationSeconds: number;
  durations?: readonly number[]; pricesPerSecondUSD: Readonly<Record<string, number>>; inputImageUSD?: number; pricingURL: string;
}
export interface StudioVideoSource { id: StudioVideoSourceID; name: string; provider: string; experimental?: boolean; connected: boolean; requiresApiKey: boolean; models: StudioVideoModel[] }
export const studioVideoPriceCheckedOn = '2026-10-01';
const grokRatios = ['16:9', '9:16', '1:1'] as const;
const googlePricing = 'https://ai.google.dev/gemini-api/docs/pricing';
const grokDurations = { minDurationSeconds: 1, maxDurationSeconds: 15, defaultDurationSeconds: 5, defaultResolution: '480p', aspectRatios: grokRatios };
const veoDurations = { minDurationSeconds: 4, maxDurationSeconds: 8, durations: [4, 6, 8], defaultDurationSeconds: 4, defaultResolution: '720p', aspectRatios: ['16:9', '9:16'], resolutions: ['720p', '1080p'], pricingURL: googlePricing };

export const studioVideoModels: readonly StudioVideoModel[] = [
  { ...grokDurations, sourceId: 'xai-api', id: 'grok-imagine-video-1.5', name: 'Grok Imagine Video 1.5', resolutions: ['480p', '720p', '1080p'], pricesPerSecondUSD: { '480p': 0.08, '720p': 0.14, '1080p': 0.25 }, inputImageUSD: 0.01, pricingURL: 'https://docs.x.ai/developers/models/grok-imagine-video-1.5' },
  { ...grokDurations, sourceId: 'xai-api', id: 'grok-imagine-video', name: 'Grok Imagine Video', resolutions: ['480p', '720p'], pricesPerSecondUSD: { '480p': 0.05, '720p': 0.07 }, inputImageUSD: 0.002, pricingURL: 'https://docs.x.ai/developers/models/grok-imagine-video' },
  { ...veoDurations, sourceId: 'veo-api', id: 'veo-3.1-lite-generate-preview', name: 'Veo 3.1 Lite', pricesPerSecondUSD: { '720p': 0.05, '1080p': 0.08 } },
  { ...veoDurations, sourceId: 'veo-api', id: 'veo-3.1-fast-generate-preview', name: 'Veo 3.1 Fast', pricesPerSecondUSD: { '720p': 0.10, '1080p': 0.12 } },
  { ...veoDurations, sourceId: 'veo-api', id: 'veo-3.1-generate-preview', name: 'Veo 3.1 Standard', pricesPerSecondUSD: { '720p': 0.40, '1080p': 0.40 } },
  { ...grokDurations, sourceId: 'xai-subscription', id: 'grok-imagine-video', name: 'Grok Imagine Video', resolutions: ['480p', '720p'], pricesPerSecondUSD: {}, pricingURL: 'https://grok.com' },
  { ...grokDurations, sourceId: 'xai-subscription', id: 'grok-imagine-video-1.5', name: 'Grok Imagine Video 1.5', resolutions: ['480p', '720p', '1080p'], pricesPerSecondUSD: {}, pricingURL: 'https://grok.com' },
];
export const studioVideoSources = [
  { id: 'xai-api', name: 'SpaceXAI API', provider: 'SpaceXAI', requiresApiKey: true },
  { id: 'veo-api', name: 'Google API', provider: 'Google', requiresApiKey: true },
  { id: 'xai-subscription', name: 'Grok subscription', provider: 'SpaceXAI', requiresApiKey: false },
] as const;

export function studioVideoModel(sourceId: string, modelId = ''): StudioVideoModel {
  return studioVideoModels.find((model) => model.sourceId === sourceId && model.id === modelId)
    || studioVideoModels.find((model) => model.sourceId === sourceId) || studioVideoModels[0]!;
}
export function studioVideoModelName(sourceId: string, modelId: string): string {
  return studioVideoModels.find((model) => model.sourceId === sourceId && model.id === modelId)?.name || modelId;
}
export function studioVideoDurations(options: Pick<StudioVideoOptions, 'sourceId' | 'modelId' | 'resolution'>): readonly number[] {
  const model = studioVideoModel(options.sourceId, options.modelId);
  if (options.sourceId === 'veo-api' && options.resolution !== '720p') return [8];
  return model.durations || Array.from({ length: model.maxDurationSeconds - model.minDurationSeconds + 1 }, (_, index) => index + model.minDurationSeconds);
}
export function normalizeStudioVideoOptions(value: Partial<StudioVideoOptions>): StudioVideoOptions {
  const sourceId = studioVideoSources.some((source) => source.id === value.sourceId) ? value.sourceId! : 'xai-api';
  const model = studioVideoModel(sourceId, value.modelId);
  const options: StudioVideoOptions = { sourceId, modelId: model.id, resolution: model.resolutions.includes(value.resolution || '') ? value.resolution! : model.defaultResolution, durationSeconds: value.durationSeconds || model.defaultDurationSeconds, aspectRatio: model.aspectRatios.includes(value.aspectRatio || '') ? value.aspectRatio! : '16:9' };
  const durations = studioVideoDurations(options);
  if (!durations.includes(options.durationSeconds)) options.durationSeconds = durations.includes(model.defaultDurationSeconds) ? model.defaultDurationSeconds : durations[0]!;
  return options;
}
export function studioVideoOptionError(options: StudioVideoOptions): string {
  const model = studioVideoModels.find((candidate) => candidate.sourceId === options.sourceId && candidate.id === options.modelId);
  if (!model) return 'Choose a supported video model.';
  if (!model.resolutions.includes(options.resolution)) return 'Choose a resolution supported by this video model.';
  if (!studioVideoDurations(options).includes(options.durationSeconds)) return 'Choose a length supported by this video model and resolution.';
  if (!model.aspectRatios.includes(options.aspectRatio)) return 'Choose a shape supported by this video model.';
  return '';
}
export function studioVideoEstimate(options: StudioVideoOptions, hasReference = false): { usd?: number; label: string; pricingURL: string; checkedOn: string } {
  const model = studioVideoModel(options.sourceId, options.modelId), pricingURL = model.pricingURL, checkedOn = studioVideoPriceCheckedOn;
  if (options.sourceId === 'xai-subscription') return { label: 'Account allowance or credits; check your Grok usage', pricingURL, checkedOn };
  const rate = model.pricesPerSecondUSD[options.resolution];
  if (rate === undefined || studioVideoOptionError(options)) return { label: 'Choose valid video settings to estimate cost', pricingURL, checkedOn };
  const usd = Number((rate * options.durationSeconds + (hasReference ? model.inputImageUSD || 0 : 0)).toFixed(4));
  return { usd, label: `About $${usd.toFixed(usd < 0.01 ? 3 : 2)} per video`, pricingURL, checkedOn };
}

export function studioVideoKeySource(sourceId: StudioVideoSourceID): string { return studioProviderKeySource(sourceId) || sourceId; }
export function rememberStudioVideoKey(sourceId: StudioVideoSourceID, key: string) { rememberStudioProviderKey(sourceId, key); }
export function studioVideoCredentials(sourceId: StudioVideoSourceID): { apiKey?: string; useSavedKey?: boolean } {
  return studioProviderCredentials(sourceId);
}
async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  return studioRequestDeadline(async (signal) => {
    const response = await fetch(`/api/studio/videos${path}`, { ...init, signal, headers: withServerAuthHeaders(init.headers) });
    if (!response.ok) throw new Error((await response.text()).trim().slice(0, 2000) || 'Could not check video settings.');
    return response.json() as Promise<T>;
  }, 30000, 'Video settings took too long to respond. Refresh and try again.', init.signal || undefined);
}
export async function loadStudioVideoSources(signal?: AbortSignal): Promise<StudioVideoSource[]> {
  const result = await request<{ sources: StudioVideoSource[] }>('/capabilities', { signal, cache: 'no-store' });
  if (!Array.isArray(result.sources)) throw new Error('Restart Glowbom to enable video sources.');
  return result.sources;
}
export async function loadStudioVideoKey(sourceId: StudioVideoSourceID, signal?: AbortSignal): Promise<boolean> {
  return (await loadStudioProviderKey(sourceId, signal)).saved;
}
export async function saveStudioVideoKey(sourceId: StudioVideoSourceID, key: string): Promise<void> {
  return saveStudioProviderKey(sourceId, key);
}
export async function removeStudioVideoKey(sourceId: StudioVideoSourceID): Promise<void> {
  return removeStudioProviderKey(sourceId);
}
