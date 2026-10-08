import { readBuildCompletionSound } from './build-settings';
import type { RefineRunStatus } from '../hooks/useRefineRun';

export type BuildRunStatuses = { magic: RefineRunStatus; cursor: RefineRunStatus; codex?: RefineRunStatus; claude?: RefineRunStatus; acp?: RefineRunStatus };

export function didBuildComplete(previous: BuildRunStatuses, next: BuildRunStatuses): boolean {
  return (previous.magic === 'running' && next.magic === 'completed')
    || (previous.cursor === 'running' && next.cursor === 'completed')
    || (previous.codex === 'running' && next.codex === 'completed')
    || (previous.claude === 'running' && next.claude === 'completed')
    || (previous.acp === 'running' && next.acp === 'completed');
}

const soundUrl = `${import.meta.env.BASE_URL}sounds/build-complete.wav`;
let audioContext: AudioContext | null = null;
let decodedSound: Promise<AudioBuffer | null> | null = null;

export function prepareBuildCompletionSound(): void {
  if (!readBuildCompletionSound() || !window.AudioContext) return;
  try {
    audioContext ??= new AudioContext();
    void audioContext.resume().catch(() => {});
    decodedSound ??= fetch(soundUrl)
      .then(response => response.ok ? response.arrayBuffer() : Promise.reject())
      .then(bytes => audioContext!.decodeAudioData(bytes))
      .catch(() => { decodedSound = null; return null; });
  } catch {
    // The completion handler can still try the browser's audio element.
  }
}

function playAudioElement(): void {
  try {
    const audio = new Audio(soundUrl);
    audio.volume = 0.6;
    void audio.play().catch(() => {});
  } catch {
    // Sound is optional when the browser cannot play audio.
  }
}

export function playBuildCompletionSound(): void {
  if (!readBuildCompletionSound()) return;
  void (async () => {
    const context = audioContext;
    const buffer = await decodedSound;
    if (!readBuildCompletionSound()) return;
    if (context && buffer) {
      try {
        if (context.state !== 'running') await context.resume();
        const source = context.createBufferSource();
        const gain = context.createGain();
        source.buffer = buffer;
        gain.gain.value = 0.6;
        source.connect(gain).connect(context.destination);
        source.addEventListener('ended', () => { source.disconnect(); gain.disconnect(); }, { once: true });
        source.start();
        return;
      } catch {
        // Try the audio element if Web Audio playback is unavailable.
      }
    }
    playAudioElement();
  })();
}
