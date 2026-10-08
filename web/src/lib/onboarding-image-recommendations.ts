import type { OnboardingPreference } from './onboarding';
import type { PrototypeImageSource } from './prototype-images';
import type { ProjectIconSource } from '../types/opencode';

export type OnboardingImageRuleId = 'free-placeholders' | 'local-placeholders' | 'no-image-connection'
  | 'existing-xai-subscription' | 'existing-openai-subscription' | 'existing-openai-api' | 'existing-xai-api' | 'existing-glowbom-account' | 'existing-gemini-api';

export interface OnboardingImageRecommendation {
  sourceId: PrototypeImageSource;
  model: string | null;
  reason: string;
  ruleId: OnboardingImageRuleId;
}

// This is a product preference order, not a model quality benchmark.
const connectedSourceRules: { sourceId: PrototypeImageSource; authType: ProjectIconSource['authType']; ruleId: OnboardingImageRuleId; reason: string }[] = [
  { sourceId: 'xai-subscription', authType: 'subscription', ruleId: 'existing-xai-subscription', reason: 'Use your connected xAI subscription for images. Its image limits apply.' },
  { sourceId: 'openai-subscription', authType: 'subscription', ruleId: 'existing-openai-subscription', reason: 'Use your connected ChatGPT subscription for images. ChatGPT limits apply.' },
  { sourceId: 'openai-api', authType: 'api-key', ruleId: 'existing-openai-api', reason: 'Use your configured OpenAI image connection. Provider charges apply.' },
  { sourceId: 'xai-api', authType: 'api-key', ruleId: 'existing-xai-api', reason: 'Use your configured xAI image connection. Provider charges apply.' },
  { sourceId: 'glowbom-api', authType: 'account', ruleId: 'existing-glowbom-account', reason: 'Use your connected Glowbom account for images. Your account allowance applies.' },
  { sourceId: 'gemini-api', authType: 'api-key', ruleId: 'existing-gemini-api', reason: 'Use your configured Google Gemini image connection. Provider charges apply.' },
];

export function recommendOnboardingImages(
  sources: readonly ProjectIconSource[],
  preference: OnboardingPreference,
  configuredApiKeys: Readonly<Record<string, boolean>> = {},
): OnboardingImageRecommendation {
  if (preference === 'free') return {
    sourceId: 'picsum', model: null, ruleId: 'free-placeholders',
    reason: 'Start with free placeholder photos. These are not AI-generated; downloading them uses the internet.',
  };
  if (preference === 'local') return {
    sourceId: 'picsum', model: null, ruleId: 'local-placeholders',
    reason: 'Use placeholder photos while keeping image generation off. These are not AI-generated; downloading them uses the internet.',
  };

  for (const rule of connectedSourceRules) {
    const source = sources.find(candidate => candidate.id === rule.sourceId && candidate.authType === rule.authType
      && candidate.model.trim() && (candidate.available || (rule.authType === 'api-key' && configuredApiKeys[rule.sourceId] === true)));
    if (source) return { sourceId: rule.sourceId, model: source.model, reason: rule.reason, ruleId: rule.ruleId };
  }
  return {
    sourceId: 'picsum', model: null, ruleId: 'no-image-connection',
    reason: 'No image generation connection is ready. Begin with placeholder photos that are not AI-generated, or connect an image provider.',
  };
}
