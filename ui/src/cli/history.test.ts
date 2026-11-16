import { describe, it, expect, vi } from "vitest";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import {
  evaluateStop,
  toRow,
  renderRow,
  parseRow,
  recordRun,
  renderNarrative,
  setName,
  type HistoryRow,
  type HistoryEntry,
} from "./history";

vi.mock("node:fs/promises", async (importOriginal) => {
  const actual = await importOriginal<typeof import("node:fs/promises")>();
  return {
    ...actual,
    readFile: vi.fn(actual.readFile),
    writeFile: vi.fn(actual.writeFile),
  };
});

const summary = {
  cases: 39,
  attempts: 39,
  passRate: 0.949,
  fullyPassing: 37,
  flaky: 0,
  meanIterations: 2.67,
  meanDurationMs: 7000,
  toolCalls: 65,
  toolErrors: 2,
  noToolAnswers: 0,
  errors: 0,
};

function entry(overrides: Partial<HistoryEntry> = {}): HistoryEntry {
  return {
    timestamp: "2026-08-27T06:03:30.902Z",
    label: "baseline-hard",
    repeat: 1,
    model: "sippy/gemma-4-12B",
    casesDir: "../evals/docs-agent/cases",
    promptSource: "DOCS_AGENT_SYSTEM_PROMPT",
    summary,
    results: [],
    ...overrides,
  };
}

describe("table rows", () => {
  it("renders one line that round-trips through parseRow", () => {
    const row = toRow(entry(), 1);
    const line = renderRow(row);
    expect(line).not.toContain("\n");
    const back = parseRow(line);
    expect(back).not.toBeNull();
    expect(back!.label).toBe("baseline-hard");
    expect(back!.model).toBe("sippy/gemma-4-12B");
    expect(back!.set).toBe("cases");
    expect(back!.repeat).toBe(1);
    expect(back!.passRate).toBeCloseTo(0.949, 3);
    expect(back!.clean).toBe("37/39");
  });

  it("ignores lines that are not table rows", () => {
    expect(parseRow("## What happened in each run")).toBeNull();
    expect(parseRow("| --- | --- |")).toBeNull();
    expect(parseRow("")).toBeNull();
  });
});

describe("renderNarrative", () => {
  const passing = {
    caseId: "a",
    tags: [],
    question: "q",
    runs: [],
    passCount: 1,
    attempts: 1,
    passRate: 1,
    flaky: false,
  };

  it("says nothing failed when nothing did", () => {
    const text = renderNarrative(entry({ results: [passing] }), 1);
    expect(text).toContain("Nothing failed.");
  });

  it("calls out invented configuration separately from other misses", () => {
    const results = [
      {
        ...passing,
        caseId: "negative-model-download",
        tags: ["negative"],
        passCount: 0,
        passRate: 0,
      },
      {
        ...passing,
        caseId: "cmd-minimal-config",
        tags: ["cmd"],
        passCount: 0,
        passRate: 0,
      },
    ];
    const text = renderNarrative(entry({ results }), 1);
    expect(text).toContain("Made things up.");
    expect(text).toContain("negative-model-download");
    expect(text).toContain("Got wrong.");
    expect(text).toContain("cmd-minimal-config");
  });

  it("reports answers given without consulting the docs", () => {
    const text = renderNarrative(
      entry({ summary: { ...summary, noToolAnswers: 2 } }),
      1,
    );
    expect(text).toContain("2 questions straight from memory");
  });

  it("compares against the previous run", () => {
    const previous = toRow(
      entry({ label: "older", summary: { ...summary, passRate: 0.88 } }),
      1,
    );
    const text = renderNarrative(entry(), 2, previous);
    expect(text).toContain("points better than");
    expect(text).toContain("`older`");
  });

  it("describes an equal score as level rather than an improvement", () => {
    const previous = toRow(entry({ label: "older" }), 1);
    expect(renderNarrative(entry(), 2, previous)).toContain("level with");
  });
});

function rows(...rates: number[]): HistoryRow[] {
  return rates.map(
    (passRate, i) => ({ label: `r${i}`, passRate, set: "cases" }) as HistoryRow,
  );
}

