import { expect, test } from 'bun:test';
import { mediaApprovalDraft, mediaApprovalGenerateError, mediaApprovalImageConnections, mediaApprovalImageOptions, mediaApprovalItemError, mediaApprovalReferencePatch, mediaApprovalSourceError, newMediaApprovalItem, normalizeMediaApproval, mediaApprovalVideoOptions, updateMediaVideoOptions, updateMediaImageOptions, updateMediaAudioType } from '../src/lib/media-approval';
import { normalizeStudioImageOptions } from '../src/lib/studio-image-settings';
import type { OpenCodeMediaApprovalItem, ProjectIconSource } from '../src/types/opencode';

const image: OpenCodeMediaApprovalItem = { id: 'asset-1', mediaType: 'image', prompt: 'Home page hero', provider: 'OpenAI', sourceId: 'openai-api', referenceImages: [] };

test('approval normalization retains stable identities and all generation edits across the stream', () => {
  const approval = normalizeMediaApproval({ id: 'approval-1', title: 'Create assets', message: 'Review these', items: [
    { ...image, placeholder: 'generate:Home page hero', referenceImages: ['data:image/png;base64,cGhvdG8='], aspectRatio: '4:3', usagePrompt: 'Home page background', excluded: true },
    { id: 'video-1', mediaType: 'video', prompt: 'Slow pan', provider: 'Veo', sourceId: 'veo-api', fromKey: 'asset-1', aspectRatio: '16:9' },
    { id: 'audio-1', mediaType: 'audio', prompt: 'Chime', provider: 'ElevenLabs', sourceId: 'elevenlabs-api', audioType: 'sound', voiceID: 'voice-1', modelID: 'model-1', durationSeconds: 5, promptInfluence: 0, loop: true, forceInstrumental: false },
  ] });
  expect(approval?.items[0]).toMatchObject({ id: image.id, placeholder: 'generate:Home page hero', excluded: true, referenceImages: ['data:image/png;base64,cGhvdG8='], aspectRatio: '4:3', usagePrompt: 'Home page background' });
  expect(approval?.items[1]).toMatchObject({ fromKey: 'asset-1', aspectRatio: '16:9' });
  expect(approval?.items[2]).toMatchObject({ voiceID: 'voice-1', modelID: 'model-1', durationSeconds: 5, promptInfluence: 0, loop: true });
});

test('older approvals receive stable row IDs without silently selecting a different source', () => {
  const raw = { id: 'legacy', items: [{ mediaType: 'image', prompt: 'A leaf', provider: 'OpenAI' }, null, { prompt: 'missing type' }] };
  const first = normalizeMediaApproval(raw);
  const second = normalizeMediaApproval(raw);
  expect(first?.items).toHaveLength(1);
  expect(first?.items[0]?.id).toBe(second?.items[0]?.id);
  expect(first?.items[0]?.sourceId).toBeUndefined();
  expect(mediaApprovalItemError(first!.items[0]!)).toContain('Choose a source');
  expect(normalizeMediaApproval({ items: raw.items })).toBeNull();
});

test('draft edits and reference removal do not mutate the approval or a submitted snapshot', () => {
  const original = { ...image, referenceImages: ['reference'] };
  const draft = mediaApprovalDraft([original]);
  const submitted = mediaApprovalDraft(draft);
  draft[0]!.referenceImages!.splice(0);
  draft[0]!.prompt = 'Changed prompt';
  expect(original.referenceImages).toEqual(['reference']);
  expect(submitted[0]?.referenceImages).toEqual(['reference']);
  expect(submitted[0]?.prompt).toBe(image.prompt);
  expect(draft[0]?.referenceImages).toEqual([]);
});

