import { withServerAuthHeaders } from './server-auth';
import { readStudioImage, type StudioImage, type StudioProject } from './studio';
import { studioRequestDeadline } from './studio-deadline';
import { readVoiceSettings } from './speech';

export type StudioAudioMode = 'voice' | 'music' | 'sound';
export const studioAudioLabels = { voice: 'Voice', music: 'Music', sound: 'Sound effects' } as const;
export const studioAudioModels = {
  voice: [
    { id: 'eleven_v4', name: 'Eleven v4' }, { id: 'eleven_v3', name: 'Eleven v3' },
    { id: 'eleven_multilingual_v2', name: 'Eleven Multilingual v2' }, { id: 'eleven_flash_v2_5', name: 'Eleven Flash v2.5' },
    { id: 'eleven_flash_v2', name: 'Eleven Flash v2 (English)' },
  ],
  music: [{ id: 'music_v2_5', name: 'Eleven Music v2.5' }, { id: 'music_v2', name: 'Eleven Music v2' }, { id: 'music_v1', name: 'Eleven Music v1' }],
  sound: [{ id: 'eleven_text_to_sound_v2', name: 'Eleven Sound Effects v2' }],
} as const;
export const studioAudioDraftKey = 'glowbom.studio.audio.draft.v1';
const studioAudioJobKey = 'glowbom.studio.audio.job.v1';
export const studioAudioOutputFormats = [
  { id: 'mp3_44100_128', label: 'MP3, 128 kbps, 44.1 kHz (default)', codec: 'MP3' },
  { id: 'mp3_22050_32', label: 'MP3, 32 kbps, 22.05 kHz', codec: 'MP3' },
  { id: 'mp3_24000_48', label: 'MP3, 48 kbps, 24 kHz', codec: 'MP3' },
  { id: 'mp3_44100_32', label: 'MP3, 32 kbps, 44.1 kHz', codec: 'MP3' },
  { id: 'mp3_44100_64', label: 'MP3, 64 kbps, 44.1 kHz', codec: 'MP3' },
  { id: 'mp3_44100_96', label: 'MP3, 96 kbps, 44.1 kHz', codec: 'MP3' },
  { id: 'mp3_44100_192', label: 'MP3, 192 kbps, 44.1 kHz', codec: 'MP3' },
  { id: 'mp3_48000_128', label: 'MP3, 128 kbps, 48 kHz', codec: 'MP3' },
  { id: 'mp3_48000_192', label: 'MP3, 192 kbps, 48 kHz', codec: 'MP3' },
  { id: 'opus_48000_32', label: 'Opus, 32 kbps, 48 kHz', codec: 'Opus' },
  { id: 'opus_48000_64', label: 'Opus, 64 kbps, 48 kHz', codec: 'Opus' },
  { id: 'opus_48000_96', label: 'Opus, 96 kbps, 48 kHz', codec: 'Opus' },
  { id: 'opus_48000_128', label: 'Opus, 128 kbps, 48 kHz', codec: 'Opus' },
  { id: 'opus_48000_192', label: 'Opus, 192 kbps, 48 kHz', codec: 'Opus' },
] as const;

export function studioAudioOutputFormatSupported(format: string): boolean {
  return studioAudioOutputFormats.some((option) => option.id === (format.trim() || 'mp3_44100_128'));
}

export interface StudioAudioDraft {
  mode: StudioAudioMode;
  voice: { prompt: string; voiceId: string; model: string; outputFormat: string };
  music: { prompt: string; model: string; outputFormat: string; duration: string; instrumental: boolean };
  sound: { prompt: string; model: string; outputFormat: string; duration: string; automaticDuration: boolean; promptInfluence: string; loop: boolean };
}

export interface StudioAudioRequest {
  prompt: string; audioType: StudioAudioMode; elevenLabsKey?: string; useSavedKey?: boolean;
  voiceId?: string; voiceModel?: string; soundModel?: string; musicModel?: string;
  outputFormat?: string; durationSeconds?: number; promptInfluence?: number;
  loop?: boolean; forceInstrumental?: boolean; projectPath?: string;
}

