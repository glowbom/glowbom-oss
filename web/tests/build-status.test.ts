import { expect, test } from 'bun:test';
import { buildStatusView, parseBuildStatusUpdate } from '../src/lib/build-status';

test('accepts short structured updates and rejects invalid status events', () => {
  const update = parseBuildStatusUpdate({ text: '  Building the sign-in screen\nfor your app  ', source: 'agent', at: '2026-09-27T16:00:00Z' });
  expect(update).toEqual({ text: 'Building the sign-in screen for your app', source: 'agent', at: '2026-09-27T16:00:00Z' });
  expect(parseBuildStatusUpdate({ text: 'Something happened', source: 'unknown', at: '2026-09-27T16:00:00Z' })).toBeNull();
  expect(parseBuildStatusUpdate({ text: '', source: 'agent' })).toBeNull();
  const repeated = parseBuildStatusUpdate({ text: 'GLOWBOM_STATUS: Prototype verified to compile. GLOWBOM_STATUS: Purple mode button is ready. GLOWBOM_STATUS:', source: 'agent' });
  expect(repeated?.text).toBe('Purple mode button is ready.');
  expect(parseBuildStatusUpdate({ text: 'GLOWBOM_STATUS: GLOWBOM_STATUS:', source: 'agent' })).toBeNull();
});

test('shows the latest agent update with earlier steps, not raw instructions', () => {
  const updates = [
    { text: 'Reviewing the sign-in flow', source: 'agent' as const, at: '2026-09-27T16:00:00Z' },
    { text: 'Adding the account screen', source: 'agent' as const, at: '2026-09-27T16:01:00Z' },
  ];
  const view = buildStatusView({ updates, logs: ['📋 Custom instructions: token=secret', '📝 Updated: /project/src/SignIn.tsx'] });
  expect(view.current).toBe('Adding the account screen');
  expect(view.recent).toEqual(['Reviewing the sign-in flow', 'Updated SignIn.tsx']);
  expect(JSON.stringify(view)).not.toContain('secret');
  expect(view.source).toBe('agent');
});

test('removes repeated markers from updates already held in memory', () => {
  const view = buildStatusView({ updates: [{ text: 'Earlier update GLOWBOM_STATUS: New detail GLOWBOM_STATUS:', source: 'agent', at: '2026-09-27T16:02:00Z' }], logs: [] });
  expect(view.current).toBe('New detail');
  expect(JSON.stringify(view)).not.toContain('GLOWBOM_STATUS');
});

test('falls back to known activity only and gives waiting requests priority', () => {
  const logs = ['Session created: abc123', '🔧 Running: bash {"command":"cat private.env"}', '📄 Modified: /project/src/Welcome.swift'];
  expect(buildStatusView({ updates: [], logs }).current).toBe('Updated Welcome.swift');
  expect(buildStatusView({ updates: [], logs, awaitingPermission: true }).current).toBe('Waiting for your permission');
  expect(buildStatusView({ updates: [], logs: ['Session created: abc123'] }).current).toBe('Starting the build');
});