test('previous-image provenance survives streamed approvals, ordinary edits, and submission snapshots', () => {
  const previous = 'data:image/png;base64,cHJldmlvdXM=';
  const restored = normalizeMediaApproval({ id: 'reference-review', items: [{ ...image, referenceImages: [previous], referenceOrigin: 'previous-image' }] });
  expect(restored?.items[0]?.referenceOrigin).toBe('previous-image');
  const edited = mediaApprovalDraft(restored!.items);
  edited[0]!.prompt = 'Make the scene brighter';
  edited[0]!.sourceId = 'gemini-api';
  const submitted = mediaApprovalDraft(edited);
  expect(submitted[0]).toMatchObject({ referenceImages: [previous], referenceOrigin: 'previous-image', prompt: 'Make the scene brighter', sourceId: 'gemini-api' });
  expect(restored?.items[0]?.prompt).toBe(image.prompt);
  expect(restored?.items[0]?.referenceImages).not.toBe(edited[0]?.referenceImages);
  expect(normalizeMediaApproval({ id: 'unsupported-origin', items: [{ ...image, referenceOrigin: 'unverified-origin' }] })?.items[0]?.referenceOrigin).toBeUndefined();
});

test('replacing or removing a previous image clears its provenance and survives the submitted draft', () => {
  const previous: OpenCodeMediaApprovalItem = { ...image, referenceImages: ['data:image/png;base64,cHJldmlvdXM='], referenceOrigin: 'previous-image' };
  const replacement = 'data:image/png;base64,bmV3';
  const replaced = { ...previous, ...mediaApprovalReferencePatch(replacement) };
  expect(mediaApprovalDraft([replaced])[0]).toMatchObject({ referenceImages: [replacement], referenceOrigin: undefined });
  const removed = { ...previous, ...mediaApprovalReferencePatch() };
  expect(mediaApprovalDraft([removed])[0]).toMatchObject({ referenceImages: [], referenceOrigin: undefined });
  expect(previous.referenceOrigin).toBe('previous-image');
  expect(previous.referenceImages).toEqual(['data:image/png;base64,cHJldmlvdXM=']);
});

test('empty or malformed reference arrays cannot retain previous-image provenance', () => {
  for (const referenceImages of [undefined, null, [], [null, 7], ['', '   '], 'not-an-array']) {
    const normalized = normalizeMediaApproval({ id: 'empty-reference', items: [{ ...image, referenceImages, referenceOrigin: 'previous-image' }] });
    expect(normalized?.items[0]?.referenceOrigin).toBeUndefined();
  }
  for (const referenceImages of [undefined, [], ['']]) {
    const drafted = mediaApprovalDraft([{ ...image, referenceImages, referenceOrigin: 'previous-image' }]);
    expect(drafted[0]?.referenceOrigin).toBeUndefined();
    expect(drafted[0]?.referenceImages).toEqual(referenceImages || []);
  }
});

test('non-image origins are cleared while their provided reference pixels stay intact', () => {
  const pixels = 'data:image/png;base64,cHJldmlvdXM=';
  for (const mediaType of ['video', 'audio']) {
    const provided = { ...image, mediaType, referenceImages: [pixels], referenceOrigin: 'previous-image' as const };
    const normalized = normalizeMediaApproval({ id: 'non-image-reference', items: [provided] });
    const drafted = mediaApprovalDraft([provided]);
    expect(normalized?.items[0]?.referenceOrigin).toBeUndefined();
    expect(normalized?.items[0]?.referenceImages).toEqual([pixels]);
    expect(drafted[0]?.referenceOrigin).toBeUndefined();
    expect(drafted[0]?.referenceImages).toEqual([pixels]);
    expect(provided.referenceOrigin).toBe('previous-image');
  }
});

test('draft starting frames use stable image IDs before prompts are edited or images excluded', () => {
  const startingImage = { ...image, placeholder: 'generate:Home page hero' };
  for (const fromKey of [startingImage.prompt, startingImage.placeholder, startingImage.id]) {
    const video = { id: 'video-1', mediaType: 'video', prompt: 'Slow pan', provider: 'Veo', sourceId: 'veo-api', fromKey };
    const draft = mediaApprovalDraft([startingImage, video]);
    expect(draft[1]?.fromKey).toBe(startingImage.id);
    draft[0]!.prompt = 'Changed garden scene';
    expect(mediaApprovalGenerateError(draft)).toBe('');
    draft[0]!.excluded = true;
    expect(mediaApprovalGenerateError(draft)).toContain('starting image');
    expect(video.fromKey).toBe(fromKey);
  }
  const studioVideo = { id: 'video-2', mediaType: 'video', prompt: 'Slow pan', provider: 'Veo', sourceId: 'veo-api', fromKey: 'studio-image-123' };
  expect(mediaApprovalDraft([startingImage, studioVideo])[1]?.fromKey).toBe('studio-image-123');
});

