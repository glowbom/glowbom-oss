import { afterEach, expect, test } from 'bun:test';
import { acpArguments, fetchACPConnections, readACPConnections, saveACPConnections, testACPConnection, type ACPConnection } from '../src/lib/acp';

const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });
const connection: ACPConnection = { id: 'acp-2', name: 'My agent', command: '/path with spaces/agent', args: ['--acp', 'literal argument'] };

test('ACP arguments preserve spaces and shell syntax as literal argument values', () => {
  expect(acpArguments('--acp\r\n\nargument with spaces\n  preserve spaces  \n$(literal)\n"quotes remain"\n'))
    .toEqual(['--acp', 'argument with spaces', '  preserve spaces  ', '$(literal)', '"quotes remain"']);
  expect(acpArguments('')).toEqual([]);
});

test('saved ACP profiles require known unique connection slots', () => {
  expect(readACPConnections({ connections: [] })).toEqual([]);
  expect(readACPConnections({ connections: [connection, { ...connection, id: 'acp-1' }] }).map(item => item.id)).toEqual(['acp-1', 'acp-2']);
  for (const value of [null, {}, { connections: [connection, connection] }, { connections: [{ ...connection, id: 'acp-4' }] },
    { connections: [{ ...connection, args: 'acp' }] }, { connections: [{ ...connection, args: [null] }] }]) {
    expect(() => readACPConnections(value)).toThrow('Could not read');
  }
});

test('saved ACP profiles preserve an optional model override and reject malformed selections', () => {
  for (const value of [connection, { ...connection, model: '' }, { ...connection, model: 'provider/model-one' }]) {
    expect(readACPConnections({ connections: [value] })).toEqual([value]);
  }
  for (const model of [null, 10, {}, ['provider/model-one']]) {
    expect(() => readACPConnections({ connections: [{ ...connection, model }] })).toThrow('Could not read');
  }
});

test('loading saved connections only reads settings and never calls the test endpoint', async () => {
  const calls: Array<{ url: string; method?: string }> = [];
  globalThis.fetch = (async (url, options) => {
    calls.push({ url: String(url), method: options?.method });
    return Response.json({ connections: [connection] });
  }) as typeof fetch;
  expect(await fetchACPConnections()).toEqual([connection]);
  expect(calls).toEqual([{ url: '/api/settings/acp', method: 'GET' }]);
});

test('save and remove send the full configured list without changing argument boundaries', async () => {
  const bodies: unknown[] = [];
  globalThis.fetch = (async (url, options) => {
    expect(String(url)).toBe('/api/settings/acp');
    expect(options?.method).toBe('PUT');
    const body = JSON.parse(String(options?.body));
    bodies.push(body);
    return Response.json(body);
  }) as typeof fetch;
  expect(await saveACPConnections([connection])).toEqual([connection]);
  expect(await saveACPConnections([])).toEqual([]);
  expect(bodies).toEqual([{ connections: [connection] }, { connections: [] }]);
});

test('connection tests are explicit and preserve an authentication-required result', async () => {
  const result = { name: 'Agent', version: '1.0', images: true, loadSession: false, authRequired: true };
  globalThis.fetch = (async (url, options) => {
    expect(String(url)).toBe('/api/settings/acp/test');
    expect(options?.method).toBe('POST');
    expect(JSON.parse(String(options?.body))).toEqual({ connection });
    return Response.json(result);
  }) as typeof fetch;
  expect(await testACPConnection(connection)).toEqual(result);
});

test('connection tests accept optional model metadata and older agents without it', async () => {
  const base = { name: 'Agent', version: '1.0', images: false, loadSession: false, authRequired: false };
  for (const result of [base, { ...base, model: 'provider/free-model' }, { ...base, model: '' }]) {
    globalThis.fetch = (async () => Response.json(result)) as typeof fetch;
    expect(await testACPConnection(connection)).toEqual(result);
  }
});

test('connection tests reject malformed model metadata', async () => {
  const base = { name: 'Agent', version: '1.0', images: false, loadSession: false, authRequired: false };
  for (const model of [null, 1, true, {}, ['provider/free-model']]) {
    globalThis.fetch = (async () => Response.json({ ...base, model })) as typeof fetch;
    await expect(testACPConnection(connection)).rejects.toThrow('Could not read the ACP connection test.');
  }
});

test('connection tests discover selectable models without applying a saved override', async () => {
  const result = { name: 'Agent', version: '1.0', images: false, loadSession: false, authRequired: false, model: 'provider/default', models: [{ id: 'provider/default', name: 'Default' }, { id: 'provider/free', name: 'Free', description: 'A model reported by the agent.' }] };
  globalThis.fetch = (async (_url, options) => {
    expect(JSON.parse(String(options?.body))).toEqual({ connection });
    return Response.json(result);
  }) as typeof fetch;
  expect(await testACPConnection({ ...connection, model: 'provider/free' })).toEqual(result);
});

test('connection tests validate optional selectable models and accept agents without them', async () => {
  const base = { name: 'Agent', version: '1.0', images: false, loadSession: false, authRequired: false };
  globalThis.fetch = (async () => Response.json({ ...base, models: [] })) as typeof fetch;
  expect(await testACPConnection(connection)).toEqual({ ...base, models: [] });
  for (const models of [null, 'model', [null], [{ id: '', name: 'Empty' }], [{ id: 'model' }], [{ id: 'model', name: 'Model', description: 1 }], [{ id: 'model', name: 'One' }, { id: 'model', name: 'Two' }]]) {
    globalThis.fetch = (async () => Response.json({ ...base, models })) as typeof fetch;
    await expect(testACPConnection(connection)).rejects.toThrow('Could not read the ACP connection test.');
  }
});

test('failed saves surface the safe server error and never claim success', async () => {
  globalThis.fetch = (async () => Response.json({ error: 'Could not save the ACP connections.' }, { status: 500 })) as typeof fetch;
  await expect(saveACPConnections([connection])).rejects.toThrow('Could not save the ACP connections.');
});
