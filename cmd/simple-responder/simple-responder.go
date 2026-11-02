package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func handleStreamingChatCompletion(c *gin.Context) {
	// Set headers for streaming
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Transfer-Encoding", "chunked")

	// add a wait to simulate a slow query
	if wait, err := time.ParseDuration(c.Query("wait")); err == nil {
		time.Sleep(wait)
	}

	// Send 10 "asdf" tokens
	for i := 0; i < 10; i++ {
		data := gin.H{
			"created": time.Now().Unix(),
			"choices": []gin.H{
				{
					"index": 0,
					"delta": gin.H{
						"content": "asdf",
					},
					"finish_reason": nil,
				},
			},
		}
		c.SSEvent("message", data)
		c.Writer.Flush()
	}

	// Send final data with usage info
	finalData := gin.H{
		"usage": gin.H{
			"completion_tokens": 10,
			"prompt_tokens":     25,
			"total_tokens":      35,
		},
		// add timings to simulate llama.cpp
		"timings": gin.H{
			"prompt_n":             25,
			"prompt_ms":            13,
			"predicted_n":          10,
			"predicted_ms":         17,
			"predicted_per_second": 10,
		},
	}
	c.SSEvent("message", finalData)
	c.Writer.Flush()

	// Send [DONE]
	c.SSEvent("message", "[DONE]")
	c.Writer.Flush()
}

func handleNonStreamingChatCompletion(c *gin.Context, bodyBytes []byte, responseMessage string) {
	c.Header("Content-Type", "application/json")

	// add a wait to simulate a slow query
	if wait, err := time.ParseDuration(c.Query("wait")); err == nil {
		time.Sleep(wait)
	}

	c.JSON(http.StatusOK, gin.H{
		"responseMessage":  responseMessage,
		"h_content_length": c.Request.Header.Get("Content-Length"),
		"request_body":     string(bodyBytes),
		"usage": gin.H{
			"completion_tokens": 10,
			"prompt_tokens":     25,
			"total_tokens":      35,
		},
		"timings": gin.H{
			"prompt_n":             25,
			"prompt_ms":            13,
			"predicted_n":          10,
			"predicted_ms":         17,
			"predicted_per_second": 10,
		},
	})
}

func chatCompletionsHandler(responseMessage *string) gin.HandlerFunc {
	return func(c *gin.Context) {
		bodyBytes, err := io.ReadAll(c.Request.Body)
		if err != nil {
			log.Printf("failed to read body: %v", err)
		}

		// Check if streaming is requested
		// Query is checked instead of JSON body since that event stream conflicts with other tests
		isStreaming := c.Query("stream") == "true"

		if isStreaming {
			handleStreamingChatCompletion(c)
		} else {
			handleNonStreamingChatCompletion(c, bodyBytes, *responseMessage)
		}
	}
}

func audioSpeechHandler(expectedModel *string) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read request body"})
			return
		}
		defer c.Request.Body.Close()
		modelName := gjson.GetBytes(body, "model").String()
		if modelName != *expectedModel {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid model: %s, expected: %s", modelName, *expectedModel)})
			return
		} else {
			c.JSON(http.StatusOK, gin.H{"message": "ok"})
		}
	}
}

func completionsHandler(responseMessage *string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Type", "application/json")
		c.JSON(http.StatusOK, gin.H{
			"responseMessage": *responseMessage,
			"usage": gin.H{
				"completion_tokens": 10,
				"prompt_tokens":     25,
				"total_tokens":      35,
			},
		})
	}
}

func completionHandler(responseMessage *string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Type", "application/json")
		c.JSON(http.StatusOK, gin.H{
			"responseMessage": *responseMessage,
			"usage": gin.H{
				"completion_tokens": 10,
				"prompt_tokens":     25,
				"total_tokens":      35,
			},
		})
	}
}

func audioTranscriptionsHandler(c *gin.Context) {
	// Parse the multipart form
	if err := c.Request.ParseMultipartForm(10 << 20); err != nil { // 10 MB max memory
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Error parsing multipart form: %s", err)})
		return
	}

	// Get the model from the form values
	model := c.Request.FormValue("model")

	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing model parameter"})
		return
	}

	// Get the file from the form
	file, _, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Error getting file: %s", err)})
		return
	}
	defer file.Close()

	// Read the file content to get its size
	fileBytes, err := io.ReadAll(file)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Error reading file: %s", err)})
		return
	}

	fileSize := len(fileBytes)

	// Return a JSON response with the model and transcription text including file size
	c.JSON(http.StatusOK, gin.H{
		"text":  fmt.Sprintf("The length of the file is %d bytes", fileSize),
		"model": model,

		// expose some header values for testing
		"h_content_type":   c.GetHeader("Content-Type"),
		"h_content_length": c.GetHeader("Content-Length"),
	})
}

func audioVoicesHandler(c *gin.Context) {
	model := c.Query("model")
	c.JSON(http.StatusOK, gin.H{"voices": []string{"voice1"}, "model": model})
}

func slowRespondHandler(c *gin.Context) {
	echo := c.Query("echo")
	delay := c.Query("delay")

	if echo == "" {
		echo = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	}

	// Parse the duration
	if delay == "" {
		delay = "100ms"
	}

	t, err := time.ParseDuration(delay)
	if err != nil {
		c.Header("Content-Type", "text/plain")
		c.String(http.StatusBadRequest, fmt.Sprintf("Invalid duration: %s", err))
		return
	}

	c.Header("Content-Type", "text/plain")
	// A client that went away does not end the response early: the delay per char still runs.
	writeFailed := false
	for _, char := range echo {
		if _, err := c.Writer.Write([]byte(string(char))); err != nil {
			writeFailed = true
		}
		c.Writer.Flush()

		// wait
		<-time.After(t)
	}
	if writeFailed {
		log.Printf("slow-respond: client stopped reading before the echo ended")
	}
}

