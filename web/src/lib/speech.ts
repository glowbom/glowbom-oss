import { withServerAuthHeaders } from './server-auth';

export const speechCharacterLimit = 2500;

export function speechChunks(text: string, limit = 220): string[] {
  const clean = text.replace(/\s+/g, ' ').trim();
  if (!clean) return [];
  const parts: string[] = [];
  let rest = clean;
  while (rest.length > limit) {
    const window = rest.slice(0, limit + 1);
    const sentence = Math.max(window.lastIndexOf('. '), window.lastIndexOf('? '), window.lastIndexOf('! '));
    const softer = Math.max(window.lastIndexOf('; '), window.lastIndexOf(', '));
    const space = window.lastIndexOf(' ');
    const cut = sentence >= 40 ? sentence + 1 : softer >= 40 ? softer + 1 : space >= 40 ? space + 1 : limit;
    const piece = rest.slice(0, cut).trim();
    if (!piece) break;
    parts.push(piece);
    rest = rest.slice(cut).trim();
  }
  if (rest) parts.push(rest);
  return parts;
}

export function chooseSpeechEngine(text: string, elevenLabsKey: string): 'elevenlabs' | 'browser' | 'none' {
  if (!text.trim()) return 'none';
  if (elevenLabsKey.trim() && text.trim().length <= speechCharacterLimit) return 'elevenlabs';
  return 'browser';
}

export function elevenLabsKeyFrom(raw: string | null, legacy: string | null = null): string {
  for (const value of [raw, legacy]) {
    if (!value) continue;
    try {
      const parsed = JSON.parse(value) as { elevenLabsKey?: unknown };
      if (typeof parsed.elevenLabsKey === 'string' && parsed.elevenLabsKey.trim()) return parsed.elevenLabsKey.trim();
    } catch {
      // A damaged settings copy is skipped.
    }
  }
  return '';
}

export function readStoredElevenLabsKey(): string {
  try {
    return elevenLabsKeyFrom(localStorage.getItem('glowbom_oss_provider_keys'), localStorage.getItem('glowby_oss_provider_keys'));
  } catch {
    return '';
  }
}

export interface VoiceSettings { source: 'system' | 'elevenlabs' | 'local'; elevenLabsKey: string; voiceId: string; systemVoice: string; voiceName?: string; useSavedKey?: boolean; localVoiceId?: string; localVoiceName?: string; localSetup?: 'kitten' | 'voicestudio' }
const voiceSettingsKey = 'glowbom_voice_settings';
let sessionElevenLabsKey: string | undefined;
export function readVoiceSettings(): VoiceSettings {
  const key = readStoredElevenLabsKey();
  const defaults: VoiceSettings = { source: key ? 'elevenlabs' : 'system', elevenLabsKey: key, voiceId: '', systemVoice: '' };
  try {
    const raw = localStorage.getItem(voiceSettingsKey);
    if (!raw) return defaults;
    const value = JSON.parse(raw);
    return { ...(value.useSavedKey === true ? { useSavedKey: true } : {}), ...(typeof value.voiceName === 'string' && value.voiceName ? { voiceName: value.voiceName } : {}), ...(typeof value.localVoiceId === 'string' ? { localVoiceId: value.localVoiceId } : {}), ...(typeof value.localVoiceName === 'string' && value.localVoiceName ? { localVoiceName: value.localVoiceName } : {}), ...(value.localSetup === 'kitten' || value.localSetup === 'voicestudio' ? { localSetup: value.localSetup } : {}), source: value.source === 'elevenlabs' || value.source === 'local' ? value.source : 'system', elevenLabsKey: sessionElevenLabsKey ?? '', voiceId: typeof value.voiceId === 'string' ? value.voiceId : '', systemVoice: typeof value.systemVoice === 'string' ? value.systemVoice : '' };
  } catch { return defaults; }
}
export function saveVoiceSettings(settings: VoiceSettings) {
  const { elevenLabsKey, ...preferences } = settings;
  localStorage.setItem(voiceSettingsKey, JSON.stringify(preferences));
  sessionElevenLabsKey = elevenLabsKey.trim();
}

type Stopper = () => void;
let generation = 0;
let activeStop: Stopper | null = null;

