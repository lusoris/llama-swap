import { parseChatCompletionsLine } from "./chatApi";

export interface SSEChat {
  reasoning: string;
  content: string;
}

export function parseSSEChat(text: string): SSEChat {
  const result: SSEChat = { reasoning: "", content: "" };
  for (const line of text.split("\n")) {
    const chunk = parseChatCompletionsLine(line);
    if (!chunk || chunk.done) continue;
    if (chunk.content) result.content += chunk.content;
    if (chunk.reasoning_content) result.reasoning += chunk.reasoning_content;
  }
  return result;
}
