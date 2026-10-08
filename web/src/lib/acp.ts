import { withServerAuthHeaders } from './server-auth';

export const acpConnectionIDs = ['acp-1', 'acp-2', 'acp-3'] as const;
export type ACPConnectionID = typeof acpConnectionIDs[number];
export type ACPConnection = { id: ACPConnectionID; name: string; command: string; args: string[]; model?: string };
export type ACPModelOption = { id: string; name: string; description?: string };
export type ACPProbeResult = { name: string; version: string; images: boolean; loadSession: boolean; authRequired: boolean; model?: string; models?: ACPModelOption[] };

export function acpArguments(text: string): string[] {
  return text.replace(/\r\n/g, '\n').split('\n').filter(line => line !== '');
}

export function readACPConnections(value: unknown): ACPConnection[] {
  if (!value || typeof value !== 'object' || !Array.isArray((value as { connections?: unknown }).connections)) {
    throw new Error('Could not read the saved ACP connections.');
  }
  const connections = (value as { connections: unknown[] }).connections;
  const seen = new Set<string>();
  if (connections.length > 3) throw new Error('Could not read the saved ACP connections.');
  return connections.map(item => {
    if (!item || typeof item !== 'object') throw new Error('Could not read the saved ACP connections.');
    const connection = item as Partial<ACPConnection>;
    if (!acpConnectionIDs.includes(connection.id as ACPConnectionID) || seen.has(connection.id!)
      || typeof connection.name !== 'string' || typeof connection.command !== 'string'
      || !Array.isArray(connection.args) || connection.args.some(arg => typeof arg !== 'string')
      || (connection.model !== undefined && typeof connection.model !== 'string')) {
      throw new Error('Could not read the saved ACP connections.');
    }
    seen.add(connection.id!);
    return { id: connection.id!, name: connection.name, command: connection.command, args: [...connection.args], ...(connection.model === undefined ? {} : { model: connection.model }) };
  }).sort((a, b) => a.id.localeCompare(b.id));
}

async function acpRequest(path: '' | '/test', method: 'GET' | 'PUT' | 'POST', body?: unknown, signal?: AbortSignal): Promise<unknown> {
  const response = await fetch(`/api/settings/acp${path}`, {
    method, signal, cache: 'no-store',
    headers: withServerAuthHeaders({ 'Content-Type': 'application/json' }),
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  const result = await response.json().catch(() => null);
  if (!response.ok || !result) throw new Error(typeof result?.error === 'string' ? result.error : 'Could not update the ACP connections. Try again.');
  return result;
}

export async function fetchACPConnections(signal?: AbortSignal): Promise<ACPConnection[]> {
  return readACPConnections(await acpRequest('', 'GET', undefined, signal));
}

export async function saveACPConnections(connections: ACPConnection[], signal?: AbortSignal): Promise<ACPConnection[]> {
  const saved = readACPConnections(await acpRequest('', 'PUT', { connections }, signal));
  if (typeof window !== 'undefined') window.dispatchEvent(new Event('glowbom-acp-changed'));
  return saved;
}

export async function testACPConnection(connection: ACPConnection, signal?: AbortSignal): Promise<ACPProbeResult> {
  const probeConnection = { ...connection };
  delete probeConnection.model;
  const value = await acpRequest('/test', 'POST', { connection: probeConnection }, signal) as Partial<ACPProbeResult>;
  if (typeof value.name !== 'string' || typeof value.version !== 'string' || typeof value.images !== 'boolean'
    || typeof value.loadSession !== 'boolean' || typeof value.authRequired !== 'boolean'
    || (value.model !== undefined && typeof value.model !== 'string')
    || (value.models !== undefined && (!Array.isArray(value.models) || value.models.some(model => !model || typeof model !== 'object'
      || typeof model.id !== 'string' || !model.id.trim() || typeof model.name !== 'string'
      || (model.description !== undefined && typeof model.description !== 'string'))
      || new Set(value.models.map(model => model.id)).size !== value.models.length))) {
    throw new Error('Could not read the ACP connection test.');
  }
  return value as ACPProbeResult;
}
