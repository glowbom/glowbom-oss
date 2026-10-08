import { withServerAuthHeaders } from './server-auth';
import { streamJsonSse } from './sse';
export interface ChatModel { id: string; name: string; provider: string; images: boolean; build?: boolean; reasoningEfforts?: string[]; defaultReasoningEffort?: string; isDefault?: boolean; free?: boolean; recommendedRank?: number }
export class ChatRequestError extends Error {
  constructor(message: string, readonly code = '') {
    super(message);
    this.name = 'ChatRequestError';
  }
}
export function chatAuthenticationProvider(error: unknown, requestedModel: string): string {
  if (!(error instanceof ChatRequestError) || error.code !== 'provider_auth') return '';
  const slash = requestedModel.indexOf('/');
  return slash > 0 && slash < requestedModel.length - 1 ? requestedModel.slice(0, slash) : '';
}
export function isOpenCodeFreeModel(id: string): boolean {
  const slash = id.indexOf('/');
  if (slash <= 0 || id.slice(0, slash) !== 'opencode') return false;
  const model = id.slice(slash + 1).toLowerCase();
  return model === 'big-pickle' || model.endsWith('-free');
}
export function isChatOnlyModel(id: string): boolean {
  return id.startsWith('apple-intelligence/') || id === 'ollama/maternion/mimo-v2.6:9b' || id === 'glowbom-ollama/maternion/mimo-v2.6:9b';
}
export const chatOnlyModelMessage = 'This model works in Chat only. Choose another model for Build or drawing.';
export function buildCapableModels(models: ChatModel[]): ChatModel[] {
  return models.filter((model) => !isCursorModel(model.id) && !isClaudeCodeModel(model.id) && !isACPModel(model.id) && !isChatOnlyModel(model.id) && model.build !== false);
}
export function isCodexModel(id: string): boolean {
  return /^codex\/[A-Za-z0-9._-]{1,128}$/.test(id);
}
export function codexModelID(id: string): string {
  return isCodexModel(id) ? id.slice('codex/'.length) : '';
}
export function isACPModel(id: string): boolean {
  return /^acp\/acp-[123]$/.test(id);
}
export function isClaudeCodeModel(id: string): boolean {
  return /^claude-code\/[A-Za-z0-9._-]{1,128}$/.test(id);
}
export function claudeCodeModelID(id: string): string {
  return isClaudeCodeModel(id) ? id.slice('claude-code/'.length) : '';
}
export function isCursorModel(id: string): boolean {
  return /^cursor\/[A-Za-z0-9._-]{1,128}$/.test(id);
}
export function cursorModelID(id: string): string {
  return isCursorModel(id) ? id.slice('cursor/'.length) : '';
}
function cursorNamed(models: ChatModel[], ids: string[], name: RegExp): ChatModel | undefined {
  for (const id of ids) {
    const found = models.find((model) => cursorModelID(model.id) === id);
    if (found) return found;
  }
  return models.find((model) => name.test(`${cursorModelID(model.id)} ${model.name}`));
}
export function featuredCursorModels(models: ChatModel[]): ChatModel[] {
  const cursor = models.filter((model) => isCursorModel(model.id));
  const picks = [
    cursorNamed(cursor, ['auto'], /^auto\b/i),
    cursorNamed(cursor, ['grok-4.7-xhigh-fast'], /grok[- ]4\.7/i),
    cursorNamed(cursor, ['cursor-grok-4.6-xhigh', 'grok-4.6-xhigh'], /grok[- ]4\.6/i),
    cursorNamed(cursor, ['claude-opus-5-high'], /claude[- ]opus/i),
    cursorNamed(cursor, ['gpt-5.6-sol-medium'], /gpt-5\.6/i),
  ];
  const seen = new Set<string>();
  return picks.filter((model): model is ChatModel => {
    if (!model || seen.has(model.id)) return false;
    seen.add(model.id);
    return true;
  });
}
export function visibleCursorModels(models: ChatModel[], extraIds: string[], selected = '', hiddenIds: string[] = []): ChatModel[] {
  const hidden = new Set(hiddenIds);
  const cursor = models.filter((model) => isCursorModel(model.id));
  const visible = featuredCursorModels(cursor).filter((model) => !hidden.has(model.id));
  const seen = new Set(visible.map((model) => model.id));
  for (const id of [...extraIds, selected]) {
    if (hidden.has(id)) continue;
    const model = cursor.find((item) => item.id === id);
    if (!model || seen.has(model.id)) continue;
    seen.add(model.id);
    visible.push(model);
  }
  return visible;
}
export const extraCursorModelsKey = 'glowbom_cursor_models';
export const hiddenCursorModelsKey = 'glowbom_cursor_hidden_models';
function readCursorIdList(key: string, storage?: Pick<Storage, 'getItem'>): string[] {
  try {
    const raw = (storage ?? localStorage).getItem(key);
    const parsed = raw ? JSON.parse(raw) : [];
    return Array.isArray(parsed) ? parsed.filter((id): id is string => typeof id === 'string' && isCursorModel(id)) : [];
  } catch {
    return [];
  }
}
export function readExtraCursorModels(storage?: Pick<Storage, 'getItem'>): string[] {
  return readCursorIdList(extraCursorModelsKey, storage);
}
export function readHiddenCursorModels(storage?: Pick<Storage, 'getItem'>): string[] {
  return readCursorIdList(hiddenCursorModelsKey, storage);
}
export function writeExtraCursorModels(ids: string[], storage?: Pick<Storage, 'setItem'>): void {
  try { (storage ?? localStorage).setItem(extraCursorModelsKey, JSON.stringify(ids)); } catch { /* The menu still shows the choice. */ }
}
export function writeHiddenCursorModels(ids: string[], storage?: Pick<Storage, 'setItem'>): void {
  try { (storage ?? localStorage).setItem(hiddenCursorModelsKey, JSON.stringify(ids)); } catch { /* The menu still shows the choice. */ }
}
export const addCursorModelsChoice = 'add-cursor-models';
export function chatCapableModels(models: ChatModel[], avoid = ''): ChatModel[] {
  const talk = models.filter((model) => !isOpenCodeFreeModel(model.id) && !isCursorModel(model.id) && !isClaudeCodeModel(model.id) && !isACPModel(model.id));
  const others = talk.filter((model) => model.id !== avoid);
  return others.length ? others : talk;
}
export function chatModelOptionLabel(model: ChatModel): string {
  const gateway = model.id.startsWith('explabs/');
  const recommended = typeof model.recommendedRank === 'number' && Number.isFinite(model.recommendedRank) && model.recommendedRank >= 0;
  const marks = [gateway && model.free === true ? 'Free' : '', gateway && recommended ? 'Recommended' : '', model.images ? 'Vision' : '', isOpenCodeFreeModel(model.id) || isClaudeCodeModel(model.id) || isACPModel(model.id) ? 'Build only' : '', isChatOnlyModel(model.id) ? 'Chat only' : ''].filter(Boolean);
  return marks.length ? `${model.name} · ${marks.join(' · ')}` : model.name;
}
export type WorkStatus = 'idle' | 'running' | 'completed' | 'failed' | 'cancelled';
export type WorkCardTone = 'working' | 'done' | 'problem' | 'warning' | 'idle';
export function workStatusLabel(status: WorkStatus, waiting: boolean): string {
  if (waiting) return 'Needs you';
  if (status === 'running') return 'Working';
  if (status === 'completed') return 'Completed';
  if (status === 'failed') return 'Could not finish';
  if (status === 'cancelled') return 'Stopped';
  return 'Ready';
}
export function workCardTone(status: WorkStatus, waiting: boolean, note = ''): WorkCardTone {
  if (waiting) return 'warning';
  if (status === 'failed') return 'problem';
  if (status === 'completed') return /\bwarning\b/i.test(note) ? 'warning' : 'done';
  if (status === 'running') return 'working';
  return 'idle';
}
export function workCardLabel(status: WorkStatus, waiting: boolean, note = ''): string {
  if (!waiting && status === 'completed' && workCardTone(status, waiting, note) === 'warning') return 'Warning';
  return workStatusLabel(status, waiting);
}
export const chatModelStorageKey = 'glowbom_chat_model';
export function readChatModelChoice(storage?: Pick<Storage, 'getItem'>): string {
  try {
    return (storage ?? localStorage).getItem(chatModelStorageKey)?.trim() || '';
  } catch {
    return '';
  }
}
export function groupChatModels(models: ChatModel[]): { provider: string; models: ChatModel[] }[] {
  const groups: { provider: string; models: ChatModel[] }[] = [];
  for (const model of models) {
    const provider = model.provider.trim() || 'Other';
    const existing = groups.find((group) => group.provider === provider);
    if (existing) existing.models.push(model);
    else groups.push({ provider, models: [model] });
  }
  const claudeIndex = groups.findIndex(group => group.provider === 'Claude Code');
  const cursorIndex = groups.findIndex(group => group.provider === 'Cursor');
  if (claudeIndex >= 0 && cursorIndex >= 0) {
    const [claude] = groups.splice(claudeIndex, 1);
    groups.splice(groups.findIndex(group => group.provider === 'Cursor') + 1, 0, claude!);
  }
  return groups;
}
export interface ChatMessage { role: 'user' | 'assistant'; text: string; model?: string; reasoning?: string; workedSeconds?: number; buildSeconds?: number }
export interface ChatImagePrompt { index: number; total: number; prompt: string; personalized?: boolean }
export interface ChatProgress { bookSource?: string; status?: string; reasoning?: string; warnings?: string[]; notices?: string[]; previewReady?: boolean; imagePrompts?: ChatImagePrompt[] }
export function chatModelLabel(model: { provider: string; name: string } | undefined, id: string): string {
  return (model ? `${model.provider} · ${model.name}` : id).trim().slice(0, 160);
}
export function assistantMessage(text: string, model: string): ChatMessage {
  const label = model.trim().slice(0, 160);
  return label ? { role: 'assistant', text, model: label } : { role: 'assistant', text };
}
export function visibleChatRequest(mode: 'chat' | 'prototype', text: string): string {
  if (mode !== 'prototype') return text;
  const request = text.split('User request:\n')[1]?.split('\n\nThe annotated browser')[0];
  return request || 'Create or update the prototype from my drawing.';
}
export function commitUserTurn(history: ChatMessage[], mode: 'chat' | 'prototype', text: string): { shown: ChatMessage[]; modelMessages: ChatMessage[] } {
  const shown = [...history, { role: 'user' as const, text: visibleChatRequest(mode, text) }];
  return { shown, modelMessages: [...history.map(({ role, text: prior }) => ({ role, text: prior })), { role: 'user', text }] };
}
// Keep saved history intact; only the model receives this recent context.
function modelChatMessages(messages: ChatMessage[]): ChatMessage[] {
  const selected: ChatMessage[] = [];
  const encoder = new TextEncoder();
  let bytes = 0;
  for (let index = messages.length - 1; index >= 0 && selected.length < 80; index--) {
    const message = messages[index]!;
    const size = encoder.encode(message.text).byteLength;
    if (bytes + size > 600000) {
      if (!selected.length) throw new ChatRequestError('This message is too long. Shorten it and try again.');
      break;
    }
    selected.push({ role: message.role, text: message.text });
    bytes += size;
  }
  return selected.reverse();
}
export interface ChatProject { path: string; name: string; version?: string; lastOpenedAt: string }
export function canonicalProjectPath(path: string): string {
  const trimmed = path.trim().normalize('NFC');
  if (!trimmed || trimmed === '/' || /^[A-Za-z]:\\$/.test(trimmed)) return trimmed;
  return trimmed.replace(/[/\\]+$/, '');
}
export function openedProjectPath(requested: string, resolved?: string): string {
  return canonicalProjectPath(resolved || '') || canonicalProjectPath(requested);
}
export function rememberChatProjects(current: ChatProject[], next: ChatProject, aliases: string[] = []): ChatProject[] {
  const path = canonicalProjectPath(next.path);
  if (!path) return current.slice(0, 10);
  const keys = new Set([path, ...aliases.map((item) => canonicalProjectPath(item)).filter(Boolean)]);
  return [{ ...next, path }, ...current.filter((item) => !keys.has(canonicalProjectPath(item.path)))].slice(0, 10);
}
export function recentChatProjects(): ChatProject[] {
  try {
    const value: unknown = JSON.parse(localStorage.getItem('glowbom_oss_project_history') || '[]');
    if (!Array.isArray(value)) return [];
    const ordered: ChatProject[] = [];
    const seen = new Set<string>();
    for (const item of value) {
      if (typeof item?.path !== 'string' || typeof item?.name !== 'string') continue;
      const path = canonicalProjectPath(item.path);
      if (!path || seen.has(path)) continue;
      seen.add(path);
      ordered.push({ ...item, path });
      if (ordered.length === 10) break;
    }
    if (ordered.length !== value.length || ordered.some((item, index) => item.path !== value[index]?.path)) {
      localStorage.setItem('glowbom_oss_project_history', JSON.stringify(ordered));
    }
    return ordered;
  } catch { return []; }
}
export async function chatJSON<T>(url: string, body?: object, signal?: AbortSignal): Promise<T> {
  const response = await fetch(`/api/chat/${url}`, { method: body ? 'POST' : 'GET', signal, headers: withServerAuthHeaders(body ? { 'Content-Type': 'application/json' } : undefined), body: body ? JSON.stringify(body) : undefined });
  if (!response.ok) throw new Error((await response.text()).trim() || 'Could not connect to OpenCode.');
  return response.json() as Promise<T>;
}
export async function streamChat(body: { projectPath: string; model: string; reasoningEffort?: string; mode: 'chat' | 'prototype' | 'translation'; agentState?: { status: 'idle' | 'running' | 'waiting'; driver?: 'opencode' | 'cursor' | 'claude-code' | 'codex' | 'acp'; steerAvailable?: boolean }; inputSketch?: import('./sketch-document').SketchDocument; lowEffort?: boolean; images?: import('./prototype-images').PrototypeImages; stack?: { id: string; name: string; description: string }; messages: ChatMessage[]; attachmentPaths: string[] }, signal: AbortSignal, onText: (text: string) => void, onProgress?: (progress: ChatProgress) => void): Promise<string> {
  if (isACPModel(body.model)) throw new ChatRequestError('ACP connections are available in Build only. Choose a different model for Chat or drawing.');
  if (isClaudeCodeModel(body.model)) throw new ChatRequestError('Claude Code is available in Build only. Choose a different model for Chat or drawing.');
  let completed = false; let failure = ''; let failureCode = ''; let text = '';
  let progress: ChatProgress = {};
  await streamJsonSse({ url: '/api/chat/stream', body: { ...body, messages: modelChatMessages(body.messages) }, signal, onEvent: (event: Record<string, unknown>) => {
    if (typeof event.bookSource === 'string') {
      progress = { ...progress, bookSource: event.bookSource };
      onProgress?.(progress);
    }
    if (typeof event.previewReady === 'boolean') {
      progress = { ...progress, previewReady: event.previewReady };
      onProgress?.(progress);
    }
    const warnings = [typeof event.warning === 'string' ? event.warning : '', ...(Array.isArray(event.warnings) ? event.warnings.filter((item): item is string => typeof item === 'string') : [])].filter(Boolean);
    if (warnings.length) {
      progress = { ...progress, warnings: [...new Set([...(progress.warnings || []), ...warnings])].slice(-8) };
      onProgress?.(progress);
    }
    if (typeof event.notice === 'string' && event.notice.trim()) {
      progress = { ...progress, notices: [...new Set([...(progress.notices || []), event.notice.trim().slice(0, 500)])].slice(-8) };
      onProgress?.(progress);
    }
    const reasoning = typeof event.reasoning === 'string' && event.reasoning.trim() ? event.reasoning.slice(-32768) : undefined;
    if (typeof event.status === 'string' || reasoning !== undefined) {
      progress = { ...progress, ...(typeof event.status === 'string' ? { status: event.status } : {}), ...(reasoning !== undefined ? { reasoning } : {}) };
      onProgress?.(progress);
    }
    const image = event.imagePrompt;
    if (image && typeof image === 'object' && 'index' in image && 'total' in image && 'prompt' in image
      && typeof image.index === 'number' && Number.isInteger(image.index) && image.index >= 1
      && typeof image.total === 'number' && Number.isInteger(image.total) && image.total <= 4 && image.index <= image.total
      && typeof image.prompt === 'string' && image.prompt.trim()) {
      const prompt: ChatImagePrompt = { index: image.index, total: image.total, prompt: image.prompt.slice(0, 2000), ...('personalized' in image && typeof image.personalized === 'boolean' ? { personalized: image.personalized } : {}) };
      progress = { ...progress, imagePrompts: [...(progress.imagePrompts || []).filter((item) => item.index !== prompt.index), prompt].sort((a, b) => a.index - b.index) };
      onProgress?.(progress);
    }
    if (typeof event.text === 'string') { text = event.text; onText(text); }
    if (event.done) {
      completed = event.success === true;
      failure = typeof event.error === 'string' ? event.error : '';
      failureCode = typeof event.code === 'string' ? event.code : '';
    }
  } });
  if (failure) throw new ChatRequestError(failure, failureCode);
  if (!completed) throw new ChatRequestError('The response stopped before completion. Your request is still available to retry.', failureCode);
  return text;
}
