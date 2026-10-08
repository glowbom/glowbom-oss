import { withServerAuthHeaders } from './server-auth';
import { openCodeApi } from './api';
import type { ProjectIconSourcesResponse } from '../types/opencode';
import { studioRequestDeadline } from './studio-deadline';
import type { StudioVideoOptions } from './studio-video';

export interface StudioImage {
  id: string;
  timestamp: string;
  prompt: string;
  sourceService: string;
  assetType?: string;
  dimensions?: [number, number] | { width: number; height: number };
  duration?: number;
  sourceId?: string;
  modelId?: string;
  resolution?: string;
  quality?: string;
  requestedDurationSeconds?: number;
  sourceType?: string;
  sourceAssetID?: string;
  sourceProjectID?: string;
  usedInProjects?: string[];
}

export function readStudioImage(value: unknown): StudioImage | undefined {
  if (!value || typeof value !== 'object') return;
  const image = value as Record<string, unknown>;
  if (typeof image.id !== 'string' || !/^[A-Za-z0-9_-]{1,128}$/.test(image.id)) return;
  const result: StudioImage = {
    id: image.id, timestamp: typeof image.timestamp === 'string' ? image.timestamp.slice(0, 100) : '',
    prompt: typeof image.prompt === 'string' ? image.prompt.slice(0, 20000) : '',
    sourceService: typeof image.sourceService === 'string' ? image.sourceService.slice(0, 200) : '',
  };
  for (const key of ['assetType', 'sourceType', 'sourceAssetID', 'sourceProjectID', 'sourceId', 'modelId', 'resolution', 'quality'] as const) {
    if (typeof image[key] === 'string') result[key] = image[key].slice(0, 128);
  }
  if (Array.isArray(image.usedInProjects)) result.usedInProjects = image.usedInProjects.filter((id): id is string => typeof id === 'string').slice(0, 100).map((id) => id.slice(0, 128));
  if (typeof image.duration === 'number' && Number.isFinite(image.duration) && image.duration >= 0) result.duration = image.duration;
  if (typeof image.requestedDurationSeconds === 'number' && Number.isFinite(image.requestedDurationSeconds) && image.requestedDurationSeconds > 0) result.requestedDurationSeconds = image.requestedDurationSeconds;
  const dimensions = image.dimensions;
  const pair = Array.isArray(dimensions) ? dimensions : dimensions && typeof dimensions === 'object' ? [(dimensions as Record<string, unknown>).width, (dimensions as Record<string, unknown>).height] : [];
  if (pair.length === 2 && pair.every((edge) => typeof edge === 'number' && Number.isFinite(edge) && edge > 0 && edge <= 65536)) result.dimensions = pair as [number, number];
  return result;
}

export interface StudioProject {
  id: string;
  name: string;
  path?: string;
  timestamp: string;
  assetCount: number;
  available: boolean;
}

export interface StudioImageStatus {
  connected: boolean;
  credential: 'subscription' | 'api-key' | '';
  provider: string;
  sourceSelectionSupported?: boolean;
  grokSubscriptionMediaEnabled?: boolean;
  error?: string;
}

export interface StudioCatalogPage {
  images: StudioImage[];
  videos: StudioImage[];
  hasMoreImages: boolean;
  hasMoreVideos: boolean;
}

export interface StudioCatalogOptions {
  imageOffset?: number;
  imageLimit?: number;
  videoOffset?: number;
  videoLimit?: number;
  projectId?: string;
  signal?: AbortSignal;
}

class StudioRequestError extends Error {
  constructor(message: string, readonly status: number) { super(message); }
}

async function studioRequest<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(`/api/studio${path}`, {
    ...init,
    headers: withServerAuthHeaders(init?.headers),
  });
  if (!response.ok) {
    throw new StudioRequestError((await response.text()).trim() || `Studio request failed with HTTP ${response.status}`, response.status);
  }
  return response.json() as Promise<T>;
}

async function studioBlob(path: string, id: string, signal?: AbortSignal, missing = 'Could not load this Studio file.'): Promise<Blob> {
  const response = await fetch(`/api/studio${path}?id=${encodeURIComponent(id)}`, {
    signal,
    headers: withServerAuthHeaders(),
  });
  if (!response.ok) throw new Error(missing);
  return response.blob();
}

export async function loadStudioImages(): Promise<StudioImage[]> {
  const result = await studioRequest<{ images: StudioImage[] }>('/images');
  return result.images;
}

export async function loadStudioVideos(): Promise<StudioImage[]> {
  const result = await studioRequest<{ videos: StudioImage[] }>('/videos');
  return result.videos;
}

export async function loadStudioAssets(options: StudioCatalogOptions = {}): Promise<StudioCatalogPage> {
  const { signal, ...page } = options;
  const query = new URLSearchParams();
  Object.entries(page).forEach(([key, value]) => {
    if (value !== undefined) query.set(key, String(value));
  });
  const result = await studioRequest<Partial<StudioCatalogPage> & { projectsSupported?: boolean }>(
    `/assets${query.size > 0 ? `?${query}` : ''}`,
    { signal },
  );
  if (options.projectId && !result.projectsSupported) throw new Error('Restart Glowbom to view Studio assets by project.');
  return {
    images: result.images ?? [],
    videos: result.videos ?? [],
    hasMoreImages: Boolean(result.hasMoreImages),
    hasMoreVideos: Boolean(result.hasMoreVideos),
  };
}

