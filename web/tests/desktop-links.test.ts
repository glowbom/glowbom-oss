import { afterEach, expect, test } from 'bun:test';
import { installDesktopLinkMenu, linkMenuAddress } from '../src/lib/desktop-links';

const base = 'http://127.0.0.1:4572/';
const originalWindow = Object.getOwnPropertyDescriptor(globalThis, 'window');
const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
afterEach(() => {
  for (const [name, descriptor] of [['window', originalWindow], ['document', originalDocument]] as const) {
    if (descriptor) Object.defineProperty(globalThis, name, descriptor);
    else Reflect.deleteProperty(globalThis, name);
  }
});

test('the desktop link menu offers web addresses that leave the app', () => {
  expect(linkMenuAddress('https://glowbom.com/docs/', '_blank', base)).toBe('https://glowbom.com/docs/');
  expect(linkMenuAddress('https://glowbom.com', '', base)).toBe('https://glowbom.com/');
  expect(linkMenuAddress('http://127.0.0.1:5173', '_blank', base)).toBe('http://127.0.0.1:5173/');
  expect(linkMenuAddress('/preview/', '_blank', base)).toBe('http://127.0.0.1:4572/preview/');
});

test('the desktop link menu leaves in-app and non-web links to the page', () => {
  expect(linkMenuAddress('/settings', '', base)).toBeNull();
  expect(linkMenuAddress('#top', '', base)).toBeNull();
  expect(linkMenuAddress('mailto:hello@glowbom.com', '_blank', base)).toBeNull();
  expect(linkMenuAddress('javascript:alert(1)', '_blank', base)).toBeNull();
  expect(linkMenuAddress('file:///etc/passwd', '_blank', base)).toBeNull();
  expect(linkMenuAddress('http://[bad', '_blank', base)).toBeNull();
});

test('a regular browser never installs the desktop link menu', () => {
  const listeners: string[] = [];
  Object.defineProperty(globalThis, 'window', { configurable: true, value: { addEventListener: (type: string) => listeners.push(type) } });
  Object.defineProperty(globalThis, 'document', { configurable: true, value: { addEventListener: (type: string) => listeners.push(type) } });
  installDesktopLinkMenu();
  expect(listeners).toEqual([]);
});
