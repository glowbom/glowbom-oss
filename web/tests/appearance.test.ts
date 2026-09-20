import { afterEach, expect, test } from 'bun:test';
import { applyAppearance, parseAppearance, readAppearance, saveAppearance } from '../src/lib/appearance';

const previousDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
const previousStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
afterEach(() => {
  for (const [key, descriptor] of [['document', previousDocument], ['localStorage', previousStorage]] as const) {
    if (descriptor) Object.defineProperty(globalThis, key, descriptor);
    else Reflect.deleteProperty(globalThis, key);
  }
});

test('system follows appearance changes while explicit choices stay fixed', () => {
  const dataset: Record<string, string> = {};
  Object.defineProperty(globalThis, 'document', { configurable: true, value: { documentElement: { dataset } } });
  applyAppearance('system', true);
  expect(dataset.theme).toBe('dark');
  applyAppearance('system', false);
  expect(dataset.theme).toBe('light');
  applyAppearance('light', true);
  expect(dataset.theme).toBe('light');
  applyAppearance('dark', false);
  expect(dataset.theme).toBe('dark');
});

test('missing and invalid saved choices default to system', () => {
  expect(parseAppearance(null)).toBe('system');
  expect(parseAppearance('corrupt')).toBe('system');
});

test('blocked browser storage does not prevent opening or changing appearance', () => {
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, get() { throw new Error('Storage blocked'); } });
  expect(readAppearance()).toBe('system');
  expect(() => saveAppearance('dark')).not.toThrow();
});
