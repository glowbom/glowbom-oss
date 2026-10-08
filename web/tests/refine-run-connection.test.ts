import { expect, test } from 'bun:test';
import { RefineRunConnection } from '../src/lib/refine-run-connection';
import type { CompanionBuildJob, CompanionStatus } from '../src/lib/api';

const job: CompanionBuildJob = { id: 'job-one', projectId: 'project', projectPath: '/project', status: 'running', output: [], startedAt: '2026-10-02T10:00:00Z' };
const snapshot = (status: string): CompanionStatus => ({ active: false, interfaces: [], jobs: [{ ...job, status }] });
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

test('early Stop waits for the server identity and confirms cancellation before detaching', async () => {
  const response = deferred<CompanionStatus>();
  const calls: string[] = [];
  const completed: CompanionBuildJob[] = [];
  const errors: unknown[] = [];
  const connection = new RefineRunConnection({ cancel: id => { calls.push(id); return response.promise; }, onStopping: () => {}, onCancelled: item => completed.push(item), onCancelError: cause => errors.push(cause) });
  connection.requestStop();
  expect(calls).toEqual([]);
  expect(connection.controller.signal.aborted).toBe(false);
  connection.identify('job-one');
  connection.identify('job-one');
  expect(connection.identify('another-job')).toBe(false);
  expect(calls).toEqual(['job-one']);
  expect(connection.terminal).toBe(false);
  expect(connection.controller.signal.aborted).toBe(false);
  response.resolve(snapshot('canceled'));
  await response.promise;
  expect(connection.terminal).toBe(true);
  expect(connection.controller.signal.aborted).toBe(true);
  expect(completed.map(item => item.status)).toEqual(['canceled']);
  expect(errors).toEqual([]);
});

test('disconnecting the observer leaves its worker active and allows a later explicit Stop', async () => {
  const completed: CompanionBuildJob[] = [];
  const calls: string[] = [];
  const connection = new RefineRunConnection({ cancel: async id => { calls.push(id); return snapshot('completed'); }, onStopping: () => {}, onCancelled: item => completed.push(item), onCancelError: () => {} });
  connection.identify('job-one');
  connection.connected = true;
  connection.disconnect();
  expect(calls).toEqual([]);
  expect(connection.terminal).toBe(false);
  expect(connection.connected).toBe(false);
  connection.requestStop();
  await Promise.resolve();
  expect(completed.map(item => item.status)).toEqual(['completed']);
  expect(connection.terminal).toBe(true);
});

test('unconfirmed or failed cancellation keeps the worker active and can be retried', async () => {
  const errors: unknown[] = [];
  const calls: string[] = [];
  const results = [snapshot('running'), snapshot('canceled')];
  const connection = new RefineRunConnection({ cancel: async id => { calls.push(id); return results.shift()!; }, onStopping: () => {}, onCancelled: () => {}, onCancelError: cause => errors.push(cause) });
  connection.identify('job-one');
  connection.requestStop();
  await Promise.resolve();
  expect(errors).toHaveLength(1);
  expect(connection.terminal).toBe(false);
  expect(connection.controller.signal.aborted).toBe(false);
  connection.requestStop();
  await Promise.resolve();
  expect(calls).toEqual(['job-one', 'job-one']);
  expect(connection.terminal).toBe(true);
});

test('a disposed observer ignores late cancellation results and never cancels a replacement', async () => {
  const response = deferred<CompanionStatus>();
  const writes: string[] = [];
  const connection = new RefineRunConnection({ cancel: () => response.promise, onStopping: () => {}, onCancelled: () => writes.push('cancelled'), onCancelError: () => writes.push('error') });
  connection.identify('job-one');
  connection.requestStop();
  connection.dispose();
  response.resolve(snapshot('canceled'));
  await response.promise;
  expect(writes).toEqual([]);
  expect(connection.identify('replacement')).toBe(false);
});
