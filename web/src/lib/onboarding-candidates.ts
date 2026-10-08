import { isACPModel, isChatOnlyModel, isClaudeCodeModel, isCursorModel, isOpenCodeFreeModel } from './chat';
import type { ChatModel } from './chat';
import type { OnboardingCandidates, OnboardingPreference } from './onboarding';

export function candidateModels(models: ChatModel[], preference: OnboardingPreference, verifiedLocalModelIds: readonly string[] = []): OnboardingCandidates {
  const localIds = new Set(verifiedLocalModelIds);
  const local = (model: ChatModel) => model.id.startsWith('apple-intelligence/') || localIds.has(model.id);
  const byId = new Map<string, ChatModel>();
  for (const model of models) {
    if (!model.id) continue;
    const previous = byId.get(model.id);
    if (!previous) { byId.set(model.id, model); continue; }
    const previousLabel = `${previous.provider}\0${previous.name}`;
    const nextLabel = `${model.provider}\0${model.name}`;
    const display = nextLabel < previousLabel ? model : previous;
    // Conflicting capability records cannot establish image support.
    byId.set(model.id, { ...display, images: previous.images && model.images });
  }
  const available = [...byId.values()];
  const chat = available.filter((model) => !isCursorModel(model.id) && !isClaudeCodeModel(model.id) && !isACPModel(model.id) && !isOpenCodeFreeModel(model.id));
  const prototype = chat.filter((model) => model.images && !isChatOnlyModel(model.id));
  const build = available.filter((model) => !isChatOnlyModel(model.id));
  if (preference === 'existing') return { chat, prototype, build };
  return {
    chat: chat.filter(local),
    prototype: prototype.filter(local),
    build: build.filter((model) => local(model) || (preference === 'free' && isOpenCodeFreeModel(model.id))),
  };
}