test('image connection controls are shared once per selected provider including Veo’s Gemini key', () => {
  const api: ProjectIconSource = { id: 'openai-api', label: 'OpenAI', model: 'image', authType: 'api-key', available: false };
  const items = [image, { ...image, id: 'image-2' }, { ...image, id: 'excluded-xai', sourceId: 'xai-api', excluded: true }, { ...image, id: 'subscription', sourceId: 'openai-subscription' }, { id: 'video-1', mediaType: 'video', prompt: 'Pan', sourceId: 'veo-api', provider: 'Veo' }];
  expect(mediaApprovalImageConnections(items, [api]).map((source) => source.id)).toEqual(['openai-api', 'gemini-api']);
  expect(mediaApprovalImageConnections(items.map((item) => ({ ...item, excluded: true })), [api])).toEqual([]);
  expect(mediaApprovalImageConnections([image], [{ ...api, authType: 'subscription' }])).toEqual([]);
});

test('excluded invalid rows do not block valid assets, while all excluded requires skipping', () => {
  const invalid = { ...image, id: 'asset-2', prompt: '', excluded: true };
  expect(mediaApprovalGenerateError([image, invalid])).toBe('');
  expect(mediaApprovalGenerateError([{ ...image, excluded: true }, invalid])).toContain('Skip all');
  expect(mediaApprovalGenerateError([image, { ...invalid, excluded: false }])).toContain('Asset 2');
});

test('added assets require usage instructions and retain their chosen media kind', () => {
  for (const kind of ['image', 'video', 'audio'] as const) {
    const added = newMediaApprovalItem(kind, `added-${kind}`);
    expect(added.mediaType).toBe(kind);
    expect(added.id).toBe(`added-${kind}`);
    expect(mediaApprovalItemError({ ...added, prompt: 'Create the new asset' })).toContain('how the app should use');
  }
  const added = { ...newMediaApprovalItem('image', 'added-image'), prompt: 'Flower', sourceId: 'openai-api', usagePrompt: 'Show beside the welcome message' };
  expect(mediaApprovalGenerateError([image, added])).toBe('');
  expect(mediaApprovalItemError({ ...added, usagePrompt: '  ' })).toContain('how the app should use');
  expect(mediaApprovalItemError({ ...added, referenceImages: ['one', 'two'] })).toContain('one reference');
  expect(mediaApprovalItemError({ ...added, referenceImages: ['https://example.com/private-photo'] })).toContain('Upload');
});

test('source readiness uses centralized keys for that provider and never another provider', () => {
  const source: ProjectIconSource = { id: 'openai-api', label: 'OpenAI', model: 'image', authType: 'api-key', available: false };
  expect(mediaApprovalSourceError(image, [source], { 'gemini-api': 'unrelated-key' }, true)).toContain('Settings');
  expect(mediaApprovalSourceError(image, [source], { 'openai-api': 'selected-key' }, true)).toBe('');
  expect(mediaApprovalSourceError(image, [], {}, true)).toContain('unavailable');
  expect(mediaApprovalSourceError({ ...image, excluded: true }, [], {}, true)).toBe('');
  expect(mediaApprovalSourceError({ ...image, sourceId: 'openai-subscription' }, [{ ...source, id: 'openai-subscription', authType: 'subscription', availabilityCode: 'codex_login' }], {}, true)).toContain('codex login');
  expect(mediaApprovalItemError({ ...image, sourceId: 'glowbom-api', aspectRatio: '16:9' })).toContain('default image size');
});

test('video assets require a supported shape and included starting frame', () => {
  const video = mediaApprovalDraft([{ id: 'video-1', mediaType: 'video', prompt: 'Pan across a garden', provider: 'Veo', sourceId: 'veo-api', fromKey: image.id, aspectRatio: '16:9' }])[0]!;
  expect(mediaApprovalGenerateError([image, video])).toBe('');
  expect(mediaApprovalGenerateError([{ ...image, excluded: true }, video])).toContain('starting image');
  expect(mediaApprovalItemError({ ...video, fromKey: '' })).toContain('starting frame');
  expect(mediaApprovalItemError({ ...video, aspectRatio: '1:1' })).toContain('shape supported');
});

