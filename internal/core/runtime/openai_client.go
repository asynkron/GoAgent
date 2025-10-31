package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/asynkron/goagent/internal/core/schema"
)

// OpenAIClient wraps the HTTP client required to call the OpenAI Responses API.
type OpenAIClient struct {
	apiKey          string
	model           string
	reasoningEffort string
	httpClient      *http.Client
	tool            schema.ToolDefinition
	baseURL         string
	logger          Logger
	metrics         Metrics
}

const defaultOpenAIBaseURL = "https://api.openai.com/v1"

// NewOpenAIClient configures the client with the provided API key and model identifier.
func NewOpenAIClient(apiKey, model, reasoningEffort, baseURL string, logger Logger, metrics Metrics) (*OpenAIClient, error) {
	if apiKey == "" {
		return nil, errors.New("openai: API key is required")
	}
	if model == "" {
		return nil, errors.New("openai: model is required")
	}
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}
	tool, err := schema.Definition()
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = &NoOpLogger{}
	}
	if metrics == nil {
		metrics = &NoOpMetrics{}
	}
	return &OpenAIClient{
		apiKey:          apiKey,
		model:           model,
		reasoningEffort: reasoningEffort,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		tool:    tool,
		baseURL: baseURL,
		logger:  logger,
		metrics: metrics,
	}, nil
}

// RequestPlan sends the accumulated chat history to OpenAI and returns the
// resulting tool call payload so the runtime can perform validation before
// decoding it.
func (c *OpenAIClient) RequestPlan(ctx context.Context, history []ChatMessage) (ToolCall, error) {
	// Non-streaming path reuses the Responses API implementation without emitting deltas.
	return c.RequestPlanStreamingResponses(ctx, history, nil)
}

// Chat Completions helpers, types, and streaming have been removed.

// RequestPlanStreamingResponses streams using the modern OpenAI Responses API.
// It maps response.output_text.delta chunks to the onDelta callback and collects
// function_call deltas into a ToolCall to return on completion.
func (c *OpenAIClient) RequestPlanStreamingResponses(ctx context.Context, history []ChatMessage, onDelta func(string)) (ToolCall, error) {
	start := time.Now()
	c.logger.Debug(ctx, "Requesting plan from OpenAI",
		Field("model", c.model),
		Field("history_length", len(history)),
	)

	// Optional debug streaming: set GOAGENT_DEBUG_STREAM=1 to enable verbose prints
	debugStream := strings.TrimSpace(os.Getenv("GOAGENT_DEBUG_STREAM")) != ""
	if debugStream {
		fmt.Println("====== STREAM: entering RequestPlanStreamingResponses")
	}

	// Build request
	inputMsgs := buildMessagesFromHistory(history)
	payload, err := c.buildRequestBody(inputMsgs)
	if err != nil {
		return ToolCall{}, fmt.Errorf("openai(responses): encode request: %w", err)
	}

	// Execute request
	resp, err := c.executeRequest(ctx, payload, start)
	if err != nil {
		return ToolCall{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	// Parse stream
	reader := bufio.NewReader(resp.Body)
	parser := newStreamParser(reader, onDelta, debugStream)
	toolCall, err := parser.parse()

	// Record metrics
	duration := time.Since(start)
	if err != nil {
		c.metrics.RecordAPICall(duration, false)
		c.logger.Error(ctx, "OpenAI API stream parsing failed", err,
			Field("duration_ms", duration.Milliseconds()),
		)
		return ToolCall{}, err
	}

	if toolCall.Name != "" {
		c.metrics.RecordAPICall(duration, true)
		c.logger.Debug(ctx, "OpenAI API request completed successfully",
			Field("duration_ms", duration.Milliseconds()),
			Field("tool_name", toolCall.Name),
		)
	} else {
		c.metrics.RecordAPICall(duration, true)
		c.logger.Debug(ctx, "OpenAI API request completed (no tool call)",
			Field("duration_ms", duration.Milliseconds()),
		)
	}

	return toolCall, nil
}

// extractPartialJSONStringField scans a partial JSON object for a given field name
// and returns the raw (still JSON-escaped) string value content if found.
// complete=true when an unescaped closing quote was found.
func extractPartialJSONStringField(buf, field string) (raw string, complete bool, ok bool) {
	// Find the last occurrence to favor the most recent partial chunk.
	key := "\"" + field + "\""
	idx := strings.LastIndex(buf, key)
	if idx == -1 {
		return "", false, false
	}
	// Scan forward: key" : "
	i := idx + len(key)
	// skip whitespace
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\n' || buf[i] == '\t' || buf[i] == '\r') {
		i++
	}
	if i >= len(buf) || buf[i] != ':' {
		return "", false, false
	}
	i++
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\n' || buf[i] == '\t' || buf[i] == '\r') {
		i++
	}
	if i >= len(buf) || buf[i] != '"' {
		return "", false, false
	}
	// value starts after the opening quote
	start := i + 1
	// Walk until an unescaped closing quote or end of buffer
	for i = start; i < len(buf); i++ {
		c := buf[i]
		if c == '\\' {
			// Skip escaped char if present
			if i+1 < len(buf) {
				if buf[i+1] == 'u' {
					// Attempt to skip \uXXXX if complete, otherwise break
					if i+6 <= len(buf) {
						i += 5 // loop will i++ -> total +6
						continue
					}
					// incomplete unicode escape
					return buf[start:i], false, true
				}
				i++
				continue
			}
			// trailing backslash
			return buf[start:i], false, true
		}
		if c == '"' {
			// Found terminating quote
			return buf[start:i], true, true
		}
	}
	// Incomplete string reaches buffer end
	return buf[start:], false, true
}

