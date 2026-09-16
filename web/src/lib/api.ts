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

export const openCodeApi = {
  async getHealth(agentDriver: 'opencode' | 'cursor' = 'opencode'): Promise<OpenCodeHealthResponse> {
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

  async pickProjectFolder(): Promise<OpenCodeProjectPickResponse> {
    return requestJson<OpenCodeProjectPickResponse>(`${API_PREFIX}/opencode/project/pick`, {
      method: 'POST',
    });
  },

  async pickInstructionFiles(): Promise<OpenCodeInstructionFilesPickResponse> {
    return requestJson<OpenCodeInstructionFilesPickResponse>(`${API_PREFIX}/opencode/instructions/files/pick`, {
      method: 'POST',
    });
  },

  async renameProject(path: string, name: string): Promise<{ success: boolean; project?: OpenCodeProjectEnvelope['project']; error?: string }> {
    return requestJson(`${API_PREFIX}/opencode/project/rename`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path, name }),
    });
  },

  async updateProjectSettings(payload: OpenCodeProjectSettingsRequest): Promise<OpenCodeProjectSettingsResponse> {
    return requestJson<OpenCodeProjectSettingsResponse>(`${API_PREFIX}/opencode/project/settings`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
  },

  async generateIcon(payload: OpenCodeGenerateIconRequest): Promise<OpenCodeGenerateIconResponse> {
    return requestJson<OpenCodeGenerateIconResponse>(`${API_PREFIX}/opencode/project/icon/generate`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
  },

  async getProjectIcon(path: string): Promise<{ success: boolean; exists: boolean; image?: string }> {
    return requestJson(`${API_PREFIX}/opencode/project/icon?path=${encodeURIComponent(path)}`);
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

  async respondToPermission(payload: OpenCodePermissionRespondRequest): Promise<{ ok: boolean }> {
    return requestJson<{ ok: boolean }>(`${API_PREFIX}/opencode/permission/respond`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
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
