import { describe, expect, test } from 'bun:test';
import { readBuzzSessionResponse } from '../src/lib/buzz-session';

const snapshot = {
  connected: true, connecting: false, relayUrl: 'https://community.example', channelId: 'test-channel',
  members: [{ pubkey: 'a'.repeat(64), role: 'member', displayName: 'Pulse (fixture)' }],
};

describe('Buzz session responses', () => {
  test('reads a confirmed connection and its members', async () => {
    expect(await readBuzzSessionResponse(Response.json(snapshot))).toEqual(snapshot);
  });

  test('distinguishes disconnected and pending snapshots', async () => {
    for (const connecting of [false, true]) {
      const data = { ...snapshot, connected: false, connecting, members: [] };
      expect(await readBuzzSessionResponse(Response.json(data))).toEqual(data);
    }
  });

  test('accepts optional picture metadata without requiring it from older backends', async () => {
    const data = { ...snapshot, members: [{ ...snapshot.members[0], pictureUrl: 'https://community.example/media/avatar.png' }] };
    expect(await readBuzzSessionResponse(Response.json(data))).toEqual(data);
    await expect(readBuzzSessionResponse(Response.json({ ...snapshot, members: [{ ...snapshot.members[0], pictureUrl: 123 }] })))
      .rejects.toThrow('unexpected response');
  });

  test('explains a plain-text 404 from an older backend', async () => {
    await expect(readBuzzSessionResponse(new Response('404 page not found\n', { status: 404 })))
      .rejects.toThrow('Stop and restart OSS with the updated code');
  });

  test('explains plain-text authentication failures', async () => {
    for (const status of [401, 403]) {
      await expect(readBuzzSessionResponse(new Response('Unauthorized', { status })))
        .rejects.toThrow('access tokens match');
    }
  });

  test('uses safe messages for structured errors', async () => {
    await expect(readBuzzSessionResponse(Response.json({ code: 'lookup_failed' }, { status: 502 })))
      .rejects.toThrow('Check credentials and channel access');
  });

  test('never presents raw proxy errors or malformed JSON as connection state', async () => {
    for (const body of ['<html>private proxy diagnostic</html>', '404 page not found', '{invalid']) {
      await expect(readBuzzSessionResponse(new Response(body)))
        .rejects.toThrow('unexpected response');
      await expect(readBuzzSessionResponse(new Response(body, { status: 502 })))
        .rejects.toThrow('Check that OSS is running');
    }
  });

  test('rejects unrelated JSON and incomplete or malformed rosters', async () => {
    for (const body of [null, {}, { connected: 'true' }, { ...snapshot, members: [null] }, { ...snapshot, members: [{}] }]) {
      await expect(readBuzzSessionResponse(Response.json(body))).rejects.toThrow('unexpected response');
    }
  });
});