export function stopSpeaking() {
  generation += 1;
  const stop = activeStop;
  activeStop = null;
  stop?.();
}

function finish(id: number, onEnd?: () => void) {
  if (generation !== id) return;
  activeStop = null;
  onEnd?.();
}

async function speakAudioEndpoint(path: string, body: Record<string, unknown>, id: number, onEnd?: () => void): Promise<'playing' | 'failed' | 'stopped'> {
  const controller = new AbortController();
  let audio: HTMLAudioElement | null = null;
  activeStop = () => {
    controller.abort();
    if (!audio) return;
    audio.pause();
    audio.removeAttribute('src');
    audio.load();
  };
  try {
    const response = await fetch(path, {
      method: 'POST',
      signal: controller.signal,
      headers: withServerAuthHeaders({ 'Content-Type': 'application/json' }),
      body: JSON.stringify(body),
    });
    if (generation !== id) return 'stopped';
    if (!response.ok) return 'failed';
    const data = await response.json() as { audio?: unknown };
    if (generation !== id) return 'stopped';
    if (typeof data.audio !== 'string' || !data.audio.startsWith('data:')) return 'failed';
    audio = new Audio(data.audio);
    audio.onended = () => finish(id, onEnd);
    audio.onerror = () => finish(id, onEnd);
    await audio.play();
    return generation === id ? 'playing' : 'stopped';
  } catch {
    return generation === id && !controller.signal.aborted ? 'failed' : 'stopped';
  }
}

function speakBrowser(text: string, id: number, onEnd?: () => void, voiceURI?: string): boolean {
  const synth = window.speechSynthesis;
  if (!synth || typeof SpeechSynthesisUtterance === 'undefined') return false;
  const chunks = speechChunks(text);
  if (!chunks.length) return false;
  let index = 0;
  let stopped = false;
  activeStop = () => {
    stopped = true;
    synth.cancel();
  };
  const step = () => {
    if (stopped || generation !== id) return;
    if (index >= chunks.length) {
      finish(id, onEnd);
      return;
    }
    const utterance = new SpeechSynthesisUtterance(chunks[index]);
    index += 1;
    utterance.rate = 1;
    utterance.voice = synth.getVoices().find((voice) => voice.voiceURI === voiceURI) || null;
    utterance.onend = () => step();
    utterance.onerror = () => {
      if (!stopped) finish(id, onEnd);
    };
    synth.speak(utterance);
  };
  try { synth.resume(); } catch { /* Playback can still start without resume. */ }
  window.setTimeout(() => {
    if (generation === id) step();
  }, 0);
  return true;
}

export async function speakText(text: string, options: { elevenLabsKey?: string; settings?: VoiceSettings; fallback?: boolean; onEnd?: () => void } = {}): Promise<boolean> {
  stopSpeaking();
  const clean = text.replace(/\s+/g, ' ').trim();
  if (!clean) return false;
  const id = ++generation;
  const settings = options.settings || readVoiceSettings();
  if (settings.source === 'local') {
    if (clean.length <= speechCharacterLimit) {
      const outcome = await speakAudioEndpoint('/api/audio/local', { prompt: clean, voiceId: settings.localVoiceId || '' }, id, options.onEnd);
      if (outcome === 'playing') return true;
      if (generation !== id || options.fallback === false) return false;
    } else if (options.fallback === false) return false;
    return speakBrowser(clean, id, options.onEnd, settings.systemVoice);
  }
  const key = options.elevenLabsKey?.trim() ?? (settings.source === 'elevenlabs' ? settings.elevenLabsKey.trim() : '');
  const useSavedKey = settings.source === 'elevenlabs' && !!settings.useSavedKey && !key;
  if (chooseSpeechEngine(clean, key || (useSavedKey ? 'saved' : '')) === 'elevenlabs') {
    const outcome = await speakAudioEndpoint('/api/audio', { prompt: clean, audioType: 'voice', elevenLabsKey: key, voiceId: settings.voiceId, useSavedKey, ephemeral: true }, id, options.onEnd);
    if (outcome === 'playing') return true;
    if (generation !== id || options.fallback === false) return false;
  }
  if (settings.source === 'elevenlabs' && options.fallback === false) return false;
  if (generation !== id) return false;
  return speakBrowser(clean, id, options.onEnd, settings.systemVoice);
}
