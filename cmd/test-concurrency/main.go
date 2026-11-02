package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// parseFlags reads the flags; ok is false after the usage text was printed.
func parseFlags() (baseURL string, models []string, prompt string, maxTokens int, ok bool) {
	promptFlag := flag.String("prompt", "Write a few sentences about the history of computing.", "user message sent to each model")
	maxTokensFlag := flag.Int("max-tokens", 256, "max_tokens per request")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <base-url> <model> [model...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Example: %s -max-tokens 400 http://localhost:8080 A B C D\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if len(args) < 2 {
		flag.Usage()
		return "", nil, "", 0, false
	}

	return args[0], args[1:], *promptFlag, *maxTokensFlag, true
}

func dispatchRequests(ctx context.Context, prog *tea.Program, baseURL string, models []string, prompt string, maxTokens int, wg *sync.WaitGroup) {
	// Chain of triggers ensures requests are sent in the order provided.
	triggers := make([]chan struct{}, len(models))
	for i := range triggers {
		triggers[i] = make(chan struct{}, 1)
	}
	triggers[0] <- struct{}{}

	for i, name := range models {
		wg.Add(1)
		go func(idx int, mdl string) {
			defer wg.Done()

			<-triggers[idx]

			reqStart := time.Now()
			prog.Send(statusMsg{idx: idx, status: statusStreaming})

			if idx+1 < len(triggers) {
				triggers[idx+1] <- struct{}{}
			}

			err := sendRequest(ctx, baseURL, mdl, prompt, maxTokens, idx, func(i int, text string) {
				prog.Send(deltaMsg{idx: i, text: text})
			})

			elapsed := time.Since(reqStart)
			if err != nil {
				prog.Send(statusMsg{idx: idx, status: statusError, elapsed: elapsed, err: err})
			} else {
				prog.Send(statusMsg{idx: idx, status: statusDone, elapsed: elapsed})
			}
		}(i, name)
	}
}

func main() {
	baseURL, models, prompt, maxTokens, ok := parseFlags()
	if !ok {
		os.Exit(1)
	}

	m := newModel(models)
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())

	var wg sync.WaitGroup
	start := time.Now()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatchRequests(ctx, prog, baseURL, models, prompt, maxTokens, &wg)

	if _, err := prog.Run(); err != nil {
		cancel()
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	cancel()

	wg.Wait()
	printSummary(m, start)
}

func printSummary(m *model, start time.Time) {
	fmt.Println("Summary:")
	for _, p := range m.panels {
		switch p.status {
		case statusError:
			fmt.Printf("  [%d] %-20s ERROR   elapsed=%s err=%v\n",
				p.idx, p.model, p.elapsed.Round(time.Millisecond), p.err)
		case statusDone:
			fmt.Printf("  [%d] %-20s done    elapsed=%s\n",
				p.idx, p.model, p.elapsed.Round(time.Millisecond))
		default:
			fmt.Printf("  [%d] %-20s %s\n", p.idx, p.model, p.status)
		}
	}
	fmt.Printf("all done in %s\n", time.Since(start).Round(time.Millisecond))
}
