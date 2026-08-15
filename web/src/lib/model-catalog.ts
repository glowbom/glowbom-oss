import type {
  ModelCatalogEntry,
  ModelCatalogGroup,
  ModelCatalogProvider,
  OpenAIModelsResponseModel,
  ProviderID,
  ResolvedRefineModel,
} from '../types/opencode';

const OPTION_SEPARATOR = '::';

export const MODEL_PROVIDERS: ModelCatalogProvider[] = [
  {
    id: 'ollama',
    label: 'Ollama',
    requiresKey: false,
  },
  {
    id: 'xai',
    label: 'xAI',
    requiresKey: true,
    keyField: 'xaiKey',
    keyLabel: 'xAI API Key',
  },
  {
    id: 'fireworks',
    label: 'Fireworks',
    requiresKey: true,
    keyField: 'fireworksKey',
    keyLabel: 'Fireworks API Key',
  },
  {
    id: 'openrouter',
    label: 'OpenRouter',
    requiresKey: true,
    keyField: 'openrouterKey',
    keyLabel: 'OpenRouter API Key',
  },
  {
    id: 'opencode',
    label: 'OpenCode Zen',
    requiresKey: true,
    keyField: 'opencodeZenKey',
    keyLabel: 'OpenCode Zen API Key',
  },
  {
    id: 'openai',
    label: 'OpenAI',
    requiresKey: true,
    keyField: 'openaiKey',
    keyLabel: 'OpenAI / ChatGPT Token',
  },
  {
    id: 'anthropic',
    label: 'Anthropic',
    requiresKey: true,
    keyField: 'anthropicKey',
    keyLabel: 'Anthropic API Key',
  },
  {
    id: 'google',
    label: 'Google',
    requiresKey: true,
    keyField: 'geminiKey',
    keyLabel: 'Google Gemini API Key',
  },
];

const STATIC_MODEL_CATALOG: Record<ProviderID, ModelCatalogEntry[]> = {
  ollama: [
    {
      providerId: 'ollama',
      id: 'gpt-oss:20b',
      label: 'GPT-OSS',
      description: 'Local model via Ollama.',
    },
    {
      providerId: 'ollama',
      id: 'qwen3.5',
      label: 'Qwen 3.5',
      description: 'Local multimodal model via Ollama.',
    },
  ],
  xai: [
    {
      providerId: 'xai',
      id: 'grok-4.3',
      label: 'Grok 4.3',
      description: 'xAI flagship reasoning model for agentic tasks.',
    },
  ],
  fireworks: [
    {
      providerId: 'fireworks',
      id: 'glm-5',
      label: 'GLM-5',
      description: 'Fireworks GLM-5.',
    },
    {
      providerId: 'fireworks',
      id: 'kimi-k2p5',
      label: 'Kimi K2.5',
      description: 'Fireworks Kimi K2.5.',
    },
    {
      providerId: 'fireworks',
      id: 'minimax-m2p5',
      label: 'MiniMax M2.5',
      description: 'Fireworks MiniMax M2.5.',
    },
  ],
  openrouter: [
    {
      providerId: 'openrouter',
      id: 'z-ai/glm-5',
      label: 'GLM-5',
      description: 'OpenRouter GLM-5.',
    },
    {
      providerId: 'openrouter',
      id: 'moonshotai/kimi-k2.5',
      label: 'Kimi K2.5',
      description: 'OpenRouter Kimi K2.5.',
    },
    {
      providerId: 'openrouter',
      id: 'minimax/minimax-m2.5',
      label: 'MiniMax M2.5',
      description: 'OpenRouter MiniMax M2.5.',
    },
  ],
  opencode: [
    {
      providerId: 'opencode',
      id: 'kimi-k2.5-free',
      label: 'Kimi K2.5 FREE',
      description: 'OpenCode Zen Kimi K2.5 free tier.',
    },
    {
      providerId: 'opencode',
      id: 'glm-5-free',
      label: 'GLM 5 FREE',
      description: 'OpenCode Zen GLM-5 free tier.',
    },
    {
      providerId: 'opencode',
      id: 'minimax-m2.5-free',
      label: 'MiniMax M2.5 FREE',
      description: 'OpenCode Zen MiniMax free tier.',
    },
    {
      providerId: 'opencode',
      id: 'big-pickle',
      label: 'Big Pickle FREE',
      description: 'OpenCode Zen Big Pickle free tier.',
    },
  ],
  openai: [
    {
      providerId: 'openai',
      id: 'gpt-5.5',
      label: 'GPT-5.5',
      description: 'OpenAI GPT-5.5.',
    },
    {
      providerId: 'openai',
      id: 'gpt-5.3-codex',
      label: 'GPT-5.3 Codex',
      description: 'OpenAI Codex model.',
    },
    {
      providerId: 'openai',
      id: 'gpt-5.2',
      label: 'GPT-5.2',
      description: 'OpenAI GPT-5.2.',
    },
    {
      providerId: 'openai',
      id: 'gpt-5.2-codex',
      label: 'GPT-5.2 Codex',
      description: 'OpenAI Codex model.',
    },
    {
      providerId: 'openai',
      id: 'gpt-5.1-codex-mini',
      label: 'GPT-5.1 Codex mini',
      description: 'OpenAI Codex mini model.',
    },
    {
      providerId: 'openai',
      id: 'gpt-5.1-codex-max',
      label: 'GPT-5.1 Codex Max',
      description: 'OpenAI Codex max model.',
    },
  ],
  anthropic: [
    {
      providerId: 'anthropic',
      id: 'claude-sonnet-4-6',
      label: 'Claude Sonnet 4.6',
      description: 'Anthropic Sonnet 4.6.',
    },
    {
      providerId: 'anthropic',
      id: 'claude-opus-4-7',
      label: 'Claude Opus 4.7',
      description: 'Anthropic Opus 4.7.',
    },
  ],
  google: [
    {
      providerId: 'google',
      id: 'gemini-3.1-pro-preview',
      label: 'Gemini 3.1 Pro',
      description: 'Google Gemini 3.1 Pro.',
    },
    {
      providerId: 'google',
      id: 'gemini-3-flash-preview',
      label: 'Gemini 3 Flash',
      description: 'Google Gemini 3 Flash.',
    },
  ],
};

