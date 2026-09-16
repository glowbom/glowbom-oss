const SESSION_STORAGE_KEY = 'glowbom.serverToken';
const LEGACY_SESSION_STORAGE_KEY = 'glowby.serverToken';

function browserTokenFromURL(): string {
  if (typeof window === 'undefined') {
    return '';
  }

  const url = new URL(window.location.href);
  const token = (url.searchParams.get('glowbom_token') || url.searchParams.get('glowby_token') || '').trim();
  if (!token) {
    return '';
  }

  try {
    window.sessionStorage.setItem(SESSION_STORAGE_KEY, token);
  } catch {
    // Ignore storage failures and keep using the token for this page load.
  }

  url.searchParams.delete('glowbom_token');
  url.searchParams.delete('glowby_token');
  window.history.replaceState({}, document.title, url.toString());
  return token;
}

function resolveServerToken(): string {
  const envToken = String(
    import.meta.env.VITE_GLOWBOM_SERVER_TOKEN || import.meta.env.VITE_GLOWBY_SERVER_TOKEN || '',
  ).trim();
  if (envToken) {
    return envToken;
  }

  const urlToken = browserTokenFromURL();
  if (urlToken) {
    return urlToken;
  }

  if (typeof window === 'undefined') {
    return '';
  }

  try {
    const currentToken = (window.sessionStorage.getItem(SESSION_STORAGE_KEY) || '').trim();
    if (currentToken) {
      return currentToken;
    }

    const legacyToken = (window.sessionStorage.getItem(LEGACY_SESSION_STORAGE_KEY) || '').trim();
    if (legacyToken) {
      window.sessionStorage.setItem(SESSION_STORAGE_KEY, legacyToken);
    }
    return legacyToken;
  } catch {
    return '';
  }
}

export function withServerAuthHeaders(headers?: HeadersInit): Headers {
  const resolved = new Headers(headers);
  const token = resolveServerToken();
  if (token && !resolved.has('Authorization')) {
    resolved.set('Authorization', `Bearer ${token}`);
  }
  return resolved;
}

// Copy the same local token used by API requests, only after a user click.
export async function copyLocalBackendAccessToken(): Promise<void> {
  const token = resolveServerToken();
  if (!token) {
    throw new Error('No local backend token is available. Restart OSS through its launcher and reload this page.');
  }
  if (!navigator.clipboard?.writeText) {
    throw new Error('Clipboard access is unavailable. Open OSS on localhost or 127.0.0.1 and try again.');
  }
  try {
    await navigator.clipboard.writeText(token);
  } catch {
    throw new Error('Could not copy the token. Allow clipboard access for this page and try again.');
  }
}
