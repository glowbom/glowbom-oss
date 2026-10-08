import { isDesktopApp, shareDesktopLink } from './downloads';

// The desktop window shows WebKit's own link menu. Its "Open Link" and download items
// cannot leave the app window, so the desktop app shows this small menu instead.
// Share opens the macOS share sheet, including AirDrop.

/** Returns the address a link menu should offer, or null when the link stays inside the app. */
export function linkMenuAddress(href: string, target: string, base: string): string | null {
  let url: URL;
  try { url = new URL(href, base); } catch { return null; }
  if (url.protocol !== 'http:' && url.protocol !== 'https:') return null;
  if (target !== '_blank' && url.origin === new URL(base).origin) return null;
  return url.href;
}

/** Opens a web address in the default browser. The desktop shell turns new window requests into a browser launch. */
export function openInBrowser(href: string): void {
  window.open(href, '_blank', 'noopener,noreferrer');
}

let menu: HTMLDivElement | null = null;
let focusReturn: HTMLElement | null = null;

function closeMenu(restoreFocus = true): void {
  if (!menu) return;
  menu.remove();
  menu = null;
  if (restoreFocus) focusReturn?.focus({ preventScroll: true });
  focusReturn = null;
}

function menuButton(label: string, action: (button: HTMLButtonElement) => void): HTMLButtonElement {
  const button = document.createElement('button');
  button.type = 'button';
  button.setAttribute('role', 'menuitem');
  button.textContent = label;
  button.addEventListener('click', () => { action(button); closeMenu(); });
  return button;
}

async function copyLink(href: string): Promise<void> {
  try { await navigator.clipboard.writeText(href); }
  catch { /* The clipboard is unavailable. The link stays visible in the app. */ }
}

function showMenu(href: string, x: number, y: number, anchor: HTMLElement): void {
  closeMenu(false);
  const element = document.createElement('div');
  element.className = 'desktop-link-menu';
  element.setAttribute('role', 'menu');
  element.setAttribute('aria-label', 'Link actions');
  const open = menuButton('Open in browser', () => openInBrowser(href));
  const share = menuButton('Share link', (button) => {
    const rect = button.getBoundingClientRect();
    void shareDesktopLink(href, rect);
  });
  element.append(open, menuButton('Copy link', () => { void copyLink(href); }), share);
  // A modal dialog sits above the page, so the menu must live inside it to be visible.
  (anchor.closest('dialog[open]') ?? document.body).append(element);
  const rect = element.getBoundingClientRect();
  element.style.left = `${Math.max(8, Math.min(x, window.innerWidth - rect.width - 8))}px`;
  element.style.top = `${Math.max(8, Math.min(y, window.innerHeight - rect.height - 8))}px`;
  menu = element;
  focusReturn = anchor;
  open.focus({ preventScroll: true });
}

function onContextMenu(event: MouseEvent): void {
  if (event.defaultPrevented || !(event.target instanceof Element)) return;
  const anchor = event.target.closest('a[href]');
  if (!(anchor instanceof HTMLAnchorElement)) return;
  const href = linkMenuAddress(anchor.getAttribute('href') || '', anchor.target, location.href);
  if (!href) return;
  event.preventDefault();
  const rect = anchor.getBoundingClientRect();
  const fromKeyboard = !event.clientX && !event.clientY;
  showMenu(href, fromKeyboard ? rect.left + 16 : event.clientX, fromKeyboard ? rect.bottom : event.clientY, anchor);
}

function onKeyDown(event: KeyboardEvent): void {
  if (!menu) return;
  if (event.key === 'Escape' || event.key === 'Tab') { event.preventDefault(); closeMenu(); return; }
  if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
  event.preventDefault();
  const buttons = Array.from(menu.querySelectorAll('button'));
  const index = buttons.findIndex((button) => button === document.activeElement);
  const next = event.key === 'Home' ? 0 : event.key === 'End' ? buttons.length - 1
    : event.key === 'ArrowDown' ? (index + 1) % buttons.length : (index - 1 + buttons.length) % buttons.length;
  buttons[next]?.focus({ preventScroll: true });
}

/** Installs the desktop link menu once. Does nothing in a regular browser. */
export function installDesktopLinkMenu(): void {
  if (!isDesktopApp()) return;
  document.addEventListener('contextmenu', onContextMenu);
  document.addEventListener('pointerdown', (event) => { if (menu && !menu.contains(event.target as Node)) closeMenu(false); }, true);
  window.addEventListener('keydown', onKeyDown, true);
  window.addEventListener('scroll', () => closeMenu(false), true);
  window.addEventListener('resize', () => closeMenu(false));
  window.addEventListener('blur', () => closeMenu(false));
}