describe("evaluateStop", () => {
  it("does not stop while the score keeps improving", () => {
    const v = evaluateStop(rows(0.5, 0.6, 0.7), "cases", 5);
    expect(v.sinceBest).toBe(0);
    expect(v.best).toBeCloseTo(0.7);
    expect(v.shouldStop).toBe(false);
  });

  it("stops after five runs that fail to beat the best", () => {
    const v = evaluateStop(rows(0.9, 0.8, 0.85, 0.9, 0.88, 0.89), "cases", 5);
    expect(v.sinceBest).toBe(5);
    expect(v.shouldStop).toBe(true);
  });

  // A tie is another attempt that did not move the number, not a win.
  it("treats an equal score as a non-improvement", () => {
    expect(evaluateStop(rows(0.9, 0.9), "cases", 5).sinceBest).toBe(1);
  });

  it("resets the counter when the best is beaten", () => {
    const v = evaluateStop(rows(0.9, 0.8, 0.8, 0.95), "cases", 5);
    expect(v.sinceBest).toBe(0);
    expect(v.best).toBeCloseTo(0.95);
  });

  // A holdout run scores differently by construction; mixing it in would look
  // like a regression and trip the rule early.
  it("ignores runs against a different case set", () => {
    const mixed: HistoryRow[] = [
      { label: "a", passRate: 0.9, set: "cases" } as HistoryRow,
      { label: "h", passRate: 0.4, set: "cases-holdout" } as HistoryRow,
      { label: "b", passRate: 0.95, set: "cases" } as HistoryRow,
    ];
    const v = evaluateStop(mixed, "cases", 5);
    expect(v.sinceBest).toBe(0);
    expect(v.best).toBeCloseTo(0.95);
  });

  it("is inert with no history", () => {
    expect(evaluateStop([], "cases", 5).shouldStop).toBe(false);
  });
});

describe("subset runs", () => {
  // A --only run covers different questions, so it must not become the
  // best-so-far and make every full run look like a regression.
  it("scopes a filtered run into its own set", () => {
    expect(setName("../evals/docs-agent/cases")).toBe("cases");
    expect(setName("../evals/docs-agent/cases", "^ttl-")).toBe(
      "cases (subset)",
    );
  });

  it("keeps a subset run out of the full set's stop rule", () => {
    const history: HistoryRow[] = [
      { label: "full", passRate: 0.88, set: "cases" } as HistoryRow,
      { label: "sub", passRate: 1.0, set: "cases (subset)" } as HistoryRow,
    ];
    const v = evaluateStop(history, "../evals/docs-agent/cases", 5);
    expect(v.best).toBeCloseTo(0.88);
    expect(v.bestLabel).toBe("full");
    expect(v.sinceBest).toBe(0);
  });

  it("scopes selected groups independently from full and other group runs", () => {
    expect(setName("../evals/docs-agent/cases", undefined, ["routing"])).toBe(
      "cases [routing]",
    );
    expect(
      setName("../evals/docs-agent/cases", undefined, [
        "routing",
        "operations",
      ]),
    ).toBe("cases [operations,routing]");
    const history: HistoryRow[] = [
      { label: "full", passRate: 0.5, set: "cases" } as HistoryRow,
      { label: "routing", passRate: 0.9, set: "cases [routing]" } as HistoryRow,
    ];
    expect(
      evaluateStop(history, "../evals/docs-agent/cases", 5, undefined, [
        "routing",
      ]).best,
    ).toBeCloseTo(0.9);
  });
});

describe("recordRun", () => {
  it("treats ENOENT as an empty history on the first run and appends next", async () => {
    const dir = await mkdtemp(path.join(tmpdir(), "llama-swap-history-"));
    try {
      const file = path.join(dir, "history.md");
      const first = await recordRun(file, entry({ label: "run-1" }));
      expect(first).toHaveLength(1);
      expect(first[0].index).toBe(1);
      expect(first[0].label).toBe("run-1");

      const second = await recordRun(file, entry({ label: "run-2" }));
      expect(second.map((r) => r.label)).toEqual(["run-1", "run-2"]);

      const text = await readFile(file, "utf8");
      expect(text).toContain("### 1. `run-1`");
      expect(text).toContain("### 2. `run-2`");
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it("rethrows readFile errors other than ENOENT", async () => {
    const dir = await mkdtemp(path.join(tmpdir(), "llama-swap-history-"));
    try {
      const file = path.join(dir, "history.md");
      vi.mocked(writeFile).mockClear();
      vi.mocked(readFile).mockRejectedValueOnce(
        Object.assign(new Error("EIO: i/o error, read"), { code: "EIO" }),
      );
      await expect(recordRun(file, entry())).rejects.toThrow(/EIO/);
      expect(vi.mocked(writeFile)).not.toHaveBeenCalled();
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });
});