// decodePartialJSONString decodes a JSON string content (without surrounding quotes)
// while tolerating truncated/incomplete trailing escapes.
func decodePartialJSONString(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		// Escape sequence
		if i+1 >= len(s) {
			// trailing backslash, stop
			break
		}
		esc := s[i+1]
		switch esc {
		case '"', '\\', '/':
			b.WriteByte(esc)
			i++
		case 'b':
			b.WriteByte('\b')
			i++
		case 'f':
			b.WriteByte('\f')
			i++
		case 'n':
			b.WriteByte('\n')
			i++
		case 'r':
			b.WriteByte('\r')
			i++
		case 't':
			b.WriteByte('\t')
			i++
		case 'u':
			if i+6 <= len(s) {
				hex := s[i+2 : i+6]
				if v, err := strconv.ParseInt(hex, 16, 32); err == nil {
					b.WriteRune(rune(v))
					i += 5 // account for \uXXXX (loop adds +1)
				} else {
					// invalid hex, write literally
					b.WriteString("\\u")
					i++
				}
			} else {
				// incomplete unicode escape; stop
				i = len(s)
			}
		default:
			// unknown escape, write literally
			b.WriteByte('\\')
			// if last char was backslash and next is unknown, write it too if present
			if i+1 < len(s) {
				b.WriteByte(esc)
				i++
			}
		}
	}
	return b.String()
}

// extractPartialJSONStringArrayField finds a JSON array of strings under the given field
// name within a partial JSON object and returns the list of fully parsed elements
// encountered so far. The function is tolerant of truncated buffers and will stop
// before an incomplete string or missing closing bracket.
func extractPartialJSONStringArrayField(buf, field string) (values []string, complete bool, ok bool) {
	key := "\"" + field + "\""
	idx := strings.LastIndex(buf, key)
	if idx == -1 {
		return nil, false, false
	}
	i := idx + len(key)
	// skip to ':'
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\n' || buf[i] == '\t' || buf[i] == '\r') {
		i++
	}
	if i >= len(buf) || buf[i] != ':' {
		return nil, false, false
	}
	i++
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\n' || buf[i] == '\t' || buf[i] == '\r') {
		i++
	}
	if i >= len(buf) || buf[i] != '[' {
		return nil, false, false
	}
	// move past '['
	i++
	// Parse string entries until incomplete
	for i < len(buf) {
		// skip whitespace and commas
		for i < len(buf) {
			c := buf[i]
			if c == ' ' || c == '\n' || c == '\t' || c == '\r' || c == ',' {
				i++
				continue
			}
			break
		}
		if i >= len(buf) {
			return values, false, true
		}
		if buf[i] == ']' {
			return values, true, true
		}
		if buf[i] != '"' {
			return values, false, true
		}
		// parse quoted string
		start := i + 1
		j := start
		for j < len(buf) {
			c := buf[j]
			if c == '\\' {
				if j+1 < len(buf) {
					if buf[j+1] == 'u' {
						if j+6 <= len(buf) {
							j += 6
							continue
						}
						return values, false, true
					}
					j += 2
					continue
				}
				return values, false, true
			}
			if c == '"' {
				raw := buf[start:j]
				values = append(values, decodePartialJSONString(raw))
				j++
				i = j
				break
			}
			j++
		}
		if j >= len(buf) {
			return values, false, true
		}
		// loop continues to parse next value or closing bracket
	}
	return values, false, true
}
