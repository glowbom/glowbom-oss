import type {
  OpenCodeAvailableModelsRequest,
  OpenCodeAvailableModelsResponse,
  OpenCodeAuthConnectRequest,
  OpenCodeAuthConnectionResponse,
  OpenCodeAuthDisconnectRequest,
  OpenCodeAuthOAuthStartRequest,
  OpenCodeAuthOAuthStartResponse,
  OpenCodeAuthOAuthStatusResponse,
  OpenAIModelsRequest,
  OpenAIModelsResponse,
  OpenCodeAuthStatus,
  OpenCodeHealthResponse,
  OpenCodeInstructionFilesPickResponse,
  OpenCodeMediaApprovalRespondRequest,
  OpenCodeMediaApproval,
  OpenCodeProjectHistoryResponse,
  OpenCodeProjectIDEStatusResponse,
  OpenCodePermissionRespondRequest,
  OpenCodeProjectPickResponse,
  OpenCodeProjectEnvelope,
  OpenCodeProjectOpenRequest,
  OpenCodeProjectOpenResponse,
  OpenCodeQuestionRespondRequest,
  OpenCodeProjectSettingsRequest,
  OpenCodeProjectSettingsResponse,
  OpenCodeGenerateIconRequest,
  OpenCodeGenerateIconResponse,
  ProjectIconSourcesResponse,
  OpenCodePermission,
  OpenCodeQuestion,
} from '../types/opencode';
import { withServerAuthHeaders } from './server-auth';

const API_PREFIX = '/api';

export class ApiError extends Error {
  readonly status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

async function parseResponseError(response: Response): Promise<string> {
  const raw = await response.text();
  if (!raw) {
    return `Request failed with HTTP ${response.status}`;
  }

  try {
    const parsed = JSON.parse(raw) as { error?: string; message?: string };
    return parsed.error || parsed.message || `Request failed with HTTP ${response.status}`;
  } catch {
    return raw;
  }
}

async function requestJson<T>(input: RequestInfo | URL, init?: RequestInit): Promise<T> {
  const response = await fetch(input, {
    ...init,
    headers: withServerAuthHeaders(init?.headers),
  });
  if (!response.ok) {
    throw new ApiError(await parseResponseError(response), response.status);
  }
  return (await response.json()) as T;
}

export interface CompanionPairing {
  version: 1;
  name: string;
  url: string;
  token: string;
  certificateSHA256: string;
  expiresAt: string;
}

export interface CompanionStatus {
  active: boolean;
  interfaces: { address: string; name: string }[];
  pairing?: CompanionPairing;
  jobs: CompanionBuildJob[];
  buildModels?: CompanionBuildModel[];
  discovery?: { active: boolean; expiresAt?: string; message?: string };
  pairingRequests?: CompanionNearbyPairingRequest[];
}

export interface CompanionNearbyPairingRequest {
  id: string;
  deviceName: string;
  code: string;
  expiresAt: string;
}

export interface CompanionBuildModel {
  available: boolean;
  projectId?: string;
  projectPath?: string;
  model: string;
  revision: number;
  updatedAt?: string;
  source?: 'phone' | 'desktop';
}

export interface CompanionBuildJob {
  id: string;
  projectId: string;
  projectPath: string;
  kind?: string;
  source?: 'desktop' | 'phone';
  instructions?: string;
  model?: string;
  reasoningEffort?: string;
  targets?: string[];
  buildTargets?: string[];
  permissionMode?: 'ask' | 'all';
  agentDriver?: 'opencode' | 'cursor' | 'claude-code' | 'codex' | 'acp';
  agentName?: string;
  resultText?: string;
  runId?: string;
  buildStatus?: { text: string; source: 'agent' | 'activity' | 'system'; at: string };
  liveStatuses?: { text: string; source: 'agent' | 'activity' | 'system'; at: string }[];
  partialLine?: string;
  changedFiles?: string[];
  sessionID?: string;
  pendingMediaApproval?: OpenCodeMediaApproval;
  attachments?: { id: string; projectId: string; filename: string; mimeType: string; byteCount: number; width: number; height: number }[];
  resolvedDecisions?: { kind: 'permission' | 'question' | 'media'; id: string }[];
  status: string;
  output: string[];
  error?: string;
  startedAt: string;
  finishedAt?: string;
  pendingPermission?: OpenCodePermission;
  pendingQuestion?: OpenCodeQuestion;
}

export const companionApi = {
  getStatus(signal?: AbortSignal): Promise<CompanionStatus> {
    return requestJson(`${API_PREFIX}/companion`, { signal, cache: 'no-store' });
  },
  enable(payload: { address?: string; projectPaths: string[] }, signal?: AbortSignal): Promise<CompanionStatus> {
    return requestJson(`${API_PREFIX}/companion`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload), signal, cache: 'no-store',
    });
  },
  disable(signal?: AbortSignal): Promise<CompanionStatus> {
    return requestJson(`${API_PREFIX}/companion`, { method: 'DELETE', signal, cache: 'no-store' });
  },
  setNearbyPairing(active: boolean, signal?: AbortSignal): Promise<CompanionStatus> {
    return requestJson(`${API_PREFIX}/companion/pairing/discovery`, {
      method: active ? 'POST' : 'DELETE', signal, cache: 'no-store',
    });
  },
  respondToNearbyPairing(payload: { id: string; decision: 'approve' | 'reject'; code: string }, signal?: AbortSignal): Promise<CompanionStatus> {
    return requestJson(`${API_PREFIX}/companion/pairing/respond`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload), signal, cache: 'no-store',
    });
  },
  respondToBuild(payload: { jobId: string; kind: 'permission' | 'question'; id: string; response?: 'once' | 'all' | 'reject'; answer?: string; answers?: string[][]; answerByQuestionID?: Record<string, string[]> }, signal?: AbortSignal): Promise<CompanionStatus> {
    return requestJson(`${API_PREFIX}/companion/respond`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload), signal, cache: 'no-store',
    });
  },
  cancelBuild(jobId: string, signal?: AbortSignal): Promise<CompanionStatus> {
    return requestJson(`${API_PREFIX}/companion/cancel`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ jobId }), signal, cache: 'no-store',
    });
  },
  setBuildModel(projectPath: string, model: string, signal?: AbortSignal, expectedRevision?: number): Promise<CompanionBuildModel> {
    return requestJson(`${API_PREFIX}/companion/build-model`, {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ projectPath, model, ...(expectedRevision === undefined ? {} : { expectedRevision }) }), signal, cache: 'no-store',
    });
  },
};

