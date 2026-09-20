import { withServerAuthHeaders } from './server-auth';

export interface AccountStatus {
  version: 1;
  status: 'signed_in' | 'signed_out' | 'unavailable';
  email?: string;
  subscriptionStatus?: string;
  code?: string;
}
export type LoginState = 'idle' | 'pending' | 'complete' | 'failed' | 'canceled' | 'canceling';

export function accountMessage(code?: string): string {
  switch (code) {
    case 'cli_unavailable': return 'Glowbom CLI was not found. Install it, then try again. You can also continue without an account.';
    case 'cli_update_required': return 'Update Glowbom CLI to connect your account. You can still continue locally.';
    case 'backend_auth_required': return 'Start the local app with glowbom start to connect your account securely.';
    case 'account_busy': return 'Another account operation is running. Wait a moment and try again.';
    default: return 'Could not check your Glowbom account. Try again or continue without an account.';
  }
}

export async function accountRequest<T>(path: string, method = 'GET', signal?: AbortSignal): Promise<T> {
  let response: Response;
  let result;
  try {
    response = await fetch(`/api/account/${path}`, { method, signal, headers: withServerAuthHeaders(), cache: 'no-store' });
    result = await response.json();
  } catch (error) {
    if (signal?.aborted) throw error;
    throw new Error('Could not reach the local Glowbom service. Start it with glowbom start, then try again.');
  }
  if (!result || typeof result !== 'object') throw new Error(accountMessage());
  if (!response.ok) throw new Error(accountMessage(result.code));
  if (result.version !== 1) throw new Error('Update Glowbom CLI to connect your account.');
  return result as T;
}
