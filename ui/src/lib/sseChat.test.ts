import { describe, expect, it } from "vitest";
import { parseSSEChat } from "./sseChat";

describe("parseSSEChat", () => {
  it("accumulates content and reasoning deltas across SSE lines", () => {
    const sse = [
      ": keepalive",
      "event: message",
      'data: {"choices":[{"delta":{"reasoning_content":"think "}}]}',
      'data: {"choices":[{"delta":{"reasoning":"more"}}]}',
      'data: {"choices":[{"delta":{"content":"hello "}}]}',
      'data: {"choices":[{"delta":{"content":"world"}}]}',
      "data: [DONE]",
      "",
    ].join("\n");

    expect(parseSSEChat(sse)).toEqual({
      reasoning: "think more",
      content: "hello world",
    });
  });

  it("skips malformed SSE JSON lines and keeps subsequent deltas", () => {
    const sse = [
      'data: {"choices":[{"delta":{"content":"first "}}]}',
      "data: {not-valid-json",
      "data: null",
      "data: []",
      'data: {"choices":[{"delta":{"reasoning_content":"plan"}}]}',
      'data: {"choices":[{"delta":{"content":"second"}}]}',
      "data: [DONE]",
    ].join("\n");

    expect(parseSSEChat(sse)).toEqual({
      reasoning: "plan",
      content: "first second",
    });
  });
});
