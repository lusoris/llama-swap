import { afterEach, describe, expect, it, vi } from "vitest";
import { mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import {
  main,
  reportCliError,
  CliError,
  applyAgentEvent,
  isEntryPoint,
  type RunState,
} from "./docsAgent";
import type { ChatMessage, ToolCall } from "../lib/types";

function captureStderr(): { read: () => string } {
  let out = "";
  vi.spyOn(process.stderr, "write").mockImplementation((chunk: unknown) => {
    out += typeof chunk === "string" ? chunk : String(chunk);
    return true;
  });
  return { read: () => out };
}

async function catchRejection(promise: Promise<unknown>): Promise<unknown> {
  try {
    await promise;
  } catch (error) {
    return error;
  }
  throw new Error("expected promise to reject");
}

function freshState(): RunState {
  return {
    answer: "",
    reasoning: "",
    iterations: 0,
    doneReason: "error",
    toolCalls: [],
  };
}

afterEach(() => {
  vi.restoreAllMocks();
  process.exitCode = undefined;
});

describe("main CLI validation", () => {
  it("rejects ask without a question with CliError", async () => {
    const error = await catchRejection(main(["ask"]));
    expect(error).toBeInstanceOf(CliError);
    expect((error as CliError).name).toBe("CliError");
    expect((error as CliError).exitCode).toBe(1);
    expect((error as CliError).message).toBe("ask needs a question");
  });

  it("rejects judge without --results with CliError", async () => {
    const error = await catchRejection(main(["judge"]));
    expect(error).toBeInstanceOf(CliError);
    expect((error as CliError).exitCode).toBe(1);
    expect((error as CliError).message).toBe("--results is required");
  });

  it("rejects non-numeric --repeat and non-positive --concurrency with CliError", async () => {
    const badRepeat = await catchRejection(main(["eval", "--repeat", "abc"]));
    expect(badRepeat).toBeInstanceOf(CliError);
    expect((badRepeat as CliError).exitCode).toBe(1);
    expect((badRepeat as CliError).message).toBe("--repeat must be a number");

    const badConcurrency = await catchRejection(
      main(["eval", "--concurrency", "0"]),
    );
    expect(badConcurrency).toBeInstanceOf(CliError);
    expect((badConcurrency as CliError).exitCode).toBe(1);
    expect((badConcurrency as CliError).message).toBe(
      "--concurrency must be a positive integer",
    );
  });
});

describe("reportCliError", () => {
  it("reports unknown command rejection with exitCode 1", async () => {
    const stderr = captureStderr();
    const error = await catchRejection(main(["bogus"]));
    reportCliError(error);
    expect(stderr.read().startsWith('error: unknown command "bogus"')).toBe(
      true,
    );
    expect(process.exitCode).toBe(1);
  });

  it("reports judge --min-score CliError with exitCode 2 and exact stderr", async () => {
    const dir = await mkdtemp(path.join(tmpdir(), "llama-swap-docs-agent-"));
    try {
      const resultsFile = path.join(dir, "results.json");
      await writeFile(
        resultsFile,
        JSON.stringify({
          model: "m",
          casesDir: dir,
          summary: { passRate: 0.5 },
          results: [],
        }),
      );
      const stderr = captureStderr();
      const error = await catchRejection(
        main(["judge", "--results", resultsFile, "--min-score", "0.8"]),
      );
      reportCliError(error);
      expect(process.exitCode).toBe(2);
      expect(stderr.read()).toBe(
        "error: deterministic pass rate 50.0% is below --min-score 80.0%.\n" +
          "  Fix the failing assertions before spending judge tokens.\n",
      );
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it("reports plain Error and non-Error values with exitCode 1", () => {
    const stderr = captureStderr();
    reportCliError(new Error("boom"));
    expect(stderr.read()).toBe("error: boom\n");
    expect(process.exitCode).toBe(1);

    process.exitCode = undefined;
    reportCliError("raw failure");
    expect(stderr.read()).toBe("error: boom\nerror: raw failure\n");
    expect(process.exitCode).toBe(1);
  });
});

const sampleCall: ToolCall = {
  id: "call_1",
  type: "function",
  function: { name: "docs__search_docs", arguments: '{"q":"ttl"}' },
};

describe("applyAgentEvent positive", () => {
  it("accumulates deltas, records tool_end and sets doneReason", () => {
    const state = freshState();
    const deltas: string[] = [];
    const toolMessage: ChatMessage = {
      role: "tool",
      tool_call_id: "call_1",
      name: "docs__search_docs",
      content: "TTL is 0",
    };

    applyAgentEvent(state, { type: "iteration", n: 2, max: 16 });
    applyAgentEvent(state, { type: "reasoning", delta: "lookup " });
    applyAgentEvent(state, { type: "reasoning", delta: "ttl" });
    applyAgentEvent(state, { type: "content", delta: "Hello " }, (t) =>
      deltas.push(t),
    );
    applyAgentEvent(state, { type: "content", delta: "world" }, (t) =>
      deltas.push(t),
    );
    applyAgentEvent(state, {
      type: "tool_end",
      call: sampleCall,
      message: toolMessage,
      ok: true,
      durationMs: 42,
    });
    applyAgentEvent(state, { type: "done", reason: "stop" });

    expect(state.iterations).toBe(2);
    expect(state.reasoning).toBe("lookup ttl");
    expect(state.answer).toBe("Hello world");
    expect(deltas).toEqual(["Hello ", "world"]);
    expect(state.toolCalls).toEqual([
      { name: "docs__search_docs", args: '{"q":"ttl"}', ok: true, durationMs: 42 },
    ]);
    expect(state.doneReason).toBe("stop");
  });
});

describe("applyAgentEvent negative and boundary", () => {
  it("keeps streamed answer on tool-calling assistant_end and records error", () => {
    const state = freshState();
    applyAgentEvent(state, { type: "content", delta: "streamed draft" });
    applyAgentEvent(state, {
      type: "assistant_end",
      message: {
        role: "assistant",
        content: "tool narration",
        tool_calls: [sampleCall],
      },
    });
    expect(state.answer).toBe("streamed draft");

    applyAgentEvent(state, { type: "error", message: "upstream 502" });
    expect(state.error).toBe("upstream 502");
  });

  it("replaces answer on final assistant_end including empty string content", () => {
    const state = freshState();
    state.answer = "draft";
    applyAgentEvent(state, {
      type: "assistant_end",
      message: { role: "assistant", content: "final answer", tool_calls: [] },
    });
    expect(state.answer).toBe("final answer");

    applyAgentEvent(state, {
      type: "assistant_end",
      message: { role: "assistant", content: "" },
    });
    expect(state.answer).toBe("");
  });
});

describe("isEntryPoint", () => {
  it("matches the module path directly and through a symlink", async () => {
    const dir = await mkdtemp(path.join(tmpdir(), "llama-swap-entry-"));
    try {
      const real = path.join(dir, "cli.ts");
      const link = path.join(dir, "link.ts");
      await writeFile(real, "");
      await symlink(real, link);
      expect(isEntryPoint(real, real)).toBe(true);
      expect(isEntryPoint(link, real)).toBe(true);
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it("rejects another file, a missing path and an absent argv entry", async () => {
    const dir = await mkdtemp(path.join(tmpdir(), "llama-swap-entry-"));
    try {
      const real = path.join(dir, "cli.ts");
      const other = path.join(dir, "other.ts");
      await writeFile(real, "");
      await writeFile(other, "");
      expect(isEntryPoint(other, real)).toBe(false);
      expect(isEntryPoint(path.join(dir, "missing.ts"), real)).toBe(false);
      expect(isEntryPoint(undefined, real)).toBe(false);
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });
});
