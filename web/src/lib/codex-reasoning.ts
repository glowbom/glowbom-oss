import { isCodexModel, type ChatModel } from './chat';

export type CodexReasoningChoices = Record<string, string>;
export const codexReasoningStorageKey = 'glowbom_codex_reasoning_efforts';
const labels: Record<string, string> = { none: 'None', minimal: 'Minimal', light: 'Light', low: 'Low', medium: 'Medium', high: 'High', xhigh: 'Extra high', max: 'Max', ultra: 'Ultra' };

export function reasoningEffortLabel(effort: string): string {
  return labels[effort] || effort.replace(/[-_]/g, ' ').replace(/^./, letter => letter.toUpperCase());
}

export function codexReasoningEfforts(model?: ChatModel): string[] {
  if (!model || !isCodexModel(model.id)) return [];
  return [...new Set((model.reasoningEfforts || []).filter(effort => typeof effort === 'string' && /^[a-z][a-z0-9_-]{0,31}$/.test(effort)))];
}

export function selectedCodexReasoningEffort(model: ChatModel | undefined, choices: CodexReasoningChoices): string | undefined {
  const efforts = codexReasoningEfforts(model);
  if (!model || !efforts.length) return undefined;
  if (efforts.includes(choices[model.id] || '')) return choices[model.id];
  if (efforts.includes(model.defaultReasoningEffort || '')) return model.defaultReasoningEffort;
  return efforts[0];
}

export function reconcileCodexReasoningChoices(models: ChatModel[], choices: CodexReasoningChoices): CodexReasoningChoices {
  let next = choices;
  for (const model of models) {
    if (!isCodexModel(model.id) || !(model.id in choices)) continue;
    const effort = selectedCodexReasoningEffort(model, choices);
    if (effort === choices[model.id]) continue;
    if (next === choices) next = { ...choices };
    if (effort) next[model.id] = effort; else delete next[model.id];
  }
  return next;
}

export function readCodexReasoningChoices(storage?: Pick<Storage, 'getItem'>): CodexReasoningChoices {
  try {
    const value: unknown = JSON.parse((storage ?? localStorage).getItem(codexReasoningStorageKey) || '{}');
    if (!value || typeof value !== 'object' || Array.isArray(value)) return {};
    return Object.fromEntries(Object.entries(value).filter(([model, effort]) => isCodexModel(model) && typeof effort === 'string' && /^[a-z][a-z0-9_-]{0,31}$/.test(effort)));
  } catch { return {}; }
}

export function writeCodexReasoningChoices(choices: CodexReasoningChoices, storage?: Pick<Storage, 'setItem'>): void {
  try { (storage ?? localStorage).setItem(codexReasoningStorageKey, JSON.stringify(choices)); }
  catch { /* Keep the current choices usable if storage is unavailable. */ }
}

// Upgrade the previous default once when the current catalog exposes its replacement.
export function preferredCodexModelChoice(current: string, models: ChatModel[]): string {
  if (!isCodexModel(current)) return current;
  const selected = models.find(model => model.id === current);
  if (selected && !selected.isDefault && !/^codex\/gpt-5(?:[.-]|$)/.test(current)) return current;
  return models.find(model => model.id === 'codex/gpt-6.1-sol')?.id
    || selected?.id || models.find(model => isCodexModel(model.id) && model.isDefault)?.id || current;
}

const codexDefaultMigrationKey = 'glowbom_codex_gpt61_default_v1';
export function readCodexDefaultMigration(storage?: Pick<Storage, 'getItem'>): boolean {
  try { return (storage ?? localStorage).getItem(codexDefaultMigrationKey) === 'true'; } catch { return false; }
}
export function rememberCodexDefaultMigration(storage?: Pick<Storage, 'setItem'>): void {
  try { (storage ?? localStorage).setItem(codexDefaultMigrationKey, 'true'); } catch { /* The current session still keeps explicit choices. */ }
}
