import { expect, it, vi } from "vitest";
import { readDecoded, StreamReadLimitError } from "./streamRead";

function readerFor(chunks: Uint8Array[]): ReadableStreamDefaultReader<Uint8Array> {
  return new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(chunk);
      controller.close();
    },
  }).getReader();
}

async function decoded(chunks: Uint8Array[], maxReads: number): Promise<string> {
  let text = "";
  for await (const chunk of readDecoded(readerFor(chunks), maxReads)) text += chunk;
  return text;
}

it("decodes and joins chunks", async () => {
  const encoder = new TextEncoder();
  await expect(decoded([encoder.encode("hi"), encoder.encode(" there")], 3)).resolves.toBe("hi there");
});

it("decodes a multibyte character split across chunks", async () => {
  const bytes = new TextEncoder().encode("a€b");
  await expect(decoded([bytes.slice(0, 2), bytes.slice(2, 3), bytes.slice(3)], 4)).resolves.toBe("a€b");
});

it("propagates a reader rejection", async () => {
  const error = new Error("read failed");
  const reader = new ReadableStream<Uint8Array>({ start(controller) { controller.error(error); } }).getReader();
  await expect(readDecoded(reader, 2).next()).rejects.toBe(error);
});

it("accepts maxReads minus one chunks followed by done", async () => {
  await expect(decoded([new Uint8Array([65]), new Uint8Array([66])], 3)).resolves.toBe("AB");
});

it("rejects maxReads chunks with no done within the bound", async () => {
  await expect(decoded([new Uint8Array([65]), new Uint8Array([66])], 2))
    .rejects.toThrow("stream did not end within 2 reads");
});

it("cancels a stalled live stream at the read bound", async () => {
  const reader = {
    read: vi.fn()
      .mockResolvedValueOnce({ value: new Uint8Array([65]), done: false })
      .mockResolvedValueOnce({ value: new Uint8Array([66]), done: false })
      .mockImplementation(() => new Promise(() => {})),
    cancel: vi.fn().mockResolvedValue(undefined),
  };
  const consume = async () => {
    let text = "";
    for await (const chunk of readDecoded(reader as unknown as ReadableStreamDefaultReader<Uint8Array>, 2)) text += chunk;
    return text;
  };
  const error = await consume().then(() => undefined, (error: unknown) => error);
  expect(error).toBeInstanceOf(StreamReadLimitError);
  expect(reader.read).toHaveBeenCalledTimes(2);
  expect(reader.cancel).toHaveBeenCalledTimes(1);
  expect(reader.cancel).toHaveBeenCalledWith(error);
});

it("flushes an incomplete final character", async () => {
  await expect(decoded([new Uint8Array([0xe2])], 2)).resolves.toBe("�");
});
