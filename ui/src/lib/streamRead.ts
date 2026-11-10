export const MAX_STREAM_READS = 1_000_000;

export class StreamReadLimitError extends Error {
  constructor(maxReads: number) {
    super(`stream did not end within ${maxReads} reads`);
  }
}

export async function* readDecoded(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  maxReads = MAX_STREAM_READS
): AsyncGenerator<string> {
  const decoder = new TextDecoder();
  for (let i = 0; i < maxReads; i++) {
    const { value, done } = await reader.read();
    if (done) {
      const tail = decoder.decode();
      if (tail) yield tail;
      return;
    }
    yield decoder.decode(value, { stream: true });
  }
  const error = new StreamReadLimitError(maxReads);
  await reader.cancel(error);
  throw error;
}
