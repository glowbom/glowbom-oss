import { withServerAuthHeaders } from './server-auth';

interface StreamJsonSseOptions<TRequest, TEvent extends Record<string, unknown>> {
  url: string;
  body: TRequest;
  signal: AbortSignal;
  onEvent: (event: TEvent) => void;
  onResponse?: (response: Response) => void;
}

function parseErrorMessage(response: Response, bodyText: string): string {
  if (!bodyText.trim()) {
    return `Request failed with HTTP ${response.status}`;
  }

  try {
    const parsed = JSON.parse(bodyText) as { error?: string; message?: string };
    return parsed.error || parsed.message || bodyText;
  } catch {
    return bodyText;
  }
}

function parseDataPayload(frame: string): string {
  const lines = frame.split(/\r?\n/);
  const dataLines: string[] = [];

  for (const line of lines) {
    if (line.startsWith('data:')) {
      dataLines.push(line.slice(5).trimStart());
    }
  }

  return dataLines.join('\n').trim();
}

export async function streamJsonSse<TRequest extends object, TEvent extends Record<string, unknown>>(
  options: StreamJsonSseOptions<TRequest, TEvent>,
): Promise<void> {
  const response = await fetch(options.url, {
    method: 'POST',
    headers: withServerAuthHeaders({
      'Content-Type': 'application/json',
      Accept: 'text/event-stream',
    }),
    body: JSON.stringify(options.body),
    signal: options.signal,
  });

  if (!response.ok) {
    throw new Error(parseErrorMessage(response, await response.text()));
  }
  options.onResponse?.(response);

  const contentType = response.headers.get('content-type') || '';
  if (!contentType.includes('text/event-stream')) {
    const textBody = await response.text();
    if (!textBody.trim()) {
      return;
    }
    try {
      options.onEvent(JSON.parse(textBody) as TEvent);
      return;
    } catch {
      throw new Error(textBody);
    }
  }

  const stream = response.body;
  if (!stream) {
    throw new Error('No response stream was returned by the server.');
  }

  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let buffer = '';

  while (true) {
    const { done, value } = await reader.read();
    if (done) {
      break;
    }

    buffer += decoder.decode(value, { stream: true });

    let separatorIndex = buffer.indexOf('\n\n');
    while (separatorIndex >= 0) {
      const frame = buffer.slice(0, separatorIndex);
      buffer = buffer.slice(separatorIndex + 2);

      const payload = parseDataPayload(frame);
      if (payload) {
        try {
          options.onEvent(JSON.parse(payload) as TEvent);
        } catch {
          // Ignore malformed event payloads and continue the stream.
        }
      }

      separatorIndex = buffer.indexOf('\n\n');
    }
  }

  const finalPayload = parseDataPayload(buffer.trim());
  if (finalPayload) {
    try {
      options.onEvent(JSON.parse(finalPayload) as TEvent);
    } catch {
      // Ignore malformed terminal payload.
    }
  }
}

export async function streamConfirmedSse<TRequest extends object, TEvent extends Record<string, unknown>>(
  options: StreamJsonSseOptions<TRequest, TEvent>,
): Promise<TEvent> {
  let terminal: TEvent | null = null;
  await streamJsonSse<TRequest, TEvent>({ ...options, onEvent: event => {
    if (event.done === true) terminal = event;
    options.onEvent(event);
  } });
  if (!terminal) throw new Error('The response stopped before completion. Your request is still available to retry.');
  return terminal;
}
