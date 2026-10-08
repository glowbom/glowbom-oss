// Publish the tested assets on the existing OSS release before distributing Desktop.
export const GLOWBOM_LIVE_VERSION = '4.1.1';
const release = 'https://github.com/glowbom/glowbom-oss/releases/download/v4.1.0';
export const GLOWBOM_LIVE_PAGE = 'https://glowbom.com/desktop/#live';
export const GLOWBOM_LIVE_DOWNLOADS = {
  macos: { label: 'Mac', detail: 'Apple silicon and Intel', href: `${release}/Glowbom-Live-${GLOWBOM_LIVE_VERSION}-macos-universal.dmg` },
  windows: { label: 'Windows', detail: 'x64 · Unsigned preview', href: `${release}/Glowbom-Live-${GLOWBOM_LIVE_VERSION}-windows-x64-unsigned.exe` },
  linux: { label: 'Linux', detail: 'x86_64', href: `${release}/Glowbom-Live-${GLOWBOM_LIVE_VERSION}-linux-x86_64.tar.gz` },
  linuxArm: { label: 'Linux', detail: 'ARM64', href: `${release}/Glowbom-Live-${GLOWBOM_LIVE_VERSION}-linux-arm64.tar.gz` },
} as const;

export type LiveDownloadTarget = keyof typeof GLOWBOM_LIVE_DOWNLOADS;
type DeviceHints = {
  userAgent: string;
  platform: string;
  maxTouchPoints?: number;
  architecture?: string;
  bitness?: string;
};

export function detectLiveDownload({ userAgent, platform, maxTouchPoints = 0, architecture, bitness }: DeviceHints): LiveDownloadTarget | null {
  const device = `${platform} ${userAgent}`;
  if (/Android|iPhone|iPad|iPod|CrOS/i.test(device) || (platform === 'MacIntel' && maxTouchPoints > 1)) return null;
  if (/Mac/i.test(device)) return 'macos';
  const arm = /arm|aarch64/i.test(architecture ?? device);
  const arm64 = arm && bitness !== '32' && (bitness === '64' || /aarch64|arm64/i.test(device));
  const x64 = !arm && bitness !== '32' && ((architecture === 'x86' && bitness === '64') || /Win64|WOW64|x64|x86_64|amd64/i.test(device));
  if (/Win/i.test(device)) return x64 ? 'windows' : null;
  if (/Linux/i.test(device)) return arm64 ? 'linuxArm' : x64 ? 'linux' : null;
  return null;
}

export async function currentLiveDownload(): Promise<LiveDownloadTarget | null> {
  if (typeof navigator === 'undefined') return null;
  const hints: DeviceHints = { userAgent: navigator.userAgent, platform: navigator.platform, maxTouchPoints: navigator.maxTouchPoints };
  const client = navigator as Navigator & { userAgentData?: { getHighEntropyValues?(hints: string[]): Promise<{ architecture?: string; bitness?: string }> } };
  try {
    const architecture = await client.userAgentData?.getHighEntropyValues?.(['architecture', 'bitness']);
    Object.assign(hints, architecture);
  } catch {
    // Use the browser's existing device information when extra hints are unavailable.
  }
  return detectLiveDownload(hints);
}