export async function loadStudioProjects(signal?: AbortSignal, options: { metadataOnly?: boolean } = {}): Promise<{ projects: StudioProject[]; supported: boolean }> {
  try {
    const result = await studioRequest<{ projects: StudioProject[] }>(options.metadataOnly ? '/projects?metadataOnly=true' : '/projects', { signal });
    return { projects: result.projects || [], supported: true };
  } catch (error) {
    if (error instanceof StudioRequestError && [404, 405].includes(error.status)) return { projects: [], supported: false };
    throw error;
  }
}

export async function registerStudioProject(path: string, signal?: AbortSignal): Promise<StudioProject> {
  const result = await studioRequest<{ project: StudioProject }>('/projects', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ path }), signal,
  });
  return result.project;
}

export async function useStudioImageInProject(id: string, path: string, signal?: AbortSignal): Promise<{ image: StudioImage; project: StudioProject; relativePath: string }> {
  const result = await studioRequest<{ success: boolean; image: StudioImage; project: StudioProject; relativePath: string; error?: string }>('/images/use', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id, path }), signal,
  });
  if (!result.success) throw new Error(result.error || 'Could not copy this image into the project.');
  return result;
}

export function studioAssetProjectLinks(asset: StudioImage, projects: readonly StudioProject[]): { origin?: StudioProject; used: StudioProject[] } {
  const known = new Map(projects.map((project) => [project.id.toLowerCase(), project]));
  const resolve = (id: string): StudioProject => known.get(id.toLowerCase()) || { id, name: 'Unavailable project', timestamp: '', assetCount: 0, available: false };
  const ids = [...new Map((asset.usedInProjects || []).filter(Boolean).map((id) => [id.toLowerCase(), id])).values()];
  return { origin: asset.sourceProjectID ? resolve(asset.sourceProjectID) : undefined, used: ids.map(resolve) };
}

export function loadStudioImageStatus(signal?: AbortSignal): Promise<StudioImageStatus> {
  return studioRequest<StudioImageStatus>('/images/status', { signal });
}

export function loadStudioImageSources(signal?: AbortSignal): Promise<ProjectIconSourcesResponse> {
  return openCodeApi.getIconSources(signal);
}

export function loadStudioImageBlob(id: string, signal?: AbortSignal): Promise<Blob> {
  return studioBlob('/images/content', id, signal, 'Could not load this Studio image.');
}

export async function loadStudioAssetInfo(id: string, signal?: AbortSignal): Promise<StudioImage> {
  const result = await studioRequest<{ asset: StudioImage }>(`/assets/info?id=${encodeURIComponent(id)}`, { signal });
  return result.asset;
}

export function loadStudioVideoBlob(id: string, signal?: AbortSignal): Promise<Blob> {
  return studioBlob('/videos/content', id, signal, 'Could not load this Studio video.');
}

export async function generateStudioImage(prompt: string, aspectRatio: string, reference?: { id?: string; image?: string }, source?: { sourceId: string; apiKey?: string; modelId?: string; resolution?: string; quality?: string; useSavedKey?: boolean }, generationId?: string, signal?: AbortSignal): Promise<StudioImage> {
  const result = await studioRequest<{ image: StudioImage }>('/images/generate', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    signal,
    body: JSON.stringify({
      prompt,
      aspectRatio,
      referenceId: reference?.id || '',
      referenceImage: reference?.image || '',
      ...(generationId ? { generationId } : {}),
      ...(source ? { sourceId: source.sourceId, modelId: source.modelId, resolution: source.resolution, quality: source.quality,
        ...(['openai-api', 'gemini-api', 'xai-api'].includes(source.sourceId) ? source.apiKey?.trim() ? { apiKey: source.apiKey.trim() } : source.useSavedKey ? { useSavedKey: true } : {} : {}),
      } : {}),
    }),
  });
  return result.image;
}

export async function generateStudioVideo(
  prompt: string,
  aspectRatio: string,
  durationSeconds: number,
  reference: { id?: string; image?: string },
  generationId?: string,
  signal?: AbortSignal,
  options?: Partial<StudioVideoOptions> & { apiKey?: string; useSavedKey?: boolean },
): Promise<{ video: StudioImage; firstFrame?: StudioImage }> {
  return studioRequest<{ video: StudioImage; firstFrame?: StudioImage }>('/videos/generate', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    signal,
    body: JSON.stringify({
      prompt,
      aspectRatio,
      durationSeconds,
      referenceId: reference.id || '',
      referenceImage: reference.image || '',
      ...(generationId ? { generationId } : {}),
      ...(options ? { sourceId: options.sourceId, modelId: options.modelId, resolution: options.resolution, ...(options.apiKey?.trim() ? { apiKey: options.apiKey.trim() } : options.useSavedKey ? { useSavedKey: true } : {}) } : {}),
    }),
  });
}

export function cancelStudioGeneration(generationId: string, signal?: AbortSignal, timeoutMs = 15000): Promise<unknown> {
  return studioRequestDeadline((requestSignal) => studioRequest('/generation/cancel', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ generationId }), signal: requestSignal,
  }), timeoutMs, 'Could not confirm that this request stopped. Check the result and try Stop again.', signal);
}

export function resumeStudioGeneration(generationId: string, signal?: AbortSignal, credentials?: { apiKey?: string; useSavedKey?: boolean }): Promise<{ image?: StudioImage; video?: StudioImage; firstFrame?: StudioImage }> {
  return studioRequest('/generation/resume', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ generationId, ...(credentials?.apiKey?.trim() ? { apiKey: credentials.apiKey.trim() } : credentials?.useSavedKey ? { useSavedKey: true } : {}) }), signal,
  });
}
