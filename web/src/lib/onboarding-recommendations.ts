import { isOpenCodeFreeModel, type ChatModel } from './chat';
import { candidateModels } from './onboarding-candidates';
import type { OnboardingModelKey, OnboardingPreference } from './onboarding';

export type OnboardingBuildPriority = 'balanced' | 'capability';
export interface OnboardingModelAlternative {
  modelId: string;
  reason: string;
  ruleId: string;
}
export interface OnboardingModelRecommendation extends OnboardingModelAlternative {
  alternatives?: OnboardingModelAlternative[];
}
export type OnboardingRecommendations = Record<OnboardingModelKey, OnboardingModelRecommendation>;

type Family = 'gpt6-luna' | 'gpt6-sol' | 'gpt6-astra' | 'grok4.7' | 'grok4.3';
type KnownModel = { family: Family; route: number; variant: number };
type RankedModel = { model: ChatModel; family?: Family; rank: number[]; reason: string; ruleId: string };

// Glowbom product preferences; no benchmark ranking is implied.
// Model IDs and alias conventions checked on 2026-09-27:
// https://developers.openai.com/api/docs/models
// https://docs.x.ai/developers/models
// Recommendations only use IDs advertised by the connected catalog.
const roleLabel: Record<OnboardingModelKey, string> = { chatModel: 'conversation', prototypeModel: 'sketch prototypes', buildModel: 'building' };
const roleId: Record<OnboardingModelKey, string> = { chatModel: 'chat', prototypeModel: 'sketch', buildModel: 'build' };

function familyForModel(id: string): { family: Family; variant: number } | undefined {
  const gpt = /^gpt-6-(luna|sol|astra)(?:-(?:none|minimal|low|medium|high|xhigh|max|ultra)(?:-fast)?|-\d{4}-\d{2}-\d{2})?$/.exec(id);
  if (gpt) return { family: `gpt6-${gpt[1]}` as Family, variant: id === `gpt-6-${gpt[1]}` ? 0 : 1 };
  const grok = /^grok-4\.(7|3)(?:-(latest|\d{4}-\d{2}-\d{2}|(?:low|medium|high|xhigh|fast)(?:-fast)?))?$/.exec(id);
  if (grok) return { family: `grok4.${grok[1]}` as Family, variant: !grok[2] ? 0 : grok[2] === 'latest' ? 1 : /^\d{4}-/.test(grok[2]) ? 2 : 3 };
  return undefined;
}

function knownModel(id: string): KnownModel | undefined {
  const parts = id.toLowerCase().split('/');
  if (parts.length === 2) {
    const family = familyForModel(parts[1] || '');
    if (!family) return undefined;
    if ((parts[0] === 'openai' && family.family.startsWith('gpt6-')) || (parts[0] === 'xai' && family.family.startsWith('grok'))) return { ...family, route: 0 };
    if (parts[0] === 'opencode') return { ...family, route: 1 };
    if (parts[0] === 'cursor') return { ...family, route: 2 };
  }
  if (parts.length === 3 && parts[0] === 'openrouter') {
    const family = familyForModel(parts[2] || '');
    if (family && ((parts[1] === 'openai' && family.family.startsWith('gpt6-'))
      || (['x-ai', 'xai'].includes(parts[1] || '') && family.family.startsWith('grok')))) return { ...family, route: 1 };
  }
  return undefined;
}

function familyOrder(role: OnboardingModelKey, buildPriority: OnboardingBuildPriority): Family[] {
  const gpt: Family[] = role === 'chatModel' ? ['gpt6-luna', 'gpt6-sol', 'gpt6-astra']
    : role === 'buildModel' && buildPriority === 'capability' ? ['gpt6-astra', 'gpt6-sol', 'gpt6-luna']
      : ['gpt6-sol', 'gpt6-astra', 'gpt6-luna'];
  return [...gpt, 'grok4.7', 'grok4.3'];
}

function knownReason(family: Family, role: OnboardingModelKey, priority: OnboardingBuildPriority): string {
  if (family === 'gpt6-luna' && role === 'chatModel') return 'GPT-6 Luna favors efficiency for everyday conversation.';
  if (family === 'gpt6-sol' && role === 'prototypeModel') return 'GPT-6 Sol supports image input and is our balanced choice for turning sketches into code.';
  if (family === 'gpt6-sol' && role === 'buildModel') return priority === 'balanced'
    ? 'GPT-6 Sol is our balanced default for building projects.' : 'GPT-6 Sol is an available alternative for building projects.';
  if (family === 'gpt6-astra' && role === 'buildModel') return priority === 'capability'
    ? 'GPT-6 Astra matches your preference for more capability when building.' : 'GPT-6 Astra is an alternative when you want more capability for building.';
  if (family === 'grok4.7') return `Grok 4.7 is our preferred available Grok model for ${roleLabel[role]}.`;
  if (family === 'grok4.3') return `Grok 4.3 is an available Grok option for ${roleLabel[role]}.`;
  const name = family === 'gpt6-luna' ? 'GPT-6 Luna' : family === 'gpt6-sol' ? 'GPT-6 Sol' : 'GPT-6 Astra';
  return `${name} is an available GPT-6 option for ${roleLabel[role]}.`;
}

