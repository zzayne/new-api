package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var (
	port        int
	failRate    int // percentage 0-100, applies to non-error-* models
	baseLatency int // ms added to all responses
	streamDelay int // ms between stream chunks
)

var requestCounter atomic.Int64

func init() {
	flag.IntVar(&port, "port", 8080, "listen port")
	flag.IntVar(&failRate, "fail-rate", 0, "random failure percentage for normal models (0-100)")
	flag.IntVar(&baseLatency, "latency", 50, "base latency in ms for all responses")
	flag.IntVar(&streamDelay, "stream-delay", 30, "ms delay between SSE stream chunks")
}

func main() {
	flag.Parse()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/health", handleHealth)

	addr := fmt.Sprintf(":%d", port)
	log.Printf("Mock upstream server on %s (latency=%dms, fail-rate=%d%%, stream-delay=%dms)", addr, baseLatency, failRate, streamDelay)
	log.Printf("Use model names like error-400, error-429, error-safety, slow-2s etc. to trigger specific behaviors")
	log.Fatal(http.ListenAndServe(addr, mux))
}

func nextID() string {
	n := requestCounter.Add(1)
	return fmt.Sprintf("chatcmpl-mock-%d", n)
}

// ---------------------------------------------------------------------------
// Request / types
// ---------------------------------------------------------------------------

type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// ---------------------------------------------------------------------------
// POST /v1/chat/completions
// ---------------------------------------------------------------------------

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "invalid_request_error", "method_not_allowed", "Method not allowed")
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid_request_error", "invalid_request", "Invalid JSON body")
		return
	}

	model := req.Model
	if model == "" {
		model = "gpt-3.5-turbo"
	}

	// Base latency
	if baseLatency > 0 {
		time.Sleep(time.Duration(baseLatency) * time.Millisecond)
	}

	// 1. Model-name-based error routing (highest priority)
	if handleModelError(w, model) {
		return
	}

	// 2. slow-* prefix: add delay then succeed
	if delay, ok := parseSlowDelay(model); ok {
		time.Sleep(delay)
	}

	// 3. Random failure for normal models (controlled by --fail-rate)
	if failRate > 0 && rand.Intn(100) < failRate {
		handleRandomFailure(w, model)
		return
	}

	// 4. Success response
	id := nextID()
	created := time.Now().Unix()

	if req.Stream {
		log.Printf("[%s] -> 200 streaming", model)
		streamResponse(w, id, model, created)
	} else {
		log.Printf("[%s] -> 200 ok", model)
		nonStreamResponse(w, id, model, created)
	}
}

// ---------------------------------------------------------------------------
// Model-name-based error routing
// ---------------------------------------------------------------------------

// handleModelError checks the model name for error-* patterns and writes the
// corresponding error response. Returns true if handled.
func handleModelError(w http.ResponseWriter, model string) bool {
	switch model {
	case "error-400":
		log.Printf("[%s] -> 400 invalid_request (excluded=yes)", model)
		writeError(w, 400, "invalid_request_error", "invalid_request",
			"Invalid request: the model parameter is not valid")
		return true

	case "error-422":
		log.Printf("[%s] -> 422 invalid_parameter (excluded=yes)", model)
		writeError(w, 422, "invalid_request_error", "invalid_parameter",
			"Invalid parameter: temperature must be between 0 and 2")
		return true

	case "error-429":
		log.Printf("[%s] -> 429 rate_limit (excluded=yes)", model)
		w.Header().Set("Retry-After", "5")
		writeError(w, 429, "rate_limit_error", "rate_limit_exceeded",
			"Rate limit exceeded. Please retry after 5 seconds.")
		return true

	case "error-500":
		log.Printf("[%s] -> 500 internal_error (excluded=no)", model)
		writeError(w, 500, "server_error", "internal_error",
			"Internal server error")
		return true

	case "error-503":
		log.Printf("[%s] -> 503 service_unavailable (excluded=no)", model)
		writeError(w, 503, "server_error", "service_unavailable",
			"The server is temporarily overloaded. Please try again later.")
		return true

	case "error-safety":
		log.Printf("[%s] -> 400 prompt_blocked+safety (excluded=yes)", model)
		writeError(w, 400, "invalid_request_error", "prompt_blocked",
			"Your request was blocked due to safety concerns. Content policy violation detected.")
		return true

	case "error-quota":
		log.Printf("[%s] -> 400 insufficient_user_quota (excluded=yes)", model)
		writeError(w, 400, "invalid_request_error", "insufficient_user_quota",
			"Insufficient user quota to complete the request")
		return true

	case "error-context":
		log.Printf("[%s] -> 400 context_length_exceeded (excluded=yes)", model)
		writeError(w, 400, "invalid_request_error", "context_length_exceeded",
			"This model's maximum context length is 8192 tokens. Your messages resulted in 12000 tokens.")
		return true

	case "error-auth":
		log.Printf("[%s] -> 401 invalid_api_key (excluded=no)", model)
		writeError(w, 401, "authentication_error", "invalid_api_key",
			"Invalid API key provided. Please check your API key and try again.")
		return true

	case "error-timeout":
		log.Printf("[%s] -> delaying 30s to simulate timeout (excluded=no)", model)
		time.Sleep(30 * time.Second)
		writeError(w, 504, "server_error", "timeout", "Request timed out")
		return true

	case "error-random":
		return handleRandomError(w, model)
	}

	return false
}

