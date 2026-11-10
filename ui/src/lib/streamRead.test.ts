import { expect, it } from "vitest";
import { readDecoded } from "./streamRead";

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
  await expect(decoded([bytes.slice(0, 2), bytes.slice(2, 3), bytes.slice(3)], 3)).resolves.toBe("a€b");
});

it("propagates a reader rejection", async () => {
  const error = new Error("read failed");
  const reader = new ReadableStream<Uint8Array>({ start(controller) { controller.error(error); } }).getReader();
  await expect(readDecoded(reader, 2).next()).rejects.toBe(error);
});

it("accepts exactly maxReads chunks followed by done", async () => {
  await expect(decoded([new Uint8Array([65]), new Uint8Array([66])], 2)).resolves.toBe("AB");
});

it("rejects maxReads plus one chunks with the bound in the message", async () => {
  await expect(decoded([new Uint8Array([65]), new Uint8Array([66]), new Uint8Array([67])], 2))
    .rejects.toThrow("stream exceeded 2 reads");
});

it("flushes an incomplete final character", async () => {
  await expect(decoded([new Uint8Array([0xe2])], 1)).resolves.toBe("�");
});