function rankModel(model: ChatModel, role: OnboardingModelKey, preference: OnboardingPreference, priority: OnboardingBuildPriority): RankedModel {
  const id = roleId[role];
  if (preference === 'free' && role === 'buildModel' && isOpenCodeFreeModel(model.id)) {
    return { model, rank: [0], ruleId: `${id}.opencode-free`, reason: 'An available free OpenCode model for building. Free usage limits can change.' };
  }
  if (preference !== 'existing') {
    if (model.id.startsWith('apple-intelligence/')) return { model, rank: [1], ruleId: `${id}.apple-local`, reason: 'Apple Intelligence is available for conversation on this computer.' };
    return { model, rank: [2], ruleId: `${id}.local-compatible`, reason: `A verified local model compatible with ${roleLabel[role]}. This fallback is based on compatibility.` };
  }
  const known = knownModel(model.id);
  if (known) return {
    model, family: known.family, rank: [0, known.route, familyOrder(role, priority).indexOf(known.family), known.variant],
    ruleId: `${id}.${known.family}`, reason: knownReason(known.family, role, priority),
  };
  const provider = model.id.split('/')[0] || '';
  const providers = ['openai', 'xai', 'openrouter', 'opencode', 'cursor', 'apple-intelligence', 'glowbom-ollama'];
  const providerOrder = providers.indexOf(provider);
  return {
    model, rank: [1, providerOrder < 0 ? providers.length : providerOrder, model.id === 'cursor/auto' ? 0 : 1],
    ruleId: `${id}.compatibility-fallback`,
    reason: model.id === 'cursor/auto' ? 'Cursor Auto can choose a model for building. This fallback is based on compatibility.'
      : `A connected model compatible with ${roleLabel[role]}. This fallback is based on compatibility.`,
  };
}

function compareRank(a: RankedModel, b: RankedModel): number {
  for (let i = 0; i < Math.max(a.rank.length, b.rank.length); i += 1) {
    const difference = (a.rank[i] || 0) - (b.rank[i] || 0);
    if (difference) return difference;
  }
  return a.model.id < b.model.id ? -1 : a.model.id > b.model.id ? 1 : 0;
}

function choice(value: RankedModel): OnboardingModelAlternative {
  return { modelId: value.model.id, reason: value.reason, ruleId: value.ruleId };
}

function recommendRole(models: ChatModel[], role: OnboardingModelKey, preference: OnboardingPreference, priority: OnboardingBuildPriority): OnboardingModelRecommendation {
  const ranked = models.map(model => rankModel(model, role, preference, priority)).sort(compareRank);
  const selected = ranked[0];
  if (!selected) {
    const next = preference === 'local' || (preference === 'free' && role !== 'buildModel')
      ? role === 'chatModel' ? 'Set up Apple Intelligence or a local Ollama model below.' : 'Choose a local model with image input for sketches or coding support for building, or add this later.'
      : role === 'prototypeModel' ? 'Connect a model that accepts images, or add sketch support later.' : 'Connect a model below, or add this activity later.';
    return { modelId: '', ruleId: `${roleId[role]}.unavailable`, reason: `No connected model fits this activity and your current preference. ${next}` };
  }
  const alternatives: OnboardingModelAlternative[] = [];
  const seen = new Set<string>([selected.family || selected.model.id]);
  const counterpartFamily = selected.family === 'gpt6-sol' ? 'gpt6-astra' : selected.family === 'gpt6-astra' ? 'gpt6-sol' : undefined;
  const counterpart = role === 'buildModel' && counterpartFamily ? ranked.find(option => option.family === counterpartFamily) : undefined;
  const remaining = counterpart ? [counterpart, ...ranked.slice(1)] : ranked.slice(1);
  for (const option of remaining) {
    const identity = option.family || option.model.id;
    if (seen.has(identity)) continue;
    seen.add(identity);
    alternatives.push(choice(option));
    if (alternatives.length === 3) break;
  }
  return { ...choice(selected), ...(alternatives.length ? { alternatives } : {}) };
}

export function recommendOnboardingModels(
  models: ChatModel[],
  preference: OnboardingPreference,
  verifiedLocalModelIds: readonly string[] = [],
  buildPriority: OnboardingBuildPriority = 'balanced',
): OnboardingRecommendations {
  const candidates = candidateModels(models, preference, verifiedLocalModelIds);
  return {
    chatModel: recommendRole(candidates.chat, 'chatModel', preference, buildPriority),
    prototypeModel: recommendRole(candidates.prototype, 'prototypeModel', preference, buildPriority),
    buildModel: recommendRole(candidates.build, 'buildModel', preference, buildPriority),
  };
}
