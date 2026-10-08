import type { OpenCodePermission, OpenCodePermissionRespondRequest } from '../types/opencode';

type PermissionResponse = OpenCodePermissionRespondRequest['response'];
type AvailableResponse = NonNullable<OpenCodePermission['availableResponses']>[number];

export function normalizePermissionResponses(raw: unknown, sessionID = '', permissionID = ''): OpenCodePermission['availableResponses'] {
  if (!Array.isArray(raw)) return undefined;
  const nativeRememberScope = sessionID.startsWith('codex:') || sessionID.startsWith('acp:') || permissionID.startsWith('codex-') || permissionID.startsWith('acp-');
  return [...new Set(raw.filter((value): value is AvailableResponse => value === 'once' || (value === 'always' && !nativeRememberScope) || value === 'session' || value === 'build' || (value === 'all' && raw.includes('once')) || value === 'reject' || value === 'cancel'))];
}

export function agentPermissionResponses(permission: OpenCodePermission): PermissionResponse[] {
  const available = normalizePermissionResponses(permission.availableResponses, permission.sessionID, permission.id);
  if (available) return available.filter(response => response !== 'build' || permission.sessionID.startsWith('acp:'));
  if (permission.sessionID.startsWith('codex:') || permission.id.startsWith('codex-')) return ['once', 'reject'];
  return ['once', 'always', 'reject'];
}

export const permissionResponseLabels: Record<PermissionResponse, string> = {
  once: 'Allow once',
  always: 'Always allow',
  session: 'Allow for session',
  build: 'Allow for this build',
  all: 'Allow all for this build',
  reject: 'Do not allow',
  cancel: 'Cancel build',
};

export const allBuildPermissionScope = 'Automatically approves tool requests until this build ends. The agent can run commands and change files using your access to this computer.';
export const cursorPermissionScope = 'Cursor already runs tool requests automatically, subject to its local CLI deny rules.';
