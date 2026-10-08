// Bound both transport and response parsing, including transports that ignore abort.
export async function studioRequestDeadline<T>(request: (signal: AbortSignal) => Promise<T>, timeoutMs: number, message: string, signal?: AbortSignal): Promise<T> {
  if (signal?.aborted) throw signal.reason ?? new DOMException('Aborted', 'AbortError');
  const controller = new AbortController();
  let rejectRequest: (reason: unknown) => void = () => {};
  const stopped = new Promise<never>((_resolve, reject) => { rejectRequest = reject; });
  const abort = () => {
    const reason = signal?.reason ?? new DOMException('Aborted', 'AbortError');
    controller.abort(reason); rejectRequest(reason);
  };
  signal?.addEventListener('abort', abort, { once: true });
  const timer = setTimeout(() => { const error = new Error(message); controller.abort(error); rejectRequest(error); }, timeoutMs);
  try { return await Promise.race([stopped, request(controller.signal)]); }
  finally { clearTimeout(timer); signal?.removeEventListener('abort', abort); }
}
