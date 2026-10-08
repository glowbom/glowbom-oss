import { afterEach, expect, test } from 'bun:test';
import { renderToStaticMarkup } from 'react-dom/server';
import { useRefineRun, type RefineRun, type StartRefineInput } from '../src/hooks/useRefineRun';
import type { CompanionBuildJob, CompanionStatus } from '../src/lib/api';

const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });
const job: CompanionBuildJob = { id: 'job-one', projectId: 'project', projectPath: '/project', kind: 'build', status: 'running', output: ['Reviewing the app.'], startedAt: '2026-10-02T10:00:00Z', runId: 'history-one', agentDriver: 'opencode', sessionID: 'session-one' };
const snapshot = (status: string): CompanionStatus => ({ active: false, interfaces: [], jobs: [{ ...job, status }] });
const input: StartRefineInput = { projectPath: '/project', instructions: 'Build a clock', openaiAuthMode: 'opencode-config', providerKeys: { openaiKey: '', anthropicKey: '', geminiKey: '', fireworksKey: '', openrouterKey: '', opencodeZenKey: '', xaiKey: '', elevenLabsKey: '' }, imageProviderKeys: { openaiImageKey: '', geminiImageKey: '', xaiImageKey: '' } };
function runner(): RefineRun {
  let work!: RefineRun;
  function Fixture() { work = useRefineRun(); return null; }
  renderToStaticMarkup(<Fixture />);
  return work;
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(yes => { resolve = yes; });
  return { promise, resolve };
}
const tick = () => new Promise(resolve => setTimeout(resolve, 0));

test('Stop before response headers sends one scoped cancel and keeps progress attached until confirmed', async () => {
  const headers = deferred<Response>();
  const cancellation = deferred<Response>();
  const calls: string[] = [];
  let signal!: AbortSignal;
  globalThis.fetch = (async (url, init) => {
    calls.push(String(url));
    if (String(url) === '/api/companion/cancel') {
      expect(JSON.parse(String(init?.body))).toEqual({ jobId: 'job-one' });
      return cancellation.promise;
    }
    signal = init?.signal as AbortSignal;
    return headers.promise;
  }) as typeof fetch;
  const work = runner();
  const running = work.startRefine(input);
  work.stopRun();
  expect(calls).toEqual(['/api/opencode/refine']);
  expect(signal.aborted).toBe(false);
  const body = new ReadableStream({ start(controller) { signal.addEventListener('abort', () => controller.error(new DOMException('Disconnected', 'AbortError')), { once: true }); } });
  headers.resolve(new Response(body, { headers: { 'Content-Type': 'text/event-stream', 'X-Glowbom-Job-ID': 'job-one' } }));
  await tick();
  expect(calls).toEqual(['/api/opencode/refine', '/api/companion/cancel']);
  expect(signal.aborted).toBe(false);
  cancellation.resolve(Response.json(snapshot('canceled')));
  await running;
  expect(signal.aborted).toBe(true);
  expect(work.applyCompanionJob({ ...job, status: 'running' })).toBe(false);
});

test('an interrupted progress stream keeps its job available for recovery and explicit Stop', async () => {
  const calls: string[] = [];
  globalThis.fetch = (async (url, init) => {
    calls.push(String(url));
    if (String(url) === '/api/companion/cancel') return Response.json(snapshot('canceled'));
    return new Response('data: {"jobId":"job-one","output":"Reviewing the app."}\n\n', { headers: { 'Content-Type': 'text/event-stream', 'X-Glowbom-Job-ID': 'job-one' } });
  }) as typeof fetch;
  const work = runner();
  await work.startRefine(input);
  expect(calls).toEqual(['/api/opencode/refine']);
  expect(work.applyCompanionJob(job)).toBe(true);
  work.stopRun();
  await tick();
  expect(calls).toEqual(['/api/opencode/refine', '/api/companion/cancel']);
});

