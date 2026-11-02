package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// deltaSink receives streamed text fragments for a given model panel.
type deltaSink func(idx int, text string)

type streamDelta struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
}

type streamChoice struct {
	Delta streamDelta `json:"delta"`
}

type streamChunk struct {
	Choices []streamChoice `json:"choices"`
}

// sendRequest streams a chat completion and forwards each content/reasoning
// delta to sink. Reasoning and assistant content are emitted into the same
// stream so they render together.
func sendRequest(ctx context.Context, baseURL, model, prompt string, maxTokens, idx int, sink deltaSink) error {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"max_tokens": maxTokens,
		"stream":     true,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("status %d: error reading body: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	return scanStream(resp.Body, idx, sink)
}

func scanStream(r io.Reader, idx int, sink deltaSink) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		data, done, ok := sseData(scanner.Text())
		if done {
			break
		}
		if !ok {
			continue
		}
		emitChunk(data, idx, sink)
	}

	return scanner.Err()
}

// sseData extracts payload data from an SSE line and reports completion.
func sseData(line string) (data string, done bool, ok bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false, false
	}
	data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" {
		return "", false, false
	}
	if data == "[DONE]" {
		return "", true, false
	}
	return data, false, true
}

// emitChunk unmarshals a stream chunk and emits reasoning and content deltas.
func emitChunk(data string, idx int, sink deltaSink) {
	var chunk streamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return
	}
	for _, c := range chunk.Choices {
		if c.Delta.ReasoningContent != "" {
			sink(idx, c.Delta.ReasoningContent)
		}
		if c.Delta.Content != "" {
			sink(idx, c.Delta.Content)
		}
	}
}
