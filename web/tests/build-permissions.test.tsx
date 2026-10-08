import { expect, test } from 'bun:test';
import { renderToStaticMarkup } from 'react-dom/server';
import { BuildPermissionControl } from '../src/components/BuildPermissionControl';
import { useBuildPermissionChoice } from '../src/hooks/useBuildPermissionChoice';
import { agentPermissionResponses, allBuildPermissionScope } from '../src/lib/agent-permissions';
import type { OpenCodePermission } from '../src/types/opencode';
import { hookRunner } from './helpers/react-hook-runner';

test('changing agent, project, model, or reasoning scope clears an unaccepted broad approval', () => {
  let scope = 'project/codex/model/medium';
  const fixture = hookRunner(() => useBuildPermissionChoice(scope));
  expect(fixture.render().permissionMode).toBe('ask');
  for (const next of ['project/codex/model/high', 'project/acp/connection', 'other/acp/connection']) {
    fixture.render().setAllowAll(true);
    expect(fixture.render().permissionMode).toBe('all');
    scope = next;
    expect(fixture.render().permissionMode).toBe('ask');
  }
  fixture.render().setAllowAll(true);
  fixture.render().onAccepted();
  expect(fixture.render().permissionMode).toBe('ask');
  fixture.unmount();
});

test('only server-advertised permission scopes are offered for native agents', () => {
  const permission: OpenCodePermission = { id: 'one', sessionID: 'codex:thread', title: 'Run checks?', type: 'shell', message: '', pattern: '' };
  expect(agentPermissionResponses(permission)).toEqual(['once', 'reject']);
  expect(agentPermissionResponses({ ...permission, availableResponses: ['always', 'all', 'reject'] })).toEqual(['reject']);
  expect(agentPermissionResponses({ ...permission, availableResponses: ['once', 'all', 'reject'] })).toEqual(['once', 'all', 'reject']);
  expect(agentPermissionResponses({ ...permission, sessionID: 'acp:connection', availableResponses: ['once', 'build', 'reject'] })).toEqual(['once', 'build', 'reject']);
});

test('public build controls describe explicit permissions and Cursor automatic behavior', () => {
  for (const driver of ['opencode', 'codex', 'acp', 'claude-code']) {
    const html = renderToStaticMarkup(<BuildPermissionControl driver={driver} checked={false} onChange={() => {}} />);
    expect(html).toContain('type="checkbox"');
    expect(html).not.toContain('checked=""');
    expect(html).toContain(allBuildPermissionScope);
  }
  const html = renderToStaticMarkup(<BuildPermissionControl driver="cursor" checked={false} onChange={() => {}} />);
  expect(html).toContain('already runs tool requests automatically');
  expect(html).not.toContain('type="checkbox"');
});
