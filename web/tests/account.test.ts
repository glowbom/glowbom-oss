import { afterEach, expect, test } from 'bun:test';
import { accountRequest } from '../src/lib/account';

const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });
function respond(body: unknown, status = 200) {
  globalThis.fetch = (async () => new Response(JSON.stringify(body), { status })) as typeof fetch;
}

test('reads subscription status without deriving generation allowance', async () => {
  respond({ version: 1, status: 'signed_in', subscriptionStatus: 'premium' });
  const account = await accountRequest('status');
  expect(account).toEqual({ version: 1, status: 'signed_in', subscriptionStatus: 'premium' });
});

test('reports a helpful error for an unavailable proxy instead of raw JSON errors', async () => {
  globalThis.fetch = (async () => new Response('', { status: 502 })) as typeof fetch;
  await expect(accountRequest('status')).rejects.toThrow('Start it with glowbom start');
});

test('does not display arbitrary server error output', async () => {
  respond({ code: 'backend_auth_required', error: 'private diagnostic content' }, 401);
  await expect(accountRequest('login', 'POST')).rejects.toThrow('connect your account securely');
});

test('rejects incompatible account protocol versions', async () => {
  respond({ version: 2, status: 'signed_in' });
  await expect(accountRequest('status')).rejects.toThrow('Update Glowbom CLI');
});

test('preserves aborted requests so unmounted login screens ignore them', async () => {
  const controller = new AbortController();
  controller.abort();
  const aborted = new DOMException('Aborted', 'AbortError');
  globalThis.fetch = (async () => { throw aborted; }) as typeof fetch;
  await expect(accountRequest('login', 'GET', controller.signal)).rejects.toBe(aborted);
});
