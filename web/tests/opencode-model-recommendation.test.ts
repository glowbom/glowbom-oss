import { describe, expect, test } from 'bun:test';
import {
  OPENCODE_DEFAULT_MODEL_VALUE,
  OPENCODE_RECOMMENDED_MODEL_VALUE,
  findChatGPTRecommendedModel,
  resolveOpenCodeModelValue,
} from '../src/lib/opencode-model-recommendation';
import type { OpenCodeAvailableProvider } from '../src/types/opencode';

const providers: OpenCodeAvailableProvider[] = [
  {
    id: 'opencode',
    displayName: 'OpenCode Zen',
    models: [{ id: 'gpt-5.6-sol', displayName: 'GPT-5.6 Sol' }],
  },
  {
    id: 'openai',
    displayName: 'OpenAI',
    models: [
      { id: 'gpt-5.6-sol', displayName: 'GPT-5.6 Sol' },
      { id: 'gpt-5.6-terra', displayName: 'GPT-5.6 Terra' },
    ],
  },
];

describe('findChatGPTRecommendedModel', () => {
  test('recommends the OpenAI Sol route for ChatGPT OAuth', () => {
    expect(findChatGPTRecommendedModel('oauth', providers)).toEqual({
      value: 'openai/gpt-5.6-sol',
      modelLabel: 'GPT-5.6 Sol',
      fullLabel: 'GPT-5.6 Sol via ChatGPT',
    });
  });

  test('does not recommend Sol for an OpenAI API key', () => {
    expect(findChatGPTRecommendedModel('api', providers)).toBeNull();
  });

  test('does not recommend the OpenCode Zen Sol route', () => {
    expect(findChatGPTRecommendedModel('oauth', [providers[0]!])).toBeNull();
  });

  test('falls back when the OpenAI provider does not expose Sol', () => {
    expect(
      findChatGPTRecommendedModel('oauth', [
        {
          id: 'openai',
          displayName: 'OpenAI',
          models: [{ id: 'gpt-5.6-terra', displayName: 'GPT-5.6 Terra' }],
        },
      ]),
    ).toBeNull();
  });
});

describe('resolveOpenCodeModelValue', () => {
  const recommendation = findChatGPTRecommendedModel('oauth', providers);

  test('uses the recommendation for the untouched initial selection', () => {
    expect(resolveOpenCodeModelValue(OPENCODE_RECOMMENDED_MODEL_VALUE, recommendation)).toBe(
      'openai/gpt-5.6-sol',
    );
  });

  test('uses the OpenCode default while a recommendation is unavailable', () => {
    expect(resolveOpenCodeModelValue(OPENCODE_RECOMMENDED_MODEL_VALUE, null)).toBe('');
  });

  test('preserves an explicit choice to use the OpenCode default', () => {
    expect(resolveOpenCodeModelValue(OPENCODE_DEFAULT_MODEL_VALUE, recommendation)).toBe('');
  });

  test('preserves a manually selected model', () => {
    expect(resolveOpenCodeModelValue('anthropic/claude-sonnet', recommendation)).toBe(
      'anthropic/claude-sonnet',
    );
  });
});
