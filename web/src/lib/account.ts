import { withServerAuthHeaders } from './server-auth';

export interface AccountStatus {
  version: 1;
  status: 'signed_in' | 'signed_out' | 'unavailable';
  email?: string;
  subscriptionStatus?: string;
  remainingCredits?: number;
  allowanceCredits?: number;
  code?: string;
}
export type LoginState = 'idle' | 'pending' | 'complete' | 'failed' | 'canceled' | 'canceling';
export const accountStatusChangedEvent = 'glowbom-account-status-changed';

export class AccountRequestError extends Error {
  constructor(readonly code?: string) { super(accountMessage(code)); }
}

export function accountMessage(code?: string): string {
  switch (code) {
    case 'cli_unavailable': return 'Glowbom CLI was not found. Install it, then try again. You can also continue without an account.';
    case 'cli_update_required': return 'Update Glowbom CLI to connect your account. You can still continue locally.';
    case 'backend_auth_required': return 'Start the local app with glowbom start to connect your account securely.';
    case 'account_busy': return 'Another account operation is running. Wait a moment and try again.';
    case 'sign_in_required': return 'Sign in with Glowbom to download your current project.';
    case 'project_not_found': return 'Save a project in Glowbom first, then download it here.';
    case 'project_incomplete': return 'That save is still finishing. Wait a moment, then try again.';
    case 'project_too_large': return 'This project is too large to download.';
    case 'invalid_project': return 'The saved project could not be read. Save it again in Glowbom.';
    case 'rate_limited': return 'Too many downloads. Wait a minute and try again.';
    case 'download_denied': return 'Glowbom did not allow this download. Check your account and try again.';
    case 'invalid_output': return 'Choose a folder you can save into.';
    case 'output_exists': return 'Choose another folder. There is already a Glowbom project there.';
    case 'starter_unavailable': return 'The project starter could not be downloaded. Check your connection and try again.';
    case 'export_timeout': return 'The download took too long. Try again.';
    case 'canceled': return 'Download canceled.';
    case 'download_failed':
    case 'export_failed': return 'The project could not be downloaded. Try again.';
    default: return 'Could not check your Glowbom account. Try again or continue without an account.';
  }
}

export interface AccountProject {
  version: 1;
  path: string;
  files?: number;
  prompt?: string;
  drawing?: string;
  drawingType?: string;
}

const lastExportFolderKey = 'glowbom_last_export_folder';
const drawingExtensions = { 'image/png': 'png', 'image/jpeg': 'jpg', 'image/webp': 'webp' } as const;

export function readLastExportFolder(): string {
  try { return localStorage.getItem(lastExportFolderKey)?.trim() || ''; }
  catch { return ''; }
}

export function writeLastExportFolder(path: string) {
  const folder = path.trim();
  if (!folder) return;
  try { localStorage.setItem(lastExportFolderKey, folder); }
  catch { /* The chooser still opens in the home folder. */ }
}

export function accountProjectDraft(project: { prompt?: unknown; drawing?: unknown; drawingType?: unknown }): { prompt: string; drawing?: File } {
  const prompt = typeof project.prompt === 'string' ? project.prompt : '';
  const type = typeof project.drawingType === 'string' ? project.drawingType : '';
  const encoded = typeof project.drawing === 'string' ? project.drawing : '';
  const extension = drawingExtensions[type as keyof typeof drawingExtensions];
  if (!extension || !encoded || encoded.length > 14_000_000) return { prompt };
  try {
    const binary = atob(encoded);
    if (!binary.length || binary.length > 10 * 1024 * 1024) return { prompt };
    const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0));
    return { prompt, drawing: new File([bytes], `sketch.${extension}`, { type }) };
  } catch {
    return { prompt };
  }
}

export async function accountRequest<T>(path: string, method = 'GET', signal?: AbortSignal, body?: unknown): Promise<T> {
  let response: Response;
  let result;
  try {
    response = await fetch(`/api/account/${path}`, {
      method,
      signal,
      headers: withServerAuthHeaders(body === undefined ? undefined : { 'Content-Type': 'application/json' }),
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: 'no-store',
    });
    result = await response.json();
  } catch (error) {
    if (signal?.aborted) throw error;
    throw new Error('Could not reach the local Glowbom service. Start it with glowbom start, then try again.');
  }
  if (!result || typeof result !== 'object') throw new Error(accountMessage());
  if (!response.ok) throw new AccountRequestError(result.code);
  if (result.version !== 1) throw new Error('Update Glowbom CLI to connect your account.');
  if (!signal?.aborted && typeof window !== 'undefined' && (path === 'status' || path === 'logout') && (result.status === 'signed_in' || result.status === 'signed_out')) {
    window.dispatchEvent(new Event(accountStatusChangedEvent));
  }
  return result as T;
}

export function formatGlowbomCredits(value: unknown): string {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) return 'Unavailable';
  return new Intl.NumberFormat('en-US', { maximumFractionDigits: 0 }).format(Math.floor(value));
}