export interface StudioAudioAsset extends StudioImage {
  mediaType: 'audio'; audioType?: StudioAudioMode; mimeType?: string; model?: string;
  voiceId?: string; requestedDurationSeconds?: number;
}

function object(value: unknown): Record<string, unknown> {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {};
}
const text = (value: unknown, fallback: string, length = 200) => typeof value === 'string' ? value.slice(0, length) : fallback;
const mode = (value: unknown): StudioAudioMode | undefined => value === 'voice' || value === 'music' || value === 'sound' ? value : undefined;

export function defaultStudioAudioDraft(voiceId = ''): StudioAudioDraft {
  return {
    mode: 'voice',
    voice: { prompt: '', voiceId: voiceId || 'JBFqnCBsd6RMkjVDRZzb', model: 'eleven_multilingual_v2', outputFormat: 'mp3_44100_128' },
    music: { prompt: '', model: 'music_v1', outputFormat: 'mp3_44100_128', duration: '30', instrumental: true },
    sound: { prompt: '', model: 'eleven_text_to_sound_v2', outputFormat: 'mp3_44100_128', duration: '8', automaticDuration: false, promptInfluence: '0.35', loop: false },
  };
}

function sanitizeDraft(value: unknown, voiceId = ''): StudioAudioDraft {
  const saved = object(value), defaults = defaultStudioAudioDraft(voiceId);
  const voice = object(saved.voice), music = object(saved.music), sound = object(saved.sound);
  const common = (section: Record<string, unknown>, fallback: { prompt: string; model: string; outputFormat: string }) => ({
    prompt: text(section.prompt, fallback.prompt, 20000), model: text(section.model, fallback.model), outputFormat: text(section.outputFormat, fallback.outputFormat),
  });
  return {
    mode: mode(saved.mode) || defaults.mode,
    voice: { ...common(voice, defaults.voice), voiceId: text(voice.voiceId, defaults.voice.voiceId) },
    music: { ...common(music, defaults.music), duration: text(music.duration, defaults.music.duration, 40), instrumental: typeof music.instrumental === 'boolean' ? music.instrumental : defaults.music.instrumental },
    sound: {
      ...common(sound, defaults.sound), duration: text(sound.duration, defaults.sound.duration, 40),
      automaticDuration: sound.automaticDuration === true, promptInfluence: text(sound.promptInfluence, defaults.sound.promptInfluence, 40), loop: sound.loop === true,
    },
  };
}

export function readStudioAudioDraft(storage?: Pick<Storage, 'getItem'>): StudioAudioDraft {
  const voiceId = readVoiceSettings().voiceId;
  try { return sanitizeDraft(JSON.parse((storage ?? localStorage).getItem(studioAudioDraftKey) || '{}'), voiceId); }
  catch { return defaultStudioAudioDraft(voiceId); }
}

export function saveStudioAudioDraft(draft: StudioAudioDraft, storage?: Pick<Storage, 'setItem'>): boolean {
  try { (storage ?? localStorage).setItem(studioAudioDraftKey, JSON.stringify(sanitizeDraft(draft))); return true; }
  catch { return false; }
}