test('disconnectRun only closes observation, and restored sessions remain available to the next build', async () => {
  let signal!: AbortSignal;
  const calls: string[] = [];
  const payloads: Record<string, unknown>[] = [];
  globalThis.fetch = (async (url, init) => {
    calls.push(String(url)); payloads.push(JSON.parse(String(init?.body)));
    if (calls.length > 1) return Response.json({ done: true, success: true, jobId: 'job-two' });
    signal = init?.signal as AbortSignal;
    return new Response(new ReadableStream({ start(controller) { signal.addEventListener('abort', () => controller.error(new DOMException('Disconnected', 'AbortError')), { once: true }); } }), { headers: { 'Content-Type': 'text/event-stream', 'X-Glowbom-Job-ID': 'job-one' } });
  }) as typeof fetch;
  const work = runner();
  const running = work.startRefine(input);
  await tick();
  work.disconnectRun();
  await running;
  expect(signal.aborted).toBe(true);
  expect(calls).toEqual(['/api/opencode/refine']);
  expect(work.applyCompanionJob({ ...job, status: 'completed' })).toBe(true);
  await work.startRefine(input);
  expect(payloads[1]?.sessionID).toBe('session-one');
  expect(work.applyCompanionJob(job)).toBe(false);
});

test('cancel conflicts refresh the same job and preserve its completed result', async () => {
  const calls: string[] = [];
  globalThis.fetch = (async url => {
    calls.push(String(url));
    if (String(url) === '/api/companion/cancel') return new Response('Build already finished.', { status: 409 });
    if (String(url) === '/api/companion') return Response.json(snapshot('completed'));
    return new Response('', { headers: { 'Content-Type': 'text/event-stream', 'X-Glowbom-Job-ID': 'job-one' } });
  }) as typeof fetch;
  const work = runner();
  await work.startRefine(input);
  work.stopRun();
  await tick();
  expect(calls).toEqual(['/api/opencode/refine', '/api/companion/cancel', '/api/companion']);
  expect(work.applyCompanionJob({ ...job, status: 'running' })).toBe(false);
});

test('Codex continues its own recovered session without crossing into another driver', async () => {
  const payloads: Record<string, unknown>[] = [];
  globalThis.fetch = (async (_url, init) => {
    payloads.push(JSON.parse(String(init?.body)));
    return Response.json({ done: true, success: true, jobId: `next-${payloads.length}` });
  }) as typeof fetch;
  const codex = runner();
  expect(codex.applyCompanionJob({ ...job, agentDriver: 'codex', status: 'completed', sessionID: 'codex-thread' })).toBe(true);
  await codex.startRefine({ ...input, agentDriver: 'codex', model: 'model-one' });
  expect(payloads[0]?.agentDriver).toBe('codex');
  expect(payloads[0]?.sessionID).toBe('codex-thread');
  await codex.startRefine({ ...input, agentDriver: 'cursor', model: 'auto' });
  expect(payloads[1]?.sessionID).toBeUndefined();
});

test('Codex build and resumed turns forward the selected effort without leaking it to Cursor', async () => {
  const payloads: Record<string, unknown>[] = [];
  globalThis.fetch = (async (_url, init) => { payloads.push(JSON.parse(String(init?.body))); return Response.json({ done: true, success: true, jobId: `effort-${payloads.length}` }); }) as typeof fetch;
  const codex = runner();
  codex.applyCompanionJob({ ...job, agentDriver: 'codex', status: 'completed', sessionID: 'codex-thread' });
  await codex.startRefine({ ...input, agentDriver: 'codex', model: 'gpt-6.1-sol', reasoningEffort: 'ultra' });
  expect(payloads[0]?.reasoningEffort).toBe('ultra');
  expect(payloads[0]?.sessionID).toBe('codex-thread');
  await codex.startRefine({ ...input, agentDriver: 'codex', model: 'gpt-6.1-sol', reasoningEffort: 'high' });
  expect(payloads[1]?.reasoningEffort).toBe('high');
  await codex.startRefine({ ...input, agentDriver: 'cursor', model: 'auto', reasoningEffort: 'ultra' });
  expect(payloads[2]?.reasoningEffort).toBeUndefined();
});

