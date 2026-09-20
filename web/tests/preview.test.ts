import { discoverStacks } from '../src/lib/preview';
import { describe, expect, spyOn, test } from 'bun:test';
import { formatPreviewCommand, parsePreviewCommand, previewRequest } from '../src/lib/preview';
import { nextStackDirectory, STACK_PRESETS } from '../src/lib/stack-presets';

describe('custom preview commands', () => {
  test('keeps quoted paths and host/port placeholders as separate arguments', () => {
    expect(parsePreviewCommand('bun "my server.ts" --host {host} --port {port}')).toEqual(['bun', 'my server.ts', '--host', '{host}', '--port', '{port}']);
  });
  test('round trips edited settings without changing arguments', () => {
    const args = ['bun', 'a path/with "quotes".ts', '--host', '{host}', '--port', '{port}', ''];
    expect(parsePreviewCommand(formatPreviewCommand(args))).toEqual(args);
  });
  test('rejects shell chaining and incomplete quotes', () => {
    for (const command of ['bun dev && other', 'bun dev | other', 'bun $TOKEN', 'bun `other`', 'bun "unfinished']) {
      expect(() => parsePreviewCommand(command)).toThrow();
    }
  });
  test('allows automatic detection with no command', () => {
    expect(parsePreviewCommand('  ')).toEqual([]);
  });
});

test('adding another preset leaves existing target folders available', () => {
  expect(nextStackDirectory('react', ['prototype', 'web'])).toBe('react');
  expect(nextStackDirectory('react', ['./react/', 'react-2'])).toBe('react-3');
  expect(nextStackDirectory('desktop', ['desktop', 'desktop-3'])).toBe('desktop-2');
});

test('builds require a backend that understands saved stack instructions', async () => {
  const request = spyOn(globalThis, 'fetch').mockResolvedValue(new Response(JSON.stringify({ targets: [] })));
  try {
    await expect(previewRequest('/project', 'inspect', { requireStackInstructions: true })).rejects.toThrow('Restart Glowbom OSS');
    request.mockResolvedValue(new Response(JSON.stringify({ targets: [], stackInstructionsSupported: true })));
    expect(await previewRequest('/project', 'inspect', { requireStackInstructions: true })).toEqual([]);
  } finally {
    request.mockRestore();
  }
});

test('catalog commands satisfy the preview runner contract', () => {
  expect(new Set(STACK_PRESETS.map((p) => p.id)).size).toBe(STACK_PRESETS.length);
  for (const preset of STACK_PRESETS) {
    expect(new TextEncoder().encode(preset.description).length).toBeLessThanOrEqual(8000);
    if (preset.previewMode === 'command') {
      const command = preset.command || [];
      expect(command.join(' ')).toContain('{host}');
      expect(command.join(' ')).toContain('{port}');
      expect(parsePreviewCommand(formatPreviewCommand(command))).toEqual(command);
    } else {
      expect(preset.command || []).toEqual([]);
    }
  }
});

test('catalog settings refuse an older backend and preserve saved command metadata', async () => {
  const request = spyOn(globalThis, 'fetch').mockResolvedValue(new Response(JSON.stringify({ targets: [], stackInstructionsSupported: true })));
  try {
    await expect(previewRequest('/project', 'inspect', { requireStackCatalog: true })).rejects.toThrow('Restart Glowbom OSS');
    request.mockResolvedValue(new Response(JSON.stringify({ targets: [], stackCatalogSupported: true })));
    const php = STACK_PRESETS.find((p) => p.id === 'php')!;
    const config = { id: '', name: 'PHP', directory: 'php', command: php.command, description: php.description, previewMode: php.previewMode, previewNotes: php.previewNotes };
    await previewRequest('/project', 'save', { config, requireStackCatalog: true });
    const body = JSON.parse(String(request.mock.calls.at(-1)?.[1]?.body));
    expect(body.config).toEqual(config);
    expect(body.requireStackCatalog).toBeUndefined();
  } finally { request.mockRestore(); }
});


test('discovery is read-only and explains an older backend', async () => {
  const request = spyOn(globalThis, 'fetch').mockResolvedValue(new Response('Unknown preview action', { status: 400 }));
  try {
    await expect(discoverStacks('/project')).rejects.toThrow('restart Glowbom OSS');
    request.mockResolvedValue(new Response(JSON.stringify({ apps: [], limited: true })));
    expect(await discoverStacks('/project')).toEqual({ apps: [], limited: true });
    expect(JSON.parse(String(request.mock.calls.at(-1)?.[1]?.body))).toEqual({ path: '/project', action: 'discover' });
  } finally { request.mockRestore(); }
});
