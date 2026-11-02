package main

// created for issue: #252 https://github.com/mostlygeek/llama-swap/issues/252
// this simple benchmark tool sends a lot of small chat completion requests to llama-swap
// to make sure all the requests are accounted for.
//
// requests can be sent in parallel, and the tool will report the results.
// usage: go run main.go -baseurl http://localhost:8080/v1 -model llama3 -requests 1000 -par 5

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// requestTimeout bounds one chat completion request, as the client timeout did before.
const requestTimeout = 30 * time.Second

type benchmarkConfig struct {
	baseurl         string
	modelName       string
	totalRequests   int
	parallelization int
}

// parseFlags reads the CLI arguments; ok is false after a usage error was printed.
func parseFlags() (cfg benchmarkConfig, ok bool) {
	// ----- CLI arguments ----------------------------------------------------
	var (
		baseurl         string
		modelName       string
		totalRequests   int
		parallelization int
	)

	flag.StringVar(&baseurl, "baseurl", "http://localhost:8080/v1", "Base URL of the API (e.g., https://api.example.com)")
	flag.StringVar(&modelName, "model", "", "Model name to use")
	flag.IntVar(&totalRequests, "requests", 1, "Total number of requests to send")
	flag.IntVar(&parallelization, "par", 1, "Maximum number of concurrent requests")
	flag.Parse()

	if baseurl == "" || modelName == "" {
		fmt.Println("Error: both -baseurl and -model are required.")
		flag.Usage()
		return benchmarkConfig{}, false
	}
	if totalRequests <= 0 {
		fmt.Println("Error: -requests must be greater than 0.")
		return benchmarkConfig{}, false
	}
	if parallelization <= 0 {
		fmt.Println("Error: -parallelization must be greater than 0.")
		return benchmarkConfig{}, false
	}

	return benchmarkConfig{
		baseurl:         baseurl,
		modelName:       modelName,
		totalRequests:   totalRequests,
		parallelization: parallelization,
	}, true
}

func runWorker(workerID int, cfg benchmarkConfig, client *http.Client, requests <-chan int, statusCounts map[int]int, mu *sync.Mutex) {
	for reqID := range requests {
		// Build request payload as a single line JSON string
		payload := `{"model":"` + cfg.modelName + `","max_tokens":100,"stream":false,"messages":[{"role":"user","content":"write a snake game in python"}]}`

		// Send POST request
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			fmt.Sprintf("%s/chat/completions", cfg.baseurl),
			bytes.NewReader([]byte(payload)))
		if err != nil {
			cancel()
			log.Printf("[worker %d][req %d] request creation error: %v", workerID, reqID, err)
			mu.Lock()
			statusCounts[-1]++
			mu.Unlock()
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			log.Printf("[worker %d][req %d] HTTP request error: %v", workerID, reqID, err)
			mu.Lock()
			statusCounts[-1]++
			mu.Unlock()
			continue
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			log.Printf("[worker %d][req %d] read body error: %v", workerID, reqID, err)
		}
		if err := resp.Body.Close(); err != nil {
			log.Printf("[worker %d][req %d] close body error: %v", workerID, reqID, err)
		}
		cancel()

		// Record status code
		mu.Lock()
		statusCounts[resp.StatusCode]++
		mu.Unlock()
	}
}

func startStatusTicker(totalRequests int, statusCounts map[int]int, mu *sync.Mutex) (chan struct{}, chan struct{}) {
	// ----- Status ticker (prints every second) -------------------------------
	done := make(chan struct{})
	tickerDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		startTime := time.Now()
		var stopped bool
		for !stopped {
			select {
			case <-ticker.C:
				mu.Lock()
				// Compute how many requests have completed so far
				completed := 0
				for _, cnt := range statusCounts {
					completed += cnt
				}
				// Calculate duration and progress
				duration := time.Since(startTime)
				progress := completed * 100 / totalRequests
				fmt.Printf("Duration: %v, Completed: %d%% requests\n", duration, progress)
				mu.Unlock()
			case <-done:
				duration := time.Since(startTime)
				fmt.Printf("Duration: %v, Completed: %d%% requests\n", duration, 100)
				close(tickerDone)
				stopped = true
			}
		}
	}()
	return done, tickerDone
}

func printSummary(statusCounts map[int]int, mu *sync.Mutex) {
	// ----- Summary ------------------------------------------------------------
	fmt.Println("\n\n=== HTTP response code summary ===")
	mu.Lock()
	for code, cnt := range statusCounts {
		if code == -1 {
			fmt.Printf("Client-side errors (no HTTP response): %d\n", cnt)
		} else {
			fmt.Printf("%d : %d\n", code, cnt)
		}
	}
	mu.Unlock()
}

func main() {
	cfg, ok := parseFlags()
	if !ok {
		os.Exit(1)
	}

	// ----- HTTP client -------------------------------------------------------
	client := &http.Client{
		Timeout: requestTimeout,
	}

	// ----- Tracking response codes -------------------------------------------
	statusCounts := make(map[int]int) // map[statusCode]count
	var mu sync.Mutex                 // protects statusCounts

	// ----- Request queue (buffered channel) ----------------------------------
	requests := make(chan int, 10) // Buffered channel with capacity 10

	// Goroutine to fill the request queue
	go func() {
		for i := 0; i < cfg.totalRequests; i++ {
			requests <- i + 1
		}
		close(requests)
	}()

	// ----- Worker pool -------------------------------------------------------
	var wg sync.WaitGroup
	for i := 0; i < cfg.parallelization; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			runWorker(workerID, cfg, client, requests, statusCounts, &mu)
		}(i + 1)
	}

	done, tickerDone := startStatusTicker(cfg.totalRequests, statusCounts, &mu)

	// Wait for all workers to finish
	wg.Wait()
	close(done)  // stops the status-update goroutine
	<-tickerDone // give ticker time to finish / print

	printSummary(statusCounts, &mu)
}