// handleRandomError picks a random error from the pool.
// 60% success (returns false), 40% random error (returns true).
func handleRandomError(w http.ResponseWriter, model string) bool {
	if rand.Intn(100) >= 40 {
		log.Printf("[%s] -> random: success", model)
		return false
	}

	errorModels := []string{
		"error-400", "error-422", "error-429", "error-500",
		"error-503", "error-safety", "error-quota", "error-context", "error-auth",
	}
	selected := errorModels[rand.Intn(len(errorModels))]
	log.Printf("[%s] -> random: picked %s", model, selected)
	return handleModelError(w, selected)
}

// handleRandomFailure is used for --fail-rate on normal (non-error-*) models.
func handleRandomFailure(w http.ResponseWriter, model string) {
	switch rand.Intn(3) {
	case 0:
		log.Printf("[%s] -> random-fail 429", model)
		w.Header().Set("Retry-After", "1")
		writeError(w, 429, "rate_limit_error", "rate_limit_exceeded",
			"Rate limit exceeded. Please retry after 1 second.")
	case 1:
		log.Printf("[%s] -> random-fail 500", model)
		writeError(w, 500, "server_error", "internal_error", "Internal server error")
	case 2:
		log.Printf("[%s] -> random-fail 503", model)
		writeError(w, 503, "server_error", "service_unavailable",
			"The server is temporarily overloaded. Please try again later.")
	}
}

// ---------------------------------------------------------------------------
// slow-* model prefix
// ---------------------------------------------------------------------------

func parseSlowDelay(model string) (time.Duration, bool) {
	if !strings.HasPrefix(model, "slow-") {
		return 0, false
	}
	suffix := model[len("slow-"):]

	// Try Go duration format first (e.g. "2s", "500ms")
	d, err := time.ParseDuration(suffix)
	if err == nil && d > 0 {
		log.Printf("[%s] -> adding %s delay", model, d)
		return d, true
	}

	// Fallback: plain integer as seconds (e.g. "slow-2" = 2s)
	secs, err := strconv.Atoi(suffix)
	if err == nil && secs > 0 {
		d = time.Duration(secs) * time.Second
		log.Printf("[%s] -> adding %s delay", model, d)
		return d, true
	}

	return 0, false
}

// ---------------------------------------------------------------------------
// Response writers
// ---------------------------------------------------------------------------

func writeError(w http.ResponseWriter, status int, errType, errCode, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    errCode,
		},
	})
}

func nonStreamResponse(w http.ResponseWriter, id, model string, created int64) {
	content := fmt.Sprintf("Mock response from %s. Request #%d.", model, requestCounter.Load())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": content},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     10,
			"completion_tokens": 20,
			"total_tokens":      30,
		},
	})
}

func streamResponse(w http.ResponseWriter, id, model string, created int64) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "server_error", "streaming_unsupported", "Streaming not supported")
		return
	}

	words := []string{"This", "is", "a", "mock", "streaming", "response", "from", model + "."}

	for i, word := range words {
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{
				{
					"index": 0,
					"delta": map[string]string{"content": word + " "},
				},
			},
		}
		if i == 0 {
			chunk["choices"].([]map[string]any)[0]["delta"] = map[string]string{
				"role":    "assistant",
				"content": word + " ",
			}
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		if i < len(words)-1 {
			time.Sleep(time.Duration(streamDelay) * time.Millisecond)
		}
	}

	// Final chunk with finish_reason + usage
	final := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]string{},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     10,
			"completion_tokens": len(words),
			"total_tokens":      10 + len(words),
		},
	}
	data, _ := json.Marshal(final)
	fmt.Fprintf(w, "data: %s\n\n", data)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ---------------------------------------------------------------------------
// GET /v1/models
// ---------------------------------------------------------------------------

func handleModels(w http.ResponseWriter, r *http.Request) {
	models := []string{
		// Normal models
		"gpt-3.5-turbo", "gpt-4", "gpt-4o", "gpt-4o-mini",
		// Excluded errors (should be filtered by stats exclusion rules)
		"error-400", "error-422", "error-429",
		"error-safety", "error-quota", "error-context",
		// Non-excluded errors (real channel faults)
		"error-500", "error-503", "error-auth", "error-timeout",
		// Mixed
		"error-random",
		// Slow responses
		"slow-1s", "slow-2s", "slow-5s",
	}
	data := make([]map[string]any, len(models))
	for i, m := range models {
		data[i] = map[string]any{
			"id": m, "object": "model", "owned_by": "mock-upstream",
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
	})
}

// ---------------------------------------------------------------------------
// GET /health
// ---------------------------------------------------------------------------

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"time":     time.Now().Format(time.RFC3339),
		"requests": requestCounter.Load(),
	})
}
