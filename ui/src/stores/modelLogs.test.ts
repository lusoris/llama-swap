import { afterEach, expect, it, vi } from "vitest";
import { readDecoded } from "../lib/streamRead";
import { MAX_LOG_STREAM_READS, streamModelLog } from "./modelLogs";

vi.mock("../lib/streamRead", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/streamRead")>();
  return {
    ...actual,
    readDecoded: vi.fn(async function* (_reader, maxReads) {
      yield "last log line";
      throw new actual.StreamReadLimitError(maxReads);
    }),
  };
});

afterEach(() => { vi.unstubAllGlobals(); });

it("appends a visible line and aborts the log stream at its read bound", async () => {
  const mockFetch = vi.fn().mockResolvedValue({ ok: true, body: new ReadableStream() });
  vi.stubGlobal("fetch", mockFetch);
  let latest = "";
  const unsubscribe = streamModelLog("test").subscribe((value) => { latest = value; });
  try {
    await vi.waitFor(() => expect(latest).toBe(
      `last log line\nlog stream stopped after ${MAX_LOG_STREAM_READS} reads; reopen to resume\n`
    ));
    expect(readDecoded).toHaveBeenCalledWith(expect.anything(), MAX_LOG_STREAM_READS);
    expect(mockFetch.mock.calls[0][1].signal.aborted).toBe(true);
  } finally {
    unsubscribe();
  }
});