export const openCodeApi = {
  async checkProjectFolder(parentPath: string, name: string, signal: AbortSignal): Promise<{ success: boolean; path: string; exists: boolean; suggestedName?: string }> {
    return requestJson(`${API_PREFIX}/opencode/project/check`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, signal,
      body: JSON.stringify({ parentPath, name }),
    });
  },
  async createProject(parentPath: string, name: string): Promise<{ success: boolean; path: string }> {
    return requestJson(`${API_PREFIX}/opencode/project/create`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ parentPath, name }),
    });
  },
  async getHealth(agentDriver: 'opencode' | 'cursor' | 'claude-code' | 'codex' | 'acp' = 'opencode'): Promise<OpenCodeHealthResponse> {
    return requestJson<OpenCodeHealthResponse>(`${API_PREFIX}/opencode/health?agentDriver=${agentDriver}`);
  },

  async getAuthStatus(): Promise<OpenCodeAuthStatus> {
    return requestJson<OpenCodeAuthStatus>(`${API_PREFIX}/opencode/auth/status`);
  },

  async connectOpenAIAuth(payload: OpenCodeAuthConnectRequest): Promise<OpenCodeAuthConnectionResponse> {
    return requestJson<OpenCodeAuthConnectionResponse>(`${API_PREFIX}/opencode/auth/openai/connect`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
  },

  async disconnectOpenAIAuth(payload: OpenCodeAuthDisconnectRequest): Promise<OpenCodeAuthConnectionResponse> {
    return requestJson<OpenCodeAuthConnectionResponse>(`${API_PREFIX}/opencode/auth/openai/disconnect`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
  },

  async startOpenAIOAuth(payload: OpenCodeAuthOAuthStartRequest): Promise<OpenCodeAuthOAuthStartResponse> {
    return requestJson<OpenCodeAuthOAuthStartResponse>(`${API_PREFIX}/opencode/auth/openai/oauth/start`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
  },

  async getOpenAIOAuthStatus(state: string): Promise<OpenCodeAuthOAuthStatusResponse> {
    const search = new URLSearchParams({ state });
    return requestJson<OpenCodeAuthOAuthStatusResponse>(`${API_PREFIX}/opencode/auth/openai/oauth/status?${search.toString()}`);
  },

  async getProject(projectPath: string): Promise<OpenCodeProjectEnvelope> {
    const search = new URLSearchParams({ path: projectPath });
    return requestJson<OpenCodeProjectEnvelope>(`${API_PREFIX}/opencode/project?${search.toString()}`);
  },

  async getProjectHistory(projectPath: string): Promise<OpenCodeProjectHistoryResponse> {
    const search = new URLSearchParams({ path: projectPath });
    return requestJson<OpenCodeProjectHistoryResponse>(`${API_PREFIX}/opencode/project/history?${search.toString()}`);
  },

  async getProjectIDEStatus(projectPath: string): Promise<OpenCodeProjectIDEStatusResponse> {
    const search = new URLSearchParams({ path: projectPath });
    return requestJson<OpenCodeProjectIDEStatusResponse>(`${API_PREFIX}/opencode/project/ide/status?${search.toString()}`);
  },

  async pickProjectFolder(path?: string, purpose?: 'save'): Promise<OpenCodeProjectPickResponse> {
    const payload: { path?: string; purpose?: 'save' } = {};
    if (path) payload.path = path;
    if (purpose) payload.purpose = purpose;
    const hasPayload = Boolean(path || purpose);
    return requestJson<OpenCodeProjectPickResponse>(`${API_PREFIX}/opencode/project/pick`, {
      method: 'POST',
      headers: hasPayload ? { 'Content-Type': 'application/json' } : undefined,
      body: hasPayload ? JSON.stringify(payload) : undefined,
    });
  },

  async pickInstructionFiles(): Promise<OpenCodeInstructionFilesPickResponse> {
    return requestJson<OpenCodeInstructionFilesPickResponse>(`${API_PREFIX}/opencode/instructions/files/pick`, {
      method: 'POST',
    });
  },

  async renameProject(path: string, name: string, signal?: AbortSignal, expectedName?: string): Promise<{ success: boolean; project?: OpenCodeProjectEnvelope['project']; error?: string }> {
    return requestJson(`${API_PREFIX}/opencode/project/rename`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path, name, expectedName }),
      signal,
    });
  },

  async updateProjectSettings(payload: OpenCodeProjectSettingsRequest): Promise<OpenCodeProjectSettingsResponse> {
    return requestJson<OpenCodeProjectSettingsResponse>(`${API_PREFIX}/opencode/project/settings`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
  },

  async generateIcon(payload: OpenCodeGenerateIconRequest, signal?: AbortSignal): Promise<OpenCodeGenerateIconResponse> {
    return requestJson<OpenCodeGenerateIconResponse>(`${API_PREFIX}/opencode/project/icon/generate`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
      signal,
    });
  },

  async getProjectIcon(path: string, signal?: AbortSignal): Promise<{ success: boolean; exists: boolean; image?: string; error?: string }> {
    return requestJson(`${API_PREFIX}/opencode/project/icon?path=${encodeURIComponent(path)}`, { signal });
  },

  async getIconSources(signal?: AbortSignal): Promise<ProjectIconSourcesResponse> {
    for (let attempt = 0; ; attempt++) {
      const result = await requestJson<ProjectIconSourcesResponse>(`${API_PREFIX}/opencode/project/icon/sources`, { signal, cache: 'no-store' });
      if (attempt >= 2 || !result.sources.some((source) => source.availabilityCode === 'account_busy')) return result;
      await new Promise<void>((resolve, reject) => {
        const abort = () => { clearTimeout(timer); reject(signal?.reason ?? new DOMException('Aborted', 'AbortError')); };
        const timer = setTimeout(() => { signal?.removeEventListener('abort', abort); resolve(); }, 500);
        if (signal?.aborted) abort();
        else signal?.addEventListener('abort', abort, { once: true });
      });
    }
  },

  async openProject(payload: OpenCodeProjectOpenRequest): Promise<OpenCodeProjectOpenResponse> {
    return requestJson<OpenCodeProjectOpenResponse>(`${API_PREFIX}/opencode/project/open`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
  },

  async fetchOpenAIModels(payload: OpenAIModelsRequest): Promise<OpenAIModelsResponse> {
    return requestJson<OpenAIModelsResponse>(`${API_PREFIX}/providers/openai/models`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
  },

  async fetchOpenCodeAvailableModels(payload: OpenCodeAvailableModelsRequest): Promise<OpenCodeAvailableModelsResponse> {
    return requestJson<OpenCodeAvailableModelsResponse>(`${API_PREFIX}/opencode/models/available`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
  },

  async respondToQuestion(payload: OpenCodeQuestionRespondRequest): Promise<{ ok: boolean }> {
    return requestJson<{ ok: boolean }>(`${API_PREFIX}/opencode/question/respond`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
  },

  async steerChatMessage(payload: { projectPath: string; runId: string; messageIndex: number; role: 'user' | 'assistant'; text: string }): Promise<{ status: 'sent' | 'queued'; runId: string; messageIndex: number }> {
    return requestJson(`${API_PREFIX}/opencode/steer`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
  },

  async respondToPermission(payload: OpenCodePermissionRespondRequest, signal?: AbortSignal): Promise<{ ok: boolean }> {
    return requestJson<{ ok: boolean }>(`${API_PREFIX}/opencode/permission/respond`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
      signal,
    });
  },

  async respondToMediaApproval(payload: OpenCodeMediaApprovalRespondRequest): Promise<{ ok: boolean }> {
    return requestJson<{ ok: boolean }>(`${API_PREFIX}/opencode/media/approval/respond`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
  },
};

export function toErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof Error && error.message.trim()) {
    return error.message.trim();
  }
  if (typeof error === 'string' && error.trim()) {
    return error.trim();
  }
  return fallback;
}
