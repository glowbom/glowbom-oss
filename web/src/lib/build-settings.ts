const key = 'glowbom_build_use_jev';
const completionSoundKey = 'glowbom_build_completion_sound';
export function readUseJev(): boolean {
  try { return localStorage.getItem(key) === 'true'; } catch { return false; }
}
export function saveUseJev(enabled: boolean) { localStorage.setItem(key, String(enabled)); }
export function useJevForBuild(driver?: string): boolean { return driver !== 'acp' && driver !== 'cursor' && driver !== 'codex' && driver !== 'claude-code' && readUseJev(); }

export function readBuildCompletionSound(): boolean {
  try { return localStorage.getItem(completionSoundKey) !== 'false'; } catch { return true; }
}

export function saveBuildCompletionSound(enabled: boolean): void {
  localStorage.setItem(completionSoundKey, String(enabled));
}
