import { withServerAuthHeaders } from './server-auth';

export type CodexStatus = { installed: boolean; version?: string; running: boolean; connected: boolean; busy: boolean; loginPending?: boolean; error?: string };
export type CodexLogin = { authorizationURL: string; loginId: string };

export async function codexRequest<T>(path: 'status' | 'restart' | 'login' | 'login/cancel', body?: unknown, signal?: AbortSignal): Promise<T> {
  const response = await fetch(`/api/codex/${path}`, {
    method: body === undefined ? 'GET' : 'POST', signal, cache: 'no-store',
    headers: withServerAuthHeaders({ 'Content-Type': 'application/json' }),
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  const result = await response.json().catch(() => null);
  if (!response.ok || !result) throw new Error(result?.error || 'Could not connect to Codex. Try again.');
  return result as T;
}
