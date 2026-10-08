import { afterEach, expect, test } from 'bun:test';
import { loadMediaApprovalImageSources, loadMediaApprovalVideoSources } from '../src/lib/media-approval-sources';
import { mediaApprovalSourceError } from '../src/lib/media-approval';
import { studioImageSources, type StudioImageSource } from '../src/lib/studio-image-settings';
import { studioVideoModels, type StudioVideoSource } from '../src/lib/studio-video';
import { loadStudioProviderKey, rememberStudioProviderKey } from '../src/lib/studio-providers';
import { readIconApiKeys } from '../src/lib/project-icon';
import type { OpenCodeMediaApprovalItem, ProjectIconSource } from '../src/types/opencode';

const originalFetch = globalThis.fetch;
afterEach(async () => {
  globalThis.fetch = (async () => Response.json({ configured: false, saved: false })) as typeof fetch;
  for (const source of ['openai-api', 'gemini-api', 'xai-api']) { rememberStudioProviderKey(source, ''); await loadStudioProviderKey(source); }
  globalThis.fetch = originalFetch;
});
const lockedStore = () => new Response('Could not access the system credential store.', { status: 503 });
function imageSource(id: string, connected = false): StudioImageSource {
  return { ...studioImageSources.find((source) => source.id === id)!, connected };
}
function iconSource(id: string): ProjectIconSource {
  return { id, label: id, model: '', available: false, authType: id.endsWith('-subscription') ? 'subscription' : 'api-key' };
}
function imageItem(id: string): OpenCodeMediaApprovalItem {
  return { id: 'image-row', mediaType: 'image', prompt: 'Garden', provider: id, sourceId: id };
}
function mockImages(capabilities: StudioImageSource[], keyResponse: (source: string) => Response = lockedStore) {
  globalThis.fetch = (async (url) => {
    const path = String(url);
    if (path === '/api/opencode/project/icon/sources') return Response.json({ sources: capabilities.map((source) => iconSource(source.id)), recommendedSource: '' });
    if (path === '/api/studio/images/capabilities') return Response.json({ sources: capabilities });
    if (path.startsWith('/api/studio/providers/key?')) return keyResponse(new URL(path, 'http://localhost').searchParams.get('sourceId') || '');
    throw new Error(`Unexpected request: ${path}`);
  }) as typeof fetch;
}
function videoSource(id: StudioVideoSource['id'], connected = false): StudioVideoSource {
  return { id, name: id, provider: id === 'veo-api' ? 'Google' : 'SpaceXAI', connected, requiresApiKey: id !== 'xai-subscription', models: studioVideoModels.filter((model) => model.sourceId === id) };
}
function videoItem(id: StudioVideoSource['id']): OpenCodeMediaApprovalItem {
  return { id: 'video-row', mediaType: 'video', prompt: 'Garden in motion', provider: id, sourceId: id };
}

test('a locked secure store does not block an explicit image key or discard model capabilities', async () => {
  rememberStudioProviderKey('openai-api', 'session-image-key');
  mockImages([imageSource('openai-api'), imageSource('gemini-api')]);
  const loaded = await loadMediaApprovalImageSources();
  expect(loaded.imageSources[0]?.models[0]?.id).toBe('gpt-image-2.5-flare');
  expect(mediaApprovalSourceError(imageItem('openai-api'), loaded.sources, readIconApiKeys(), true)).toBe('');
});

test('a locked secure store retains environment image keys and connected subscriptions', async () => {
  mockImages([imageSource('openai-api', true), imageSource('openai-subscription', true), imageSource('xai-api')]);
  const loaded = await loadMediaApprovalImageSources();
  for (const id of ['openai-api', 'openai-subscription']) {
    expect(loaded.sources.find((source) => source.id === id)?.available).toBe(true);
    expect(mediaApprovalSourceError(imageItem(id), loaded.sources, {}, true)).toBe('');
  }
  expect(mediaApprovalSourceError(imageItem('xai-api'), loaded.sources, {}, true)).toContain('API key');
});

test('a failed unrelated key check does not erase a successfully configured image key', async () => {
  mockImages([imageSource('gemini-api'), imageSource('openai-api')], (source) => source === 'gemini-api' ? Response.json({ configured: true, saved: true }) : lockedStore());
  const loaded = await loadMediaApprovalImageSources();
  expect(loaded.imageSources.find((source) => source.id === 'gemini-api')?.connected).toBe(true);
  expect(mediaApprovalSourceError(imageItem('gemini-api'), loaded.sources, {}, true)).toBe('');
});

test('a locked secure store retains explicit video keys, environment connections, and subscriptions', async () => {
  rememberStudioProviderKey('veo-api', 'session-video-key');
  globalThis.fetch = (async (url) => String(url) === '/api/studio/videos/capabilities'
    ? Response.json({ sources: [videoSource('veo-api'), videoSource('xai-api', true), videoSource('xai-subscription', true)] }) : lockedStore()) as typeof fetch;
  const sources = await loadMediaApprovalVideoSources();
  for (const id of ['veo-api', 'xai-api', 'xai-subscription'] as const) expect(mediaApprovalSourceError(videoItem(id), [], {}, true, sources)).toBe('');
});

test('required image and video catalogue failures remain errors even with an explicit key', async () => {
  rememberStudioProviderKey('openai-api', 'session-image-key');
  globalThis.fetch = (async (url) => String(url) === '/api/opencode/project/icon/sources'
    ? Response.json({ sources: [iconSource('openai-api')], recommendedSource: '' }) : new Response('Catalogue unavailable', { status: 503 })) as typeof fetch;
  await expect(loadMediaApprovalImageSources()).rejects.toThrow('Catalogue unavailable');
  await expect(loadMediaApprovalVideoSources()).rejects.toThrow('Catalogue unavailable');
});

test('cancelled optional checks cannot return a usable source snapshot', async () => {
  const controller = new AbortController();
  mockImages([imageSource('openai-api', true)], () => { controller.abort(); return lockedStore(); });
  await expect(loadMediaApprovalImageSources(controller.signal)).rejects.toThrow('Source loading was cancelled.');
});
