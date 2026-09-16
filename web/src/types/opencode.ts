export type OpenAIAuthMode = 'api-key' | 'codex-jwt' | 'opencode-config';
export type AuthProviderID = 'chatgpt';

export type ProviderID =
  | 'ollama'
  | 'xai'
  | 'fireworks'
  | 'openrouter'
  | 'opencode'
  | 'openai'
  | 'anthropic'
  | 'google';

export type ProviderKeyField =
  | 'openaiKey'
  | 'anthropicKey'
  | 'geminiKey'
  | 'fireworksKey'
  | 'openrouterKey'
  | 'opencodeZenKey'
  | 'xaiKey'
  | 'elevenLabsKey';

export interface ProviderKeyState {
  openaiKey: string;
  anthropicKey: string;
  geminiKey: string;
  fireworksKey: string;
  openrouterKey: string;
  opencodeZenKey: string;
  xaiKey: string;
  elevenLabsKey: string;
}

export type ImageProviderID = 'openai' | 'gemini' | 'xai';

export interface ImageProviderKeyState {
  openaiImageKey: string;
  geminiImageKey: string;
  xaiImageKey: string;
}

export interface OpenCodeHealthResponse {
  healthy: boolean;
  server?: string;
  error?: string;
  hint?: string;
}

export interface OpenCodeAuthStatus {
  serverRunning: boolean;
  configuredDataHome: string;
  configuredStateHome: string;
  authFilePath: string;
  openaiCredentialType: 'none' | 'oauth' | 'api' | 'unknown';
  cachedGlowbomAuthMode: 'api-key' | 'codex-jwt' | 'opencode-config' | 'unknown';
}

export interface OpenCodeAuthConnectRequest {
  projectPath?: string;
  openaiKey?: string;
  openaiRefreshToken?: string;
  openaiExpiresAt?: number;
}

export interface OpenCodeAuthDisconnectRequest {
  projectPath?: string;
}

export interface OpenCodeAuthConnectionResponse {
  success: boolean;
  provider: AuthProviderID;
  connected: boolean;
  status: OpenCodeAuthStatus;
  error?: string;
}

export interface OpenCodeAuthOAuthStartRequest {
  projectPath?: string;
}

export interface OpenCodeAuthOAuthStartResponse {
  success: boolean;
  provider: AuthProviderID;
  state: string;
  authorizationURL: string;
  redirectURI: string;
  error?: string;
}

export interface OpenCodeAuthOAuthStatusResponse {
  success: boolean;
  provider: AuthProviderID;
  state: string;
  phase: 'pending' | 'succeeded' | 'failed';
  connected: boolean;
  status: OpenCodeAuthStatus;
  error?: string;
}

export interface GlowbomTarget {
  enabled?: boolean;
  outputDir?: string;
  lastBuild?: string;
  stack?: string;
  status?: string;
  error?: string;
}

export interface OpenCodeProject {
  name: string;
  version: string;
  description?: string;
  bundleID?: string;
  displayName?: string;
  buildNumber?: string;
  targets: Record<string, GlowbomTarget>;
  createdAt: string;
  updatedAt: string;
}

export interface OpenCodeProjectSettingsRequest {
  path: string;
  bundleID?: string;
  displayName?: string;
  buildNumber?: string;
  version?: string;
}

export interface OpenCodeProjectSettingsResponse {
  success: boolean;
  project?: OpenCodeProject;
  error?: string;
}

export interface OpenCodeGenerateIconRequest {
  path: string;
  prompt: string;
  imageSource?: string;
  openaiKey?: string;
  geminiKey?: string;
  xaiKey?: string;
  referenceImage?: string;
}

export interface OpenCodeGenerateIconResponse {
  success: boolean;
  iconPath?: string;
  sourceService?: string;
  image?: string;
  error?: string;
}

export interface OpenCodeProjectEnvelope {
  success: boolean;
  project?: OpenCodeProject;
  paths?: Record<string, string>;
  existingTargets?: string[];
  error?: string;
}

export interface OpenCodeProjectPickResponse {
  success: boolean;
  path?: string;
  canceled?: boolean;
  source?: string;
  error?: string;
}

export interface OpenCodeInstructionPickedFile {
  path: string;
  name: string;
  sizeBytes: number;
  mimeType?: string;
}

export type OpenCodeHistoryStatus = 'running' | 'completed' | 'failed' | 'cancelled';

export interface OpenCodeProjectHistoryAttachment extends OpenCodeInstructionPickedFile {
  filename: string;
  mediaType?: string;
  relativePath?: string;
}

export interface OpenCodeProjectHistoryEntry {
  id: string;
  timestamp: string;
  instructions: string;
  taskType: string;
  status?: OpenCodeHistoryStatus | string;
  outputSummary?: string;
  folderName: string;
  missingAttachmentCount?: number;
  attachments?: OpenCodeProjectHistoryAttachment[];
}

export interface OpenCodeProjectHistoryResponse {
  success: boolean;
  path?: string;
  entries?: OpenCodeProjectHistoryEntry[];
  error?: string;
}

export interface OpenCodeInstructionFilesPickResponse {
  success: boolean;
  files?: OpenCodeInstructionPickedFile[];
  canceled?: boolean;
  source?: string;
  error?: string;
}

export type OpenCodeIDE = 'finder' | 'xcode' | 'android-studio' | 'vscode';