export function studioAudioDraftError(draft: StudioAudioDraft): string {
  const section = draft[draft.mode];
  if (!section.prompt.trim()) return draft.mode === 'voice' ? 'Enter the words to speak.' : `Describe the ${draft.mode === 'music' ? 'music' : 'sound effect'} to create.`;
  if (!section.model.trim()) return 'Choose a model or enter a custom model ID.';
  if (!/^[A-Za-z0-9_.:-]{1,200}$/.test(section.model.trim())) return 'Enter a valid ElevenLabs model ID.';
  if (draft.mode === 'voice' && section.model.trim() === 'eleven_v4_turbo') return 'Eleven v4 Turbo needs a streaming connection. Choose another voice model.';
  if (!studioAudioOutputFormatSupported(section.outputFormat)) return 'Choose an MP3 or Opus output format before generating.';
  if (draft.mode === 'music' && section.prompt.trim().length > 4100) return 'Music descriptions must be 4,100 characters or fewer.';
  if (draft.mode === 'voice' && !draft.voice.voiceId.trim()) return 'Choose a voice or enter its voice ID.';
  if (draft.mode === 'music' || (draft.mode === 'sound' && !draft.sound.automaticDuration)) {
    const value = draft.mode === 'music' ? draft.music.duration : draft.sound.duration;
    const duration = value.trim() ? Number(value) : NaN;
    const minimum = draft.mode === 'music' ? 3 : 0.5, maximum = draft.mode === 'music' ? 600 : 30;
    if (!Number.isFinite(duration) || duration < minimum || duration > maximum) return `${draft.mode === 'music' ? 'Music' : 'Sound'} duration must be ${minimum} to ${maximum} seconds.`;
  }
  if (draft.mode === 'sound') {
    const influence = draft.sound.promptInfluence.trim() ? Number(draft.sound.promptInfluence) : NaN;
    if (!Number.isFinite(influence) || influence < 0 || influence > 1) return 'Prompt influence must be between 0 and 1.';
    if (draft.sound.loop && draft.sound.model.trim() && draft.sound.model.trim() !== 'eleven_text_to_sound_v2') return 'Looping requires the ElevenLabs sound effects v2 model.';
  }
  return '';
}

export function studioAudioRequestFromDraft(draft: StudioAudioDraft, credentials: { elevenLabsKey?: string; useSavedKey?: boolean } = {}, projectPath?: string | null): StudioAudioRequest {
  const error = studioAudioDraftError(draft);
  if (error) throw new Error(error);
  const section = draft[draft.mode], key = credentials.elevenLabsKey?.trim() || '';
  const request: StudioAudioRequest = {
    prompt: section.prompt.trim(), audioType: draft.mode, outputFormat: section.outputFormat.trim() || 'mp3_44100_128',
    ...(key ? { elevenLabsKey: key } : { useSavedKey: credentials.useSavedKey === true }),
    ...(projectPath?.trim() ? { projectPath: projectPath.trim() } : {}),
  };
  if (draft.mode === 'voice') return { ...request, voiceId: draft.voice.voiceId.trim(), voiceModel: draft.voice.model.trim() || 'eleven_multilingual_v2' };
  if (draft.mode === 'music') return { ...request, ...(draft.music.model.trim() ? { musicModel: draft.music.model.trim() } : {}), durationSeconds: Number(draft.music.duration), forceInstrumental: draft.music.instrumental };
  return { ...request, ...(draft.sound.model.trim() ? { soundModel: draft.sound.model.trim() } : {}), ...(draft.sound.automaticDuration ? {} : { durationSeconds: Number(draft.sound.duration) }), promptInfluence: Number(draft.sound.promptInfluence), loop: draft.sound.loop };
}

export function readStudioAudioAsset(value: unknown): StudioAudioAsset | undefined {
  const base = readStudioImage(value);
  if (!base) return;
  const saved = object(value), result: StudioAudioAsset = { ...base, mediaType: 'audio' };
  if (mode(saved.audioType)) result.audioType = mode(saved.audioType);
  for (const key of ['mimeType', 'model', 'voiceId'] as const) if (typeof saved[key] === 'string') result[key] = saved[key].slice(0, 200);
  if (typeof saved.requestedDurationSeconds === 'number' && Number.isFinite(saved.requestedDurationSeconds) && saved.requestedDurationSeconds > 0) result.requestedDurationSeconds = saved.requestedDurationSeconds;
  return result;
}

async function audioRequest<T>(path: string, init: RequestInit = {}, timeout = 30000): Promise<T> {
  return studioRequestDeadline(async (signal) => {
    const response = await fetch(path, { ...init, signal, headers: withServerAuthHeaders(init.headers) });
    if (!response.ok) throw new Error((await response.text()).trim().slice(0, 2000) || `Audio request failed with HTTP ${response.status}.`);
    return response.json() as Promise<T>;
  }, timeout, 'The audio request timed out. Refresh saved audio before generating again.', init.signal ?? undefined);
}

