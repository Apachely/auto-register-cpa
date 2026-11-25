export interface StreamFrame {
  id: string;
  event: string;
  data: string;
}

/** Parse frames incrementally, including UTF-8/chunk boundaries and CRLF. */
export function createEventDecoder(accept: (frame: StreamFrame) => void) {
  let pending = '';
  return (chunk: string) => {
    pending += chunk;
    if (pending.length > 1024 * 1024) throw new Error('Event frame is too large');
    let boundary: RegExpExecArray | null;
    while ((boundary = /\r?\n\r?\n/.exec(pending))) {
      const block = pending.slice(0, boundary.index);
      pending = pending.slice(boundary.index + boundary[0].length);
      const frame: StreamFrame = { id: '', event: 'message', data: '' };
      const data: string[] = [];
      for (const line of block.split(/\r?\n/)) {
        if (!line || line.startsWith(':')) continue;
        const colon = line.indexOf(':');
        const field = colon < 0 ? line : line.slice(0, colon);
        const value = colon < 0 ? '' : line.slice(colon + 1).replace(/^ /, '');
        if (field === 'data') data.push(value);
        if (field === 'id' && !value.includes('\0')) frame.id = value;
        if (field === 'event') frame.event = value;
      }
      if (data.length) accept({ ...frame, data: data.join('\n') });
    }
  };
}

export async function consumeEventStream(
  stream: ReadableStream<Uint8Array>,
  signal: AbortSignal,
  accept: (frame: StreamFrame) => void
) {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  const decode = createEventDecoder((frame) => {
    if (!signal.aborted) accept(frame);
  });
  const cancel = () => {
    void reader.cancel().catch(() => undefined);
  };
  signal.addEventListener('abort', cancel, { once: true });
  try {
    while (!signal.aborted) {
      const { value, done } = await reader.read();
      if (done) break;
      decode(decoder.decode(value, { stream: true }));
    }
    decode(decoder.decode());
  } finally {
    signal.removeEventListener('abort', cancel);
    await reader.cancel().catch(() => undefined);
    reader.releaseLock();
  }
}

export function waitForReconnect(signal: AbortSignal, delay: number): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) {
      resolve();
      return;
    }
    const finish = () => {
      clearTimeout(timer);
      signal.removeEventListener('abort', finish);
      resolve();
    };
    const timer = setTimeout(finish, delay);
    signal.addEventListener('abort', finish, { once: true });
  });
}