test('Claude Code resumes only its own saved session and strips Codex effort', async () => {
  const payloads: Record<string, unknown>[] = [];
  globalThis.fetch = (async (_url, init) => {
    payloads.push(JSON.parse(String(init?.body)));
    return Response.json({ done: true, success: true, jobId: `claude-${payloads.length}` });
  }) as typeof fetch;
  const claude = runner();
  expect(claude.applyCompanionJob({ ...job, agentDriver: 'claude-code', status: 'completed', sessionID: 'claude-session' })).toBe(true);
  await claude.startRefine({ ...input, agentDriver: 'claude-code', model: 'sonnet', reasoningEffort: 'ultra' });
  expect(payloads[0]?.agentDriver).toBe('claude-code');
  expect(payloads[0]?.sessionID).toBe('claude-session');
  expect(payloads[0]?.model).toBe('sonnet');
  expect(payloads[0]?.useJev).toBe(false);
  expect(payloads[0]?.reasoningEffort).toBeUndefined();
  await claude.startRefine({ ...input, agentDriver: 'cursor', model: 'auto' });
  expect(payloads[1]?.sessionID).toBeUndefined();
});

test('four workers can stream together and stopping Claude cancels only its managed job', async () => {
  const drivers = ['opencode', 'cursor', 'claude-code', 'codex'] as const;
  const signals = new Map<string, AbortSignal>();
  const cancellations: string[] = [];
  globalThis.fetch = (async (url, init) => {
    const payload = JSON.parse(String(init?.body));
    if (String(url) === '/api/companion/cancel') {
      cancellations.push(payload.jobId);
      return Response.json({ active: false, interfaces: [], jobs: [{ ...job, id: payload.jobId, agentDriver: 'claude-code', status: 'canceled' }] });
    }
    const driver = payload.agentDriver;
    const signal = init?.signal as AbortSignal;
    signals.set(driver, signal);
    return new Response(new ReadableStream({ start(controller) {
      signal.addEventListener('abort', () => controller.error(new DOMException('Disconnected', 'AbortError')), { once: true });
    } }), { headers: { 'Content-Type': 'text/event-stream', 'X-Glowbom-Job-ID': `${driver}-job` } });
  }) as typeof fetch;
  const workers = drivers.map(() => runner());
  const running = workers.map((worker, index) => worker.startRefine({ ...input, agentDriver: drivers[index] }));
  await tick();
  expect(signals.size).toBe(4);
  workers[2]!.stopRun();
  await running[2];
  expect(cancellations).toEqual(['claude-code-job']);
  expect(signals.get('claude-code')?.aborted).toBe(true);
  for (const driver of ['opencode', 'cursor', 'codex']) expect(signals.get(driver)?.aborted).toBe(false);
  workers.forEach(worker => worker.disconnectRun());
  await Promise.all(running);
});

test('native session prefixes survive streamed output before the next build', async () => {
  for (const driver of ['claude-code', 'codex'] as const) {
    const payloads: Record<string, unknown>[] = [];
    globalThis.fetch = (async (_url, init) => {
      payloads.push(JSON.parse(String(init?.body)));
      return new Response(`data: ${JSON.stringify({ output: `Session created: ${driver}:1234-5678\n` })}\n\ndata: {"done":true,"success":true}\n\n`, { headers: { 'Content-Type': 'text/event-stream' } });
    }) as typeof fetch;
    const work = runner();
    await work.startRefine({ ...input, agentDriver: driver, model: 'default' });
    await work.startRefine({ ...input, agentDriver: driver, model: 'default' });
    expect(payloads[1]?.sessionID).toBe(`${driver}:1234-5678`);
  }
});