export async function loadStudioAudio(options: { offset?: number; limit?: number; projectId?: string; signal?: AbortSignal } = {}): Promise<{ audio: StudioAudioAsset[]; hasMore: boolean }> {
  const { signal, ...page } = options, query = new URLSearchParams();
  for (const [key, value] of Object.entries(page)) if (value !== undefined) query.set(key, String(value));
  const result = await audioRequest<{ audio?: unknown[]; hasMore?: boolean }>(`/api/studio/audio${query.size ? `?${query}` : ''}`, { signal });
  return { audio: (Array.isArray(result.audio) ? result.audio : []).map(readStudioAudioAsset).filter((asset): asset is StudioAudioAsset => !!asset), hasMore: result.hasMore === true };
}

export async function generateStudioAudio(request: StudioAudioRequest): Promise<StudioAudioAsset> {
  const result = await audioRequest<{ asset?: unknown }>('/api/studio/audio/generate', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(request) }, 15 * 60 * 1000);
  const asset = readStudioAudioAsset(result.asset);
  if (!asset) throw new Error('Studio did not return saved audio. Refresh the library before generating again.');
  return asset;
}

export async function loadStudioAudioBlob(id: string, signal?: AbortSignal): Promise<Blob> {
  return studioRequestDeadline(async (boundedSignal) => {
    const response = await fetch(`/api/studio/audio/content?id=${encodeURIComponent(id)}`, { signal: boundedSignal, headers: withServerAuthHeaders() });
    if (!response.ok) throw new Error('Could not load this audio.');
    return response.blob();
  }, 60000, 'Loading the audio timed out. Try opening it again.', signal);
}

export async function deleteStudioAudio(id: string): Promise<void> {
  await audioRequest(`/api/studio/audio?id=${encodeURIComponent(id)}`, { method: 'DELETE' });
}

export async function useStudioAudioInProject(id: string, path: string): Promise<{ asset: StudioAudioAsset; project: StudioProject; relativePath: string }> {
  const result = await audioRequest<{ asset?: unknown; project: StudioProject; relativePath: string }>('/api/studio/audio/use', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id, path }) });
  const asset = readStudioAudioAsset(result.asset);
  if (!asset) throw new Error('Studio did not return the copied audio.');
  return { ...result, asset };
}

export async function loadStudioAudioVoices(credentials: { elevenLabsKey?: string; useSavedKey?: boolean }, signal?: AbortSignal): Promise<{ voiceId: string; name: string; category?: string }[]> {
  const result = await audioRequest<{ voices?: unknown[] }>('/api/audio/voices', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(credentials), signal });
  return (Array.isArray(result.voices) ? result.voices : []).flatMap((value) => {
    const voice = object(value);
    return typeof voice.voiceId === 'string' && typeof voice.name === 'string' ? [{ voiceId: voice.voiceId, name: voice.name, ...(typeof voice.category === 'string' ? { category: voice.category } : {}) }] : [];
  });
}

export async function studioAudioKeyConfigured(signal?: AbortSignal): Promise<boolean> {
  const value = await audioRequest<{ configured?: boolean }>('/api/audio/key', { signal, cache: 'no-store' });
  return value.configured === true;
}

export async function saveStudioAudioKey(key: string): Promise<void> {
  const value = await audioRequest<{ configured?: boolean }>('/api/audio/key', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ key: key.trim() }) });
  if (!value.configured) throw new Error('Could not save the key securely. Unlock your system credential store and try again.');
}

