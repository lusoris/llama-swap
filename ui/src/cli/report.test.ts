import { describe, expect, it } from "vitest";
import { formatFailureReport } from "./report";
import type { CaseResult, SuiteSummary } from "./grade";

const baseSummary: SuiteSummary = {
  cases: 2,
  attempts: 2,
  passRate: 0.5,
  fullyPassing: 1,
  flaky: 0,
  meanIterations: 2,
  meanDurationMs: 1500,
  toolCalls: 2,
  toolErrors: 0,
  noToolAnswers: 0,
  errors: 1,
};

describe("formatFailureReport", () => {
  it("reports a clean suite when every case passed", () => {
    const report = formatFailureReport({
      label: "clean",
      model: "m",
      baseUrl: "http://localhost:8080",
      systemPromptSource: "default",
      summary: { ...baseSummary, passRate: 1, fullyPassing: 2, errors: 0 },
      results: [],
    });
    expect(report).toContain("# Docs Agent eval: clean");
    expect(report).toContain("Every case passed every attempt.");
  });

  it("renders failed assertions, tool trace, answer and turn error", () => {
    const failing: CaseResult = {
      caseId: "ttl-default",
      tags: ["ttl"],
      question: "What is the default TTL?",
      passCount: 0,
      attempts: 1,
      passRate: 0,
      flaky: false,
      runs: [
        {
          caseId: "ttl-default",
          attempt: 1,
          answer: "wrong answer",
          reasoning: "",
          toolCalls: [{ name: "docs__search_docs", args: '{"q":"ttl"}', ok: false, durationMs: 10 }],
          iterations: 1,
          doneReason: "error",
          durationMs: 100,
          error: "upstream timeout",
          passed: false,
          assertions: [{ kind: "expect_regex", detail: "0", ok: false }],
        },
      ],
    };
    const report = formatFailureReport({
      model: "m",
      baseUrl: "http://localhost:8080",
      systemPromptSource: "default",
      groups: ["routing", "ops"],
      summary: baseSummary,
      results: [failing],
    });
    expect(report).toContain("- groups: `ops`, `routing`");
    expect(report).toContain("## 1 failing case");
    expect(report).toContain("### `ttl-default` (failed)");
    expect(report).toContain("- `expect_regex`: 0");
    expect(report).toContain("**-> tool error**");
    expect(report).toContain("**Turn error:** upstream timeout");
  });
});
