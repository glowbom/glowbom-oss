const STORAGE_KEY = 'glowbom.buzzSetup';
type BuzzSetup = { relayUrl: string; channelId: string };

function cleanSetup(value: unknown): BuzzSetup {
  const data = value && typeof value === 'object' ? value as Partial<BuzzSetup> : {};
  let relayUrl = '';
  try {
    if (typeof data.relayUrl === 'string' && data.relayUrl.length <= 2048) {
      const url = new URL(data.relayUrl.trim());
      if (url.protocol === 'https:' && !url.username && !url.password && !url.search && !url.hash) relayUrl = data.relayUrl.trim();
    }
  } catch { /* Incomplete URLs are not saved. */ }
  const channelId = typeof data.channelId === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(data.channelId.trim()) ? data.channelId.trim() : '';
  return { relayUrl, channelId };
}

export function loadBuzzSetup(): BuzzSetup {
  try { return cleanSetup(JSON.parse(window.localStorage.getItem(STORAGE_KEY) || '{}')); }
  catch { return { relayUrl: '', channelId: '' }; }
}

export function saveBuzzSetup(setup: BuzzSetup): void {
  try {
    // Explicitly save connection coordinates only, never credentials or rosters.
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(cleanSetup(setup)));
  } catch { /* Browsers with storage disabled can still connect. */ }
}

export function shortenBuzzId(value: string): string {
  return value.length > 15 ? `${value.slice(0, 8)}...${value.slice(-4)}` : value;
}