function dedupeModels(models: ModelCatalogEntry[]): ModelCatalogEntry[] {
  const seen = new Set<string>();
  const output: ModelCatalogEntry[] = [];

  for (const model of models) {
    const key = `${model.providerId}:${model.id}`;
    if (seen.has(key)) {
      continue;
    }
    seen.add(key);
    output.push(model);
  }

  return output;
}

export function staticModelCatalogFor(providerId: ProviderID): ModelCatalogEntry[] {
  return [...(STATIC_MODEL_CATALOG[providerId] || [])];
}

export function modelCatalogGroups(dynamicOpenAIModels: OpenAIModelsResponseModel[] = []): ModelCatalogGroup[] {
  const dynamicOpenAIEntries: ModelCatalogEntry[] = dynamicOpenAIModels.map((model) => ({
    providerId: 'openai',
    id: model.id,
    label: model.displayName || model.id,
    description: 'Loaded from ChatGPT/OpenAI model catalog.',
    isDynamic: true,
  }));

  return MODEL_PROVIDERS.map((provider): ModelCatalogGroup => {
    const baseModels = staticModelCatalogFor(provider.id);
    const mergedModels =
      provider.id === 'openai' && dynamicOpenAIEntries.length > 0
        ? dedupeModels([...dynamicOpenAIEntries, ...baseModels])
        : baseModels;

    return {
      provider,
      models: mergedModels,
    };
  }).filter((group) => group.models.length > 0);
}

export function providerDefinition(providerId: ProviderID): ModelCatalogProvider | undefined {
  return MODEL_PROVIDERS.find((provider) => provider.id === providerId);
}

export function encodeModelOptionValue(providerId: ProviderID, modelId: string): string {
  return `${providerId}${OPTION_SEPARATOR}${encodeURIComponent(modelId)}`;
}

function normalizeModelIdForProvider(providerId: ProviderID, modelId: string): string {
  if (providerId !== 'xai') {
    return modelId;
  }

  switch (modelId.trim().toLowerCase()) {
    case 'grok':
    case 'xai':
    case 'grok-4':
    case 'grok-4.1':
    case 'grok-4-1':
    case 'grok-4.1-fast':
    case 'grok-4-1-fast':
    case 'grok-4.1-fast-reasoning':
    case 'grok-4-1-fast-reasoning':
    case 'grok-4.1-fast-non-reasoning':
    case 'grok-4-1-fast-non-reasoning':
    case 'grok-4.3':
    case 'grok-4-3':
    case 'grok-4.3-latest':
    case 'grok-4-3-latest':
    case 'grok-latest':
      return 'grok-4.3';
    default:
      return modelId;
  }
}

export function decodeModelOptionValue(value: string): { providerId: ProviderID; modelId: string } | null {
  const separatorIndex = value.indexOf(OPTION_SEPARATOR);
  if (separatorIndex <= 0) {
    return null;
  }

  const providerId = value.slice(0, separatorIndex) as ProviderID;
  const encodedModel = value.slice(separatorIndex + OPTION_SEPARATOR.length);
  if (!providerDefinition(providerId)) {
    return null;
  }

  try {
    const modelId = decodeURIComponent(encodedModel);
    return {
      providerId,
      modelId: normalizeModelIdForProvider(providerId, modelId),
    };
  } catch {
    return {
      providerId,
      modelId: normalizeModelIdForProvider(providerId, encodedModel),
    };
  }
}

export function resolveRefineModel(providerId: ProviderID, modelId: string): ResolvedRefineModel {
  const normalizedModelId = normalizeModelIdForProvider(providerId, modelId);
  const group = modelCatalogGroups().find((item) => item.provider.id === providerId);
  const entry = group?.models.find((model) => model.id === normalizedModelId);
  const label = entry ? `${group?.provider.label}: ${entry.label}` : `${providerId}/${normalizedModelId}`;

  return {
    providerId,
    modelId: normalizedModelId,
    value: `${providerId}/${normalizedModelId}`,
    label,
  };
}