export function studioAudioDownloadName(asset: Pick<StudioAudioAsset, 'id' | 'prompt' | 'audioType'>, mimeType: string): string {
  const extensions: Record<string, string> = { 'audio/mpeg': 'mp3', 'audio/mp3': 'mp3', 'audio/wav': 'wav', 'audio/wave': 'wav', 'audio/x-wav': 'wav', 'audio/flac': 'flac', 'audio/ogg': 'ogg', 'audio/mp4': 'm4a' };
  const title = Array.from(asset.prompt.trim().replace(/[<>:"/\\|?*\u0000-\u001f]/g, ' ').replace(/\s+/g, ' ')).slice(0, 60).join('').replace(/[. ]+$/, '') || asset.id;
  const name = `Glowbom ${asset.audioType === 'sound' ? 'sound' : asset.audioType || 'audio'} - ${title}`;
  const suffix = `.${extensions[mimeType.toLowerCase().split(';')[0]?.trim() || ''] || 'bin'}`;
  const encoder = new TextEncoder();
  let filename = '', bytes = encoder.encode(suffix).length;
  for (const character of name) { bytes += encoder.encode(character).length; if (bytes > 240) break; filename += character; }
  return `${filename.trimEnd()}${suffix}`;
}

export interface StudioAudioJob {
  stage: 'generating' | 'complete' | 'failed'; draft: StudioAudioDraft; startedAt: number;
  asset?: StudioAudioAsset; error?: string;
}

// Keep a paid generation running when the user leaves the Audio panel.
export class StudioAudioCoordinator {
  private job: StudioAudioJob | null = null;
  private listeners = new Set<() => void>();
  constructor(private options: { storage?: Pick<Storage, 'getItem' | 'setItem'>; generate?: typeof generateStudioAudio } = {}) {
    try {
      const saved = object(JSON.parse((options.storage ?? localStorage).getItem(studioAudioJobKey) || 'null'));
      const asset = readStudioAudioAsset(saved.asset);
      if (!['generating', 'complete', 'failed'].includes(String(saved.stage))) return;
      this.job = {
        stage: saved.stage === 'complete' && asset ? 'complete' : 'failed', draft: sanitizeDraft(saved.draft),
        startedAt: typeof saved.startedAt === 'number' && Number.isFinite(saved.startedAt) ? saved.startedAt : Date.now(),
        ...(asset ? { asset } : {}),
        ...(saved.stage === 'generating' ? { error: 'Glowbom reopened during generation. The request may still finish. Refresh saved audio before generating again.' } : typeof saved.error === 'string' ? { error: saved.error.slice(0, 2000) } : {}),
      };
    } catch { /* Opening Audio does not require browser storage. */ }
  }
  getSnapshot = (): StudioAudioJob | null => this.job;
  subscribe = (listener: () => void): (() => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; };
  private publish() {
    try { (this.options.storage ?? localStorage).setItem(studioAudioJobKey, JSON.stringify(this.job)); }
    catch { /* The active request remains available during this session. */ }
    this.listeners.forEach((listener) => listener());
  }
  start(draft: StudioAudioDraft, credentials: { elevenLabsKey?: string; useSavedKey?: boolean }, projectPath?: string | null): boolean {
    if (this.job?.stage === 'generating') return false;
    const request = studioAudioRequestFromDraft(draft, credentials, projectPath);
    if (!request.elevenLabsKey && !request.useSavedKey) throw new Error('Enter or save an ElevenLabs key first.');
    const pending: StudioAudioJob = { stage: 'generating', draft: sanitizeDraft(draft), startedAt: Date.now() };
    this.job = pending; this.publish();
    void (async () => {
      try { const asset = await (this.options.generate ?? generateStudioAudio)(request); if (this.job === pending) this.job = { ...pending, stage: 'complete', asset }; }
      catch (cause) { if (this.job === pending) this.job = { ...pending, stage: 'failed', error: cause instanceof Error ? cause.message : 'Could not create this audio. Refresh the library before generating again.' }; }
      this.publish();
    })();
    return true;
  }
  forgetAsset(id: string) { if (this.job?.asset?.id === id) { this.job = null; this.publish(); } }
}

export const studioAudioCoordinator = new StudioAudioCoordinator();
