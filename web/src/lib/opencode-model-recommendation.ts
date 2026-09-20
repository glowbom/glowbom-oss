import type { OpenCodeAuthStatus, OpenCodeAvailableProvider } from '../types/opencode';

export const OPENCODE_RECOMMENDED_MODEL_VALUE = '__opencode_recommended__';
export const OPENCODE_DEFAULT_MODEL_VALUE = '__opencode_default__';

const CHATGPT_PROVIDER_ID = 'openai';
const CHATGPT_RECOMMENDED_MODEL_ID = 'gpt-6-astra';

export interface RecommendedOpenCodeModel {
  value: string;
  modelLabel: string;
  fullLabel: string;
}

function normalizeLookupValue(value: string): string {
  return value.trim().toLowerCase();
}

export function findChatGPTRecommendedModel(
  credentialType: OpenCodeAuthStatus['openaiCredentialType'] | undefined,
  providers: OpenCodeAvailableProvider[],
): RecommendedOpenCodeModel | null {
  if (credentialType !== 'oauth') {
    return null;
  }

  const openAIProvider = providers.find(
    (provider) => normalizeLookupValue(provider.id) === CHATGPT_PROVIDER_ID,
  );
  if (!openAIProvider) {
    return null;
  }

  const recommendedModel = openAIProvider.models.find(
    (model) => normalizeLookupValue(model.id) === CHATGPT_RECOMMENDED_MODEL_ID,
  );
  if (!recommendedModel) {
    return null;
  }

  const modelLabel = recommendedModel.displayName || recommendedModel.id;
  return {
    value: `${openAIProvider.id}/${recommendedModel.id}`,
    modelLabel,
    fullLabel: `${modelLabel} via ChatGPT`,
  };
}

export function resolveOpenCodeModelValue(
  selectedModel: string,
  recommendedModel: RecommendedOpenCodeModel | null,
): string {
  if (selectedModel === OPENCODE_RECOMMENDED_MODEL_VALUE) {
    return recommendedModel?.value || '';
  }
  if (selectedModel === OPENCODE_DEFAULT_MODEL_VALUE) {
    return '';
  }
  return selectedModel;
}