test('audio controls accept provider limits and remove irrelevant params when changing type', () => {
  const sound: OpenCodeMediaApprovalItem = { id: 'sound-1', mediaType: 'audio', prompt: 'A short chime', provider: 'ElevenLabs', sourceId: 'elevenlabs-api', audioType: 'sound', durationSeconds: 0.5, promptInfluence: 0, loop: true, modelID: 'eleven_text_to_sound_v2' };
  expect(mediaApprovalItemError(sound)).toBe('');
  expect(mediaApprovalItemError({ ...sound, durationSeconds: 31 })).toContain('0.5 to 30');
  expect(mediaApprovalItemError({ ...sound, promptInfluence: 1.1 })).toContain('between 0 and 1');
  expect(mediaApprovalItemError({ ...sound, durationSeconds: Number.NaN })).toContain('duration');
  expect(mediaApprovalItemError({ ...sound, modelID: 'custom-sound' })).toContain('Looping requires');
  const music = updateMediaAudioType(sound, 'music');
  expect(music).toMatchObject({ audioType: 'music', loop: false, forceInstrumental: false });
  expect(music.durationSeconds).toBe(30);
  expect(music.promptInfluence).toBeUndefined();
  expect(music.modelID).toBeUndefined();
  expect(mediaApprovalItemError({ ...music, durationSeconds: 600 })).toBe('');
  expect(mediaApprovalItemError({ ...music, durationSeconds: 2 })).toContain('3 to 600');
  expect(mediaApprovalItemError({ ...music, durationSeconds: undefined })).toContain('Choose a music duration');
  expect(mediaApprovalItemError({ ...music, durationSeconds: 0 })).toContain('3 to 600');
  expect(mediaApprovalItemError({ ...music, durationSeconds: Number.POSITIVE_INFINITY })).toContain('3 to 600');
});

test('music duration is explicit in restored approval and submission snapshots', () => {
  const item: OpenCodeMediaApprovalItem = { id: 'music-1', mediaType: 'audio', prompt: 'Soft piano', provider: 'ElevenLabs', sourceId: 'elevenlabs-api', audioType: 'music', forceInstrumental: true };
  for (const durationSeconds of [undefined, 0]) {
    const restored = normalizeMediaApproval({ id: 'music-review', items: [{ ...item, durationSeconds }] });
    expect(restored?.items[0]?.durationSeconds).toBe(30);
    expect(mediaApprovalDraft([{ ...item, durationSeconds }])[0]?.durationSeconds).toBe(30);
  }
  const edited = { ...item, durationSeconds: 12.5 };
  const snapshot = mediaApprovalDraft([edited]);
  const restored = normalizeMediaApproval({ id: 'music-review', items: snapshot });
  expect(restored?.items[0]).toMatchObject({ durationSeconds: 12.5, forceInstrumental: true });
  expect(item.durationSeconds).toBeUndefined();
  expect(mediaApprovalDraft([{ ...item, durationSeconds: -1 }])[0]?.durationSeconds).toBe(-1);
});


test('video review defaults are short and preserves all selected generation settings', () => {
  const raw: OpenCodeMediaApprovalItem = { id: 'video-review', mediaType: 'video', prompt: 'A river moves', provider: 'Google', sourceId: 'veo-api', fromKey: 'river-still' };
  expect(mediaApprovalDraft([raw])[0]).toMatchObject({ modelID: 'veo-3.1-lite-generate-preview', durationSeconds: 4, resolution: '720p' });
  const edited = { ...raw, sourceId: 'xai-api', modelID: 'grok-imagine-video-1.5', durationSeconds: 3, resolution: '1080p', aspectRatio: '1:1' };
  const restored = normalizeMediaApproval({ id: 'edited-video', items: [edited] });
  expect(restored?.items[0]).toMatchObject(edited);
  expect(mediaApprovalItemError(restored!.items[0]!)).toBe('');
  expect(raw.durationSeconds).toBeUndefined();
});