func testHandler(responseMessage *string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Type", "text/plain")
		c.String(200, *responseMessage)
	}
}

func envHandler(responseMessage *string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Type", "text/plain")
		c.String(200, *responseMessage)

		// Get environment variables
		envVars := os.Environ()

		// Write each environment variable to the response
		for _, envVar := range envVars {
			c.String(200, envVar)
		}
	}
}

func healthHandler(c *gin.Context) {
	c.Header("Content-Type", "application/json")
	c.JSON(200, gin.H{"status": "ok"})
}

func rootHandler(c *gin.Context) {
	c.Header("Content-Type", "text/plain")
	c.String(200, fmt.Sprintf("%s %s", c.Request.Method, c.Request.URL.Path))
}

func sdapiTxt2ImgHandler(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read request body"})
		return
	}
	defer c.Request.Body.Close()

	modelName := gjson.GetBytes(body, "model").String()
	c.JSON(http.StatusOK, gin.H{
		"model":  modelName,
		"images": []string{},
	})
}

func sdapiImg2ImgHandler(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read request body"})
		return
	}
	defer c.Request.Body.Close()

	modelName := gjson.GetBytes(body, "model").String()
	c.JSON(http.StatusOK, gin.H{
		"model":  modelName,
		"images": []string{},
	})
}

func sdapiLorasHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"loras": []string{},
	})
}

func setupRouter(expectedModel, responseMessage *string) *gin.Engine {
	// Create a new Gin router
	r := gin.New()

	// Set up the handler function using the provided response message
	r.POST("/v1/chat/completions", chatCompletionsHandler(responseMessage))

	// for issue #62 to check model name strips profile slug
	// has to be one of the openAI API endpoints that llama-swap proxies
	// curl http://localhost:8080/v1/audio/speech -d '{"model":"profile:TheExpectedModel"}'
	r.POST("/v1/audio/speech", audioSpeechHandler(expectedModel))

	r.POST("/v1/completions", completionsHandler(responseMessage))

	// llama-server compatibility: /completion
	r.POST("/completion", completionHandler(responseMessage))

	// issue #41
	r.POST("/v1/audio/transcriptions", audioTranscriptionsHandler)

	r.GET("/v1/audio/voices", audioVoicesHandler)

	r.GET("/slow-respond", slowRespondHandler)

	r.GET("/test", testHandler(responseMessage))

	r.GET("/env", envHandler(responseMessage))

	// Set up the /health endpoint handler function
	r.GET("/health", healthHandler)

	r.GET("/", rootHandler)

	// SD API endpoints
	r.POST("/sdapi/v1/txt2img", sdapiTxt2ImgHandler)

	r.POST("/sdapi/v1/img2img", sdapiImg2ImgHandler)

	r.GET("/sdapi/v1/loras", sdapiLorasHandler)

	return r
}

func waitForShutdown(sigChan <-chan os.Signal, ignoreSigTerm bool) {
	countSigInt := 0
	var done bool
	for !done {
		sig := <-sigChan
		switch sig {
		case syscall.SIGINT:
			countSigInt++
			if countSigInt > 1 {
				done = true
			} else {
				log.Println("Received SIGINT, send another SIGINT to shutdown")
			}
		case syscall.SIGTERM:
			if ignoreSigTerm {
				log.Println("Ignoring SIGTERM")
			} else {
				log.Println("Received SIGTERM, shutting down")
				done = true
			}
		default:
			done = true
		}
	}
}

func main() {
	gin.SetMode(gin.TestMode)
	// Define a command-line flag for the port
	port := flag.String("port", "8080", "port to listen on")
	expectedModel := flag.String("model", "TheExpectedModel", "model name to expect")

	// Define a command-line flag for the response message
	responseMessage := flag.String("respond", "hi", "message to respond with")

	silent := flag.Bool("silent", false, "disable all logging")

	ignoreSigTerm := flag.Bool("ignore-sig-term", false, "ignore SIGTERM signal")

	flag.Parse() // Parse the command-line flags

	r := setupRouter(expectedModel, responseMessage)

	address := "127.0.0.1:" + *port // Address with the specified port

	srv := &http.Server{
		Addr:    address,
		Handler: r.Handler(),
	}

	// Disable logging if the --silent flag is set
	if *silent {
		gin.SetMode(gin.ReleaseMode)
		gin.DefaultWriter = io.Discard
		log.SetOutput(io.Discard)
	}

	if !*silent {
		fmt.Printf("My PID: %d\n", os.Getpid())
	}

	go func() {
		log.Printf("simple-responder listening on %s\n", address)
		// service connections
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("simple-responder err: %s\n", err)
		}
	}()

	// Wait for interrupt signal to gracefully shutdown the server with
	// a timeout of 5 seconds.
	sigChan := make(chan os.Signal, 1)
	// kill (no param) default send syscall.SIGTERM
	// kill -2 is syscall.SIGINT
	// kill -9 is syscall.SIGKILL but can't be catch, so don't need add it
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	waitForShutdown(sigChan, *ignoreSigTerm)

	log.Println("simple-responder shutting down")
}