export interface OpenCodeProjectIDEAction {
  ide: OpenCodeIDE;
  label: string;
  available: boolean;
  path?: string;
  reason?: string;
}

export interface OpenCodeProjectIDEStatusResponse {
  success: boolean;
  path?: string;
  actions?: OpenCodeProjectIDEAction[];
  error?: string;
}

export interface OpenCodeProjectOpenRequest {
  path: string;
  ide: OpenCodeIDE;
}

export interface OpenCodeProjectOpenResponse {
  success: boolean;
  ide?: OpenCodeIDE;
  openedPath?: string;
  error?: string;
}

export interface OpenCodeAgentRequest {
  agentDriver?: 'opencode' | 'cursor';
  projectPath: string;
  sessionID?: string;
  instructions?: string;
  persistCurrentInstructionsToHistory?: boolean;
  instructionAttachmentPaths?: string[];
  model?: string;
  openaiKey?: string;
  openaiImageKey?: string;
  openaiAuthMode?: OpenAIAuthMode;
  openaiRefreshToken?: string;
  openaiExpiresAt?: number;
  anthropicKey?: string;
  geminiKey?: string;
  geminiImageKey?: string;
  fireworksKey?: string;
  openrouterKey?: string;
  opencodeZenKey?: string;
  xaiKey?: string;
  xaiImageKey?: string;
  elevenLabsKey?: string;
  imageSource?: string;
  mediaGenerationPolicy?: 'auto' | 'ask' | 'skip';
}

export interface OpenCodeQuestionOption {
  id: string;
  label: string;
  description: string;
}

export interface OpenCodeQuestionItem {
  id: string;
  prompt: string;
  options: OpenCodeQuestionOption[];
  inputType?: string;
}

export interface OpenCodeQuestion {
  id: string;
  sessionID: string;
  prompt: string;
  options: OpenCodeQuestionOption[];
  questions: OpenCodeQuestionItem[];
}

export interface OpenCodePermission {
  id: string;
  sessionID: string;
  title: string;
  type: string;
  message: string;
  pattern: string;
}

export interface OpenCodeQuestionRespondRequest {
  sessionID: string;
  questionID: string;
  answer: string;
  projectPath?: string;
  answers?: string[][];
  answerByQuestionID?: Record<string, string[]>;
}

export interface OpenCodePermissionRespondRequest {
  sessionID: string;
  permissionID: string;
  response: 'once' | 'always' | 'reject';
  projectPath?: string;
}

export interface OpenCodeMediaApprovalItem {
  mediaType: 'image' | 'video' | 'audio' | string;
  prompt: string;
  provider: string;
  audioType?: 'voice' | 'sound' | 'music' | string;
}

export interface OpenCodeMediaApproval {
  id: string;
  title: string;
  message: string;
  items: OpenCodeMediaApprovalItem[];
}

export interface OpenCodeMediaApprovalRespondRequest {
  approvalID: string;
  response: 'generate' | 'skip';
  projectPath?: string;
}

export interface OpenCodeSseEvent {
  output?: string;
  outputChunk?: string;
  done?: boolean;
  success?: boolean;
  error?: string;
  files?: number;
  changedFiles?: string[];
  question?: unknown;
  permission?: unknown;
  mediaApproval?: unknown;
  [key: string]: unknown;
}

export interface OpenAIModelsRequest {
  projectPath?: string;
  openaiKey?: string;
  openaiAuthMode: OpenAIAuthMode;
  openaiRefreshToken?: string;
  openaiExpiresAt?: number;
}

export interface OpenAIModelsResponseModel {
  id: string;
  displayName: string;
}

export interface OpenAIModelsResponseDebug {
  authMode: string;
  providerFound: boolean;
  providerModelCount: number;
  providerModelSample?: string[];
  allowlistModelIDs: string[];
  matchedModelIDs: string[];
  usedFallbackAllowlist: boolean;
}

export interface OpenAIModelsResponse {
  provider: string;
  source: string;
  models: OpenAIModelsResponseModel[];
  fetchedAt: string;
  debug?: OpenAIModelsResponseDebug;
}

export interface OpenCodeAvailableModelsRequest {
  projectPath?: string;
  openaiKey?: string;
  openaiAuthMode?: OpenAIAuthMode;
  openaiRefreshToken?: string;
  openaiExpiresAt?: number;
  anthropicKey?: string;
  geminiKey?: string;
  fireworksKey?: string;
  openrouterKey?: string;
  opencodeZenKey?: string;
  xaiKey?: string;
}

export interface OpenCodeAvailableProviderModel {
  id: string;
  displayName: string;
}

export interface OpenCodeAvailableProvider {
  id: string;
  displayName: string;
  models: OpenCodeAvailableProviderModel[];
}

export interface OpenCodeAvailableModelsResponse {
  source: string;
  providers: OpenCodeAvailableProvider[];
  fetchedAt: string;
}

export interface ModelCatalogProvider {
  id: ProviderID;
  label: string;
  requiresKey: boolean;
  keyField?: ProviderKeyField;
  keyLabel?: string;
}

export interface ModelCatalogEntry {
  providerId: ProviderID;
  id: string;
  label: string;
  description: string;
  isDynamic?: boolean;
}

export interface ModelCatalogGroup {
  provider: ModelCatalogProvider;
  models: ModelCatalogEntry[];
}

export interface ResolvedRefineModel {
  providerId: ProviderID;
  modelId: string;
  value: string;
  label: string;
}
