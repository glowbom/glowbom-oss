import { codexModelID, isACPModel, isCodexModel, type ChatModel } from './chat';

export type BuildAgent = 'opencode' | 'cursor' | 'claude-code' | 'codex' | 'acp';

export function agentModels(models: ChatModel[], driver: BuildAgent): ChatModel[] {
  return models.filter(model => model.build !== false && (driver === 'codex' ? isCodexModel(model.id) : driver === 'acp' ? isACPModel(model.id) : false));
}

export function agentRequestModel(driver: BuildAgent, id: string): string {
  if (driver === 'codex') return codexModelID(id);
  if (driver === 'acp' && isACPModel(id)) return id;
  return '';
}