test('video review rejects unsupported lengths and explicitly adjusts Google 1080p', () => {
  const item: OpenCodeMediaApprovalItem = { id: 'video-1', mediaType: 'video', prompt: 'River', provider: 'Google', sourceId: 'veo-api', modelID: 'veo-3.1-fast-generate-preview', durationSeconds: 4, resolution: '720p', fromKey: 'frame' };
  for (const durationSeconds of [1, 5, 300, 4.5, Number.NaN]) expect(mediaApprovalItemError({ ...item, durationSeconds })).toContain('length');
  expect(mediaApprovalItemError({ ...item, resolution: '1080p' })).toContain('length');
  const changed = { ...item, ...updateMediaVideoOptions(item, { resolution: '1080p' }) };
  expect(mediaApprovalVideoOptions(changed)).toMatchObject({ durationSeconds: 8, resolution: '1080p', modelId: item.modelID });
  expect(mediaApprovalItemError(changed)).toBe('');
  expect(mediaApprovalItemError({ ...item, modelID: 'unknown-model' })).toContain('supported video model');
});

test('image review preserves exact model, quality, size, shape, and reference across submission and stream', () => {
  const edited = { ...image, modelID: 'gpt-image-2.5-flare', resolution: '1536x1024', quality: 'xhigh', aspectRatio: '4:3', referenceImages: ['data:image/png;base64,cGhvdG8='] };
  const snapshot = mediaApprovalDraft([edited]);
  const restored = normalizeMediaApproval({ id: 'selected-image', items: snapshot });
  expect(restored?.items[0]).toMatchObject(edited);
  expect(mediaApprovalItemError(restored!.items[0]!)).toBe('');
  for (const patch of [{ modelID: 'unlisted-image' }, { resolution: '99K' }, { quality: 'unknown' }]) {
    const invalid = { ...edited, ...patch };
    expect(mediaApprovalDraft([invalid])[0]).toMatchObject(patch);
    expect(mediaApprovalItemError(invalid)).not.toBe('');
  }
});

test('changing image provider replaces incompatible model and quality without losing the prompt or reference', () => {
  const original = { ...image, modelID: 'gpt-image-2.5-flare', resolution: '1536x1024', quality: 'max', aspectRatio: '4:3', referenceImages: ['data:image/png;base64,cGhvdG8='] };
  const options = normalizeStudioImageOptions({ ...mediaApprovalImageOptions(original), sourceId: 'xai-api', modelId: '', resolution: '', quality: '' });
  const changed = { ...original, ...updateMediaImageOptions(options) };
  expect(changed).toMatchObject({ modelID: 'grok-imagine-image-2.0', quality: 'low', resolution: '1k', aspectRatio: '4:3', prompt: image.prompt, referenceImages: original.referenceImages });
  expect(mediaApprovalItemError(changed)).toBe('');
  const glowbom = { ...changed, ...updateMediaImageOptions(normalizeStudioImageOptions({ sourceId: 'glowbom-api' })) };
  expect(mediaApprovalItemError(glowbom)).toBe('');
});

test('ElevenLabs review accepts custom IDs and guards v4 limits before generation', () => {
  const voice: OpenCodeMediaApprovalItem = { id: 'voice-model', mediaType: 'audio', prompt: 'Welcome home', provider: 'ElevenLabs', sourceId: 'elevenlabs-api', audioType: 'voice', modelID: 'eleven_v4' };
  expect(mediaApprovalItemError(voice)).toBe('');
  expect(mediaApprovalItemError({ ...voice, prompt: 'a'.repeat(2001) })).toContain('2,000');
  expect(mediaApprovalItemError({ ...voice, modelID: 'eleven_v4_turbo' })).toContain('streaming');
  for (const audioType of ['voice', 'music', 'sound']) {
    const custom = { ...voice, audioType, modelID: 'custom_model:v5', ...(audioType === 'music' ? { durationSeconds: 12 } : {}) };
    expect(mediaApprovalItemError(custom)).toBe('');
    expect(mediaApprovalItemError({ ...custom, modelID: '' })).toContain('custom model ID');
    expect(normalizeMediaApproval({ id: `custom-${audioType}`, items: mediaApprovalDraft([custom]) })?.items[0]).toMatchObject(custom);
  }
  expect(mediaApprovalItemError({ ...voice, modelID: '../unsupported' })).toContain('model ID');
});
