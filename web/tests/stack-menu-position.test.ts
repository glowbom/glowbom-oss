import { expect, test } from 'bun:test';
import { stackMenuPosition } from '../src/lib/stack-menu-position';

test('folder options open below the toolbar when there is room', () => {
  const position = stackMenuPosition({ left: 140, top: 160, bottom: 200 }, { width: 1200, height: 800 }, 300);
  expect(position.left).toBe(140);
  expect(position.top).toBe(208);
  expect(position.width).toBe(284);
  expect(position.maxHeight).toBe(440);
});

test('folder options move above a low toolbar and stay inside narrow windows', () => {
  const position = stackMenuPosition({ left: 280, top: 600, bottom: 640 }, { width: 320, height: 700 }, 400);
  expect(position.left).toBe(24);
  expect(position.top).toBe(192);
  expect(position.left + position.width).toBeLessThanOrEqual(308);
  expect(position.top + Math.min(400, position.maxHeight)).toBeLessThanOrEqual(688);
});

test('keyboard or short windows constrain the menu instead of clipping its actions', () => {
  const viewport = { width: 260, height: 180, left: 30, top: 100 };
  const position = stackMenuPosition({ left: 280, top: 175, bottom: 215 }, viewport, 600);
  expect(position.left).toBeGreaterThanOrEqual(42);
  expect(position.top).toBeGreaterThanOrEqual(112);
  expect(position.left + position.width).toBeLessThanOrEqual(278);
  expect(position.top + position.maxHeight).toBeLessThanOrEqual(268);
});
