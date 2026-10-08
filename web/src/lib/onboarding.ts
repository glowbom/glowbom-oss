import type { ChatModel } from './chat';
import { prototypeImageSources, type PrototypeImageSource } from './prototype-images';
import { candidateModels } from './onboarding-candidates';
import { recommendOnboardingModels, type OnboardingBuildPriority } from './onboarding-recommendations';
export { candidateModels } from './onboarding-candidates';

export type OnboardingIntent = 'sketch' | 'chat' | 'project';
export type OnboardingPreference = 'existing' | 'free' | 'local';
export type OnboardingModelKey = 'chatModel' | 'prototypeModel' | 'buildModel';
export interface OnboardingChoices {
  intent: OnboardingIntent;
  preference: OnboardingPreference;
  chatModel: string;
  prototypeModel: string;
  buildModel: string;
  imageSource?: PrototypeImageSource;
  buildPriority?: 'balanced' | 'capability';
}
export interface OnboardingProgress {
  version: 1;
  step: 'intent' | 'preference' | 'setup' | 'ready';
  choices: OnboardingChoices;
  completed: boolean;
  deferredModels?: OnboardingModelKey[];
  automaticModels?: OnboardingModelKey[];
  automaticImages?: boolean;
}
export interface OnboardingCandidates {
  chat: ChatModel[];
  prototype: ChatModel[];
  build: ChatModel[];
}

export const onboardingStorageKey = 'glowbom_onboarding_v1';

function initialProgress(): OnboardingProgress {
  return {
    version: 1,
    step: 'intent',
    choices: { intent: 'sketch', preference: 'existing', chatModel: '', prototypeModel: '', buildModel: '' },
    completed: false,
  };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function modelChoice(value: unknown): value is string {
  return typeof value === 'string' && value.length <= 512 && !/[\r\n\0]/.test(value);
}

export function readOnboardingProgress(storage?: Pick<Storage, 'getItem'>): OnboardingProgress {
  try {
    const raw = (storage ?? localStorage).getItem(onboardingStorageKey);
    const value: unknown = raw ? JSON.parse(raw) : null;
    if (!isRecord(value) || value.version !== 1 || typeof value.completed !== 'boolean'
      || typeof value.step !== 'string' || !['intent', 'preference', 'setup', 'ready'].includes(value.step) || !isRecord(value.choices)) return initialProgress();
    const choices = value.choices;
    if (typeof choices.intent !== 'string' || !['sketch', 'chat', 'project'].includes(choices.intent)
      || typeof choices.preference !== 'string' || !['existing', 'free', 'local'].includes(choices.preference)
      || !modelChoice(choices.chatModel) || !modelChoice(choices.prototypeModel) || !modelChoice(choices.buildModel)) return initialProgress();
    return {
      version: 1,
      step: value.step as OnboardingProgress['step'],
      completed: value.completed,
      ...(Array.isArray(value.deferredModels) ? { deferredModels: [...new Set(value.deferredModels.filter((key): key is OnboardingModelKey => key === 'chatModel' || key === 'prototypeModel' || key === 'buildModel'))] } : {}),
      ...(Array.isArray(value.automaticModels) ? { automaticModels: [...new Set(value.automaticModels.filter((key): key is OnboardingModelKey => key === 'chatModel' || key === 'prototypeModel' || key === 'buildModel'))] } : {}),
      ...(typeof value.automaticImages === 'boolean' ? { automaticImages: value.automaticImages } : {}),
      choices: {
        intent: choices.intent as OnboardingIntent,
        preference: choices.preference as OnboardingPreference,
        chatModel: choices.chatModel.trim(),
        prototypeModel: choices.prototypeModel.trim(),
        buildModel: choices.buildModel.trim(),
        ...(prototypeImageSources.includes(choices.imageSource as PrototypeImageSource) ? { imageSource: choices.imageSource as PrototypeImageSource } : {}),
        ...(['balanced', 'capability'].includes(choices.buildPriority as string) ? { buildPriority: choices.buildPriority as 'balanced' | 'capability' } : {}),
      },
    };
  } catch {
    return initialProgress();
  }
}

export function writeOnboardingProgress(progress: OnboardingProgress, storage?: Pick<Storage, 'setItem'>): void {
  try {
    const { intent, preference, chatModel, prototypeModel, buildModel, imageSource, buildPriority } = progress.choices;
    // Persist only safe choices. Connection credentials stay in the current session.
    (storage ?? localStorage).setItem(onboardingStorageKey, JSON.stringify({
      version: progress.version, step: progress.step, completed: progress.completed,
      ...(progress.deferredModels ? { deferredModels: progress.deferredModels } : {}),
      ...(progress.automaticModels ? { automaticModels: progress.automaticModels } : {}),
      ...(typeof progress.automaticImages === 'boolean' ? { automaticImages: progress.automaticImages } : {}),
      choices: { intent, preference, chatModel, prototypeModel, buildModel, ...(imageSource ? { imageSource } : {}), ...(buildPriority ? { buildPriority } : {}) },
    }));
  } catch { /* Setup can continue when browser storage is unavailable. */ }
}

export function reopenOnboardingProgress(progress: OnboardingProgress, models: Pick<OnboardingChoices, OnboardingModelKey>): OnboardingProgress {
  const keys: OnboardingModelKey[] = ['chatModel', 'prototypeModel', 'buildModel'];
  return {
    ...progress,
    step: 'setup',
    choices: { ...progress.choices, ...models },
    deferredModels: keys.filter(key => !models[key]),
    automaticModels: [],
    automaticImages: false,
  };
}

export function recommendedModels(
  models: ChatModel[],
  preference: OnboardingPreference,
  existing: Partial<OnboardingChoices> = {},
  verifiedLocalModelIds: readonly string[] = [],
  deferredModels: readonly OnboardingModelKey[] = [],
  buildPriority: OnboardingBuildPriority = 'balanced',
): Pick<OnboardingChoices, 'chatModel' | 'prototypeModel' | 'buildModel'> {
  const candidates = candidateModels(models, preference, verifiedLocalModelIds);
  const recommendations = recommendOnboardingModels(models, preference, verifiedLocalModelIds, buildPriority);
  const choose = (key: OnboardingModelKey, options: ChatModel[], selected: string | undefined): string => {
    if (deferredModels.includes(key)) return '';
    if (selected && options.some((model) => model.id === selected)) return selected;
    return recommendations[key].modelId;
  };
  return {
    chatModel: choose('chatModel', candidates.chat, existing.chatModel),
    prototypeModel: choose('prototypeModel', candidates.prototype, existing.prototypeModel),
    buildModel: choose('buildModel', candidates.build, existing.buildModel),
  };
}