test('ACP resumes only the same saved connection and project and never forwards other agent settings', async () => {
  const payloads: Record<string, unknown>[] = [];
  globalThis.fetch = (async (_url, init) => {
    payloads.push(JSON.parse(String(init?.body)));
    return Response.json({ done: true, success: true, jobId: `acp-${payloads.length}` });
  }) as typeof fetch;
  const acp = runner();
  expect(acp.applyCompanionJob({ ...job, agentDriver: 'acp', model: 'acp/acp-1', status: 'completed', sessionID: 'acp:acp-1:abcdef:YWdlbnQ' })).toBe(true);
  await acp.startRefine({ ...input, agentDriver: 'acp', model: 'acp/acp-1', reasoningEffort: 'ultra' });
  expect(payloads[0]?.agentDriver).toBe('acp');
  expect(payloads[0]?.sessionID).toBe('acp:acp-1:abcdef:YWdlbnQ');
  expect(payloads[0]?.model).toBe('acp/acp-1');
  expect(payloads[0]?.useJev).toBe(false);
  expect(payloads[0]?.reasoningEffort).toBeUndefined();
  await acp.startRefine({ ...input, agentDriver: 'acp', model: 'acp/acp-2' });
  expect(payloads[1]?.sessionID).toBeUndefined();
  await acp.startRefine({ ...input, agentDriver: 'acp', model: 'acp/acp-1' });
  expect(payloads[2]?.sessionID).toBeUndefined();
  const restored = runner();
  restored.applyCompanionJob({ ...job, agentDriver: 'acp', model: 'acp/acp-1', status: 'completed', sessionID: 'acp:acp-1:abcdef:YWdlbnQ' });
  await restored.startRefine({ ...input, projectPath: '/other', agentDriver: 'acp', model: 'acp/acp-1' });
  expect(payloads[3]?.sessionID).toBeUndefined();
});

test('ACP namespaced session IDs survive progress streaming intact', async () => {
  const payloads: Record<string, unknown>[] = [];
  globalThis.fetch = (async (_url, init) => {
    payloads.push(JSON.parse(String(init?.body)));
    return new Response('data: {"output":"Session created: acp:acp-3:abcdef:YWdlbnQtc2Vzc2lvbg\\n"}\n\ndata: {"done":true,"success":true}\n\n', { headers: { 'Content-Type': 'text/event-stream' } });
  }) as typeof fetch;
  const acp = runner();
  await acp.startRefine({ ...input, agentDriver: 'acp', model: 'acp/acp-3' });
  await acp.startRefine({ ...input, agentDriver: 'acp', model: 'acp/acp-3' });
  expect(payloads[1]?.sessionID).toBe('acp:acp-3:abcdef:YWdlbnQtc2Vzc2lvbg');
});

test('ACP stream and recovery preserve the agent name from the build snapshot', async () => {
  const { hookRunner } = await import('./helpers/react-hook-runner');
  globalThis.fetch = (async () => new Response('data: {"agentName":"Original agent","sessionID":"acp:acp-1:abcdef:YWdlbnQ"}\n\ndata: {"done":true,"success":true}\n\n', { headers: { 'Content-Type': 'text/event-stream' } })) as typeof fetch;
  const fixture = hookRunner(useRefineRun);
  try {
    await fixture.render().startRefine({ ...input, agentDriver: 'acp', model: 'acp/acp-1', agentName: 'Old catalog name' });
    expect(fixture.render().agentName).toBe('Original agent');
    expect(fixture.render().hasSession).toBe(true);
  } finally { fixture.unmount(); }
  const restored = hookRunner(useRefineRun);
  try {
    restored.render().applyCompanionJob({ ...job, agentDriver: 'acp', model: 'acp/acp-1', agentName: 'Saved agent', status: 'completed' });
    expect(restored.render().agentName).toBe('Saved agent');
  } finally { restored.unmount(); }
});
