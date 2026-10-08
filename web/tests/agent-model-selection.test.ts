import { expect, test } from 'bun:test';
import { agentModels, agentRequestModel } from '../src/lib/agent-model-selection';
import type { ChatModel } from '../src/lib/chat';

const models: ChatModel[] = [
  { id: 'codex/build-model', name: 'Codex model', provider: 'Codex', images: true },
  { id: 'codex/chat-only', name: 'Chat only', provider: 'Codex', images: true, build: false },
  { id: 'acp/acp-2', name: 'Configured agent', provider: 'ACP', images: false, build: true },
  { id: 'acp/acp-4', name: 'Invalid connection', provider: 'ACP', images: false, build: true },
  { id: 'openai/build-model', name: 'Provider model', provider: 'OpenAI', images: true },
];

test('each agent picker contains only its build-capable connected models', () => {
  expect(agentModels(models, 'codex').map(model => model.id)).toEqual(['codex/build-model']);
  expect(agentModels(models, 'acp').map(model => model.id)).toEqual(['acp/acp-2']);
  expect(agentModels(models, 'opencode')).toEqual([]);
});

test('build requests strip the Codex namespace and retain the ACP connection namespace', () => {
  expect(agentRequestModel('codex', 'codex/build-model')).toBe('build-model');
  expect(agentRequestModel('acp', 'acp/acp-2')).toBe('acp/acp-2');
  expect(agentRequestModel('codex', 'openai/build-model')).toBe('');
  expect(agentRequestModel('acp', 'acp/acp-4')).toBe('');
  expect(agentRequestModel('acp', '')).toBe('');
});
