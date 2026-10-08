import { GLOWBOM_LIVE_DOWNLOADS, GLOWBOM_LIVE_PAGE, type LiveDownloadTarget } from './glowbom-live-downloads';

type DesktopBridge = { core: { invoke<T>(command: string, args?: Record<string, unknown>): Promise<T> } };
function desktopBridge(): DesktopBridge | undefined {
  return typeof window === 'undefined' ? undefined : (window as Window & { __TAURI__?: DesktopBridge }).__TAURI__;
}

export function isDesktopApp(): boolean {
  return typeof window !== 'undefined' && ('__TAURI__' in window || '__TAURI_INTERNALS__' in window);
}

function requestBrowserDownload(file: File): void {
  const url = URL.createObjectURL(file);
  const link = document.createElement('a');
  link.href = url; link.download = file.name;
  try { document.body.append(link); link.click(); }
  finally { link.remove(); window.setTimeout(() => URL.revokeObjectURL(url), 30_000); }
}

export async function saveGeneratedFile(file: File): Promise<boolean> {
  if (file.size > 40 * 1024 * 1024) throw new Error('Choose an export smaller than 40 MB.');
  const bridge = desktopBridge();
  if (!bridge?.core?.invoke) {
    if (isDesktopApp()) throw new Error('Quit and reopen the updated Glowbom app to save files.');
    requestBrowserDownload(file); return true;
  }
  const bytes = new Uint8Array(await file.arrayBuffer());
  let binary = '';
  for (let offset = 0; offset < bytes.length; offset += 0x8000) binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000));
  try {
    return await bridge.core.invoke<boolean>('save_generated_file', { name: file.name, data: btoa(binary) });
  } catch (error) {
    if (error === 'native_save_unsupported') { requestBrowserDownload(file); return true; }
    if (error === 'Could not save the file. Choose another location and try again.') throw new Error(error);
    throw new Error(error === 'A save dialog is already open.' ? error : 'Could not save the file. Reopen the latest Glowbom app and try again.');
  }
}

export async function shareDesktopLink(href: string, anchor: { x: number; y: number; width: number; height: number }): Promise<void> {
  const bridge = desktopBridge();
  if (!bridge?.core?.invoke) {
    if (isDesktopApp()) throw new Error('Quit and reopen the updated Glowbom app to share links.');
    return;
  }
  try {
    await bridge.core.invoke('share_link', { url: href, x: anchor.x, y: anchor.y, width: anchor.width, height: anchor.height });
  } catch (error) {
    if (error === 'Choose a web link to share.' || error === 'Sharing from this menu is available in Glowbom for Mac.') throw new Error(error);
    throw new Error('Could not open sharing. Reopen the latest Glowbom app and try again.');
  }
}

export async function openGlowbomLiveDownload(target: LiveDownloadTarget | null): Promise<void> {
  const bridge = desktopBridge();
  if (bridge?.core?.invoke) {
    try { await bridge.core.invoke('open_live_download', { target: target ?? 'website' }); }
    catch { throw new Error('Could not open the download. Reopen the latest Glowbom app and try again.'); }
    return;
  }
  if (isDesktopApp()) throw new Error('Quit and reopen the updated Glowbom app to open downloads.');
  const link = document.createElement('a');
  link.href = target ? GLOWBOM_LIVE_DOWNLOADS[target].href : GLOWBOM_LIVE_PAGE;
  link.target = '_blank'; link.rel = 'noopener noreferrer';
  try { document.body.append(link); link.click(); }
  finally { link.remove(); }
}
