export type BuzzMember = { pubkey: string; role: string; displayName: string; pictureUrl?: string };
export type BuzzSession = {
  connected: boolean;
  connecting: boolean;
  relayUrl: string;
  channelId: string;
  members: BuzzMember[];
};

const messages: Record<string, string> = {
  local_auth_required: 'Start OSS through its launcher to enable authenticated local access.',
  cli_unavailable: 'Install Buzz or configure GLOWBOM_BUZZ_CLI, then restart the backend.',
  invalid_credentials: 'Check the HTTPS relay URL, channel UUID, key, and owner-auth tag.',
  lookup_failed: 'Buzz could not read this channel. Check credentials and channel access.',
  timeout: 'Buzz did not respond in time. Try again.',
  already_connected: 'A connection already exists. Its current status is shown below.',
  busy: 'A lookup is running. Wait or disconnect to cancel it.',
  disconnected: 'The connection has ended. Connect again to load members.',
};

export async function readBuzzSessionResponse(response: Response): Promise<BuzzSession> {
  // Older backends and authentication middleware can return plain text or HTML.
  // Never expose a raw response or JSON parser error in the credential form.
  if (response.status === 404 || response.status === 405) {
    throw new Error('This backend does not support Buzz connections yet. Stop and restart OSS with the updated code, then reload this page.');
  }
  if (response.status === 401 || response.status === 403) {
    throw new Error('Local backend access was denied. Restart OSS through its launcher and reload this page so the access tokens match.');
  }
  const data: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    const code = data && typeof data === 'object' && 'code' in data ? String(data.code) : '';
    throw new Error(messages[code] || 'The local backend could not complete the Buzz request. Check that OSS is running and try again.');
  }
  if (!isBuzzSession(data)) {
    throw new Error('The local backend returned an unexpected response. Restart OSS with the updated code, then reload this page.');
  }
  return data;
}

function isBuzzSession(data: unknown): data is BuzzSession {
  if (!data || typeof data !== 'object') return false;
  const session = data as Partial<BuzzSession>;
  return typeof session.connected === 'boolean' && typeof session.connecting === 'boolean'
    && typeof session.relayUrl === 'string' && typeof session.channelId === 'string'
    && Array.isArray(session.members) && session.members.every((member) => member
      && typeof member.pubkey === 'string' && typeof member.role === 'string'
      && typeof member.displayName === 'string'
      && (member.pictureUrl === undefined || typeof member.pictureUrl === 'string'));
}
