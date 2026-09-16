import { afterEach, expect, test } from 'bun:test';
import { loadBuzzSetup, saveBuzzSetup, shortenBuzzId } from '../src/lib/buzz-setup';

const originalWindow = globalThis.window;
const setup = { relayUrl: 'https://community.example/', channelId: '8c87903e-c0b0-4938-90c3-89e3281dd06e' };
function storage(initial = '') {
  let value = initial;
  globalThis.window = { localStorage: { getItem: () => value, setItem: (_key: string, next: string) => { value = next; } } } as unknown as Window & typeof globalThis;
  return () => value;
}
afterEach(() => { globalThis.window = originalWindow; });

test('remembers only connection coordinates, even if extra fields are supplied', () => {
  const saved = storage();
  saveBuzzSetup({ ...setup, privateKey: 'secret', authTag: 'secret', token: 'secret' } as typeof setup);
  expect(JSON.parse(saved())).toEqual(setup);
  expect(loadBuzzSetup()).toEqual(setup);
});

test('restores coordinates from older data without restoring credentials', () => {
  storage(JSON.stringify({ ...setup, privateKey: 'secret' }));
  expect(loadBuzzSetup()).toEqual(setup);
});

test('handles invalid saved data and unavailable browser storage', () => {
  storage('{invalid');
  expect(loadBuzzSetup()).toEqual({ relayUrl: '', channelId: '' });
  globalThis.window = { get localStorage() { throw new Error('storage blocked'); } } as unknown as Window & typeof globalThis;
  expect(loadBuzzSetup()).toEqual({ relayUrl: '', channelId: '' });
  expect(() => saveBuzzSetup(setup)).not.toThrow();
});

test('does not save credentials embedded in a URL or an invalid channel field', () => {
  for (const relayUrl of ['https://user:secret@community.example', 'https://community.example/?token=secret', 'https://community.example/#secret', 'secret']) {
    storage();
    saveBuzzSetup({ relayUrl, channelId: 'secret' });
    expect(loadBuzzSetup()).toEqual({ relayUrl: '', channelId: '' });
  }
});

test('abbreviates both UUIDs and public keys while preserving their ends', () => {
  expect(shortenBuzzId(setup.channelId)).toBe('8c87903e...d06e');
  expect(shortenBuzzId('ed0fbf15' + 'a'.repeat(52) + '6a83')).toBe('ed0fbf15...6a83');
  expect(shortenBuzzId('short')).toBe('short');
});
