package runtime

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// streamParser handles parsing of SSE (Server-Sent Events) streams from OpenAI.
type streamParser struct {
	reader                    *bufio.Reader
	onDelta                   func(string)
	debugStream               bool
	toolID                    string
	toolName                  string
	toolArgs                  string
	lastEmittedMessage        string
	lastEmittedReasoningCount int
}

// newStreamParser creates a new stream parser instance.
func newStreamParser(reader *bufio.Reader, onDelta func(string), debugStream bool) *streamParser {
	return &streamParser{
		reader:      reader,
		onDelta:     onDelta,
		debugStream: debugStream,
	}
}

// parse reads and parses the SSE stream until completion or error.
func (p *streamParser) parse() (ToolCall, error) {
	if p.debugStream {
		fmt.Println("====== STREAM: HTTP connected; starting SSE read loop")
	}

	for {
		line, rerr := p.reader.ReadString('\n')
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return ToolCall{}, fmt.Errorf("openai(responses): stream read: %w", rerr)
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ":") {
			continue // keepalive/comment
		}
		if !strings.HasPrefix(line, "data:") && !strings.HasPrefix(line, "data: ") {
			continue
		}
		chunkData := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		if chunkData == "[DONE]" {
			if p.debugStream {
				fmt.Println("------ STREAM: [DONE]")
			}
			break
		}

		evt, err := p.parseEvent(chunkData)
		if err != nil {
			if p.debugStream {
				fmt.Println("------ STREAM: decode-error", err)
			}
			continue
		}

		p.processEvent(evt)
	}

	if p.toolName != "" {
		return ToolCall{ID: p.toolID, Name: p.toolName, Arguments: p.toolArgs}, nil
	}
	// No tool call is valid for plain text responses
	return ToolCall{}, nil
}

// parseEvent parses a single SSE data chunk into an event map.
func (p *streamParser) parseEvent(chunkData string) (map[string]any, error) {
	var evt map[string]any
	if err := json.Unmarshal([]byte(chunkData), &evt); err != nil {
		// Truncate chunk data for error message if too long
		chunkPreview := chunkData
		if len(chunkPreview) > 200 {
			chunkPreview = chunkPreview[:200] + "..."
		}
		return nil, fmt.Errorf("parseEvent: failed to parse JSON event: %w (chunk: %q)", err, chunkPreview)
	}
	if p.debugStream {
		t, _ := evt["type"].(string)
		if t == "" {
			fmt.Println("------ STREAM: event ?")
		} else {
			fmt.Println("------ STREAM:", t)
		}
	}
	return evt, nil
}

// processEvent handles a single stream event and updates parser state.
func (p *streamParser) processEvent(evt map[string]any) {
	t, _ := evt["type"].(string)
	switch t {
	case "response.output_text.delta":
		p.handleOutputTextDelta(evt)
	case "response.function_call.delta", "response.tool_call.delta", "message.function_call.delta", "message.tool_call.delta",
		"response_function_call_delta", "response_tool_call_delta", "response_function_call_arguments_delta":
		p.handleFunctionCallDelta(evt)
	case "response.function_call.arguments.delta", "response.tool_call.arguments.delta", "message.function_call.arguments.delta", "message.tool_call.arguments.delta",
		"response.function_call_arguments.delta", "response.tool_call_arguments.delta":
		p.handleArgumentsDelta(evt)
	case "message.delta", "response.message.delta":
		p.handleMessageDelta(evt)
	case "response.completed", "response.output_text.done", "response.function_call.completed":
		p.handleCompletion(evt)
	}
}

// handleOutputTextDelta processes output text delta events.
func (p *streamParser) handleOutputTextDelta(evt map[string]any) {
	if s, _ := evt["delta"].(string); s != "" {
		if p.onDelta != nil {
			p.onDelta(s)
		}
	}
}

// handleFunctionCallDelta processes function/tool call delta events.
func (p *streamParser) handleFunctionCallDelta(evt map[string]any) {
	if name, _ := evt["name"].(string); name != "" {
		p.toolName = name
	}
	if id, _ := evt["call_id"].(string); id != "" {
		p.resetCall(id)
	}
	// Arguments may be provided as top-level "arguments" string, as a
	// raw delta string, or nested under a delta object.
	if args, _ := evt["arguments"].(string); args != "" {
		p.toolArgs += args
		p.emitMessageDelta(p.toolArgs)
		p.emitReasoningDeltas(p.toolArgs)
	} else if ds, _ := evt["delta"].(string); ds != "" {
		p.toolArgs += ds
		p.emitMessageDelta(p.toolArgs)
		p.emitReasoningDeltas(p.toolArgs)
	} else if dm, _ := evt["delta"].(map[string]any); dm != nil {
		if s, _ := dm["arguments"].(string); s != "" {
			p.toolArgs += s
			p.emitMessageDelta(p.toolArgs)
			p.emitReasoningDeltas(p.toolArgs)
		}
		if n, _ := dm["name"].(string); n != "" {
			p.toolName = n
		}
	}
}

// handleArgumentsDelta processes dedicated arguments delta events.
func (p *streamParser) handleArgumentsDelta(evt map[string]any) {
	if s, _ := evt["delta"].(string); s != "" {
		p.toolArgs += s
		p.emitMessageDelta(p.toolArgs)
		p.emitReasoningDeltas(p.toolArgs)
	}
}

// handleMessageDelta processes message delta events.
func (p *streamParser) handleMessageDelta(evt map[string]any) {
	if dm, _ := evt["delta"].(map[string]any); dm != nil {
		if t, _ := dm["type"].(string); t == "output_text.delta" {
			if s, _ := dm["text"].(string); s != "" && p.onDelta != nil {
				p.onDelta(s)
			}
		}
	} else if arr, _ := evt["delta"].([]any); arr != nil {
		for _, it := range arr {
			if m, _ := it.(map[string]any); m != nil {
				if t, _ := m["type"].(string); t == "output_text.delta" {
					if s, _ := m["text"].(string); s != "" && p.onDelta != nil {
						p.onDelta(s)
					}
				}
			}
		}
	}
}

// handleCompletion processes completion events and extracts final tool call data.
func (p *streamParser) handleCompletion(evt map[string]any) {
	if p.toolArgs == "" || p.toolName == "" || p.toolID == "" {
		if respObj, _ := evt["response"].(map[string]any); respObj != nil {
			if p.toolName == "" {
				if s, ok := findStringInMap(respObj, "name"); ok {
					p.toolName = s
				}
			}
			if p.toolID == "" {
				if s, ok := findStringInMap(respObj, "call_id"); ok {
					p.toolID = s
				}
			}
			if p.toolArgs == "" {
				if s, ok := findStringInMap(respObj, "arguments"); ok {
					p.toolArgs = s
				}
			}
		}
	}
}

// findStringInMap searches a nested map structure for a string value by key using DFS.
func findStringInMap(v any, key string) (string, bool) {
	switch vv := v.(type) {
	case map[string]any:
		if s, ok := vv[key].(string); ok && s != "" {
			return s, true
		}
		for _, child := range vv {
			if s, ok := findStringInMap(child, key); ok {
				return s, true
			}
		}
	case []any:
		for _, child := range vv {
			if s, ok := findStringInMap(child, key); ok {
				return s, true
			}
		}
	}
	return "", false
}

// resetCall resets the parser state when a new tool call ID is observed.
func (p *streamParser) resetCall(newID string) {
	if newID != "" && newID != p.toolID {
		p.toolID = newID
		p.toolArgs = ""
		p.lastEmittedMessage = ""
		p.lastEmittedReasoningCount = 0
	}
}

// emitMessageDelta extracts and emits the "message" field from partial JSON.
func (p *streamParser) emitMessageDelta(buf string) {
	if p.onDelta == nil {
		return
	}
	if raw, _, ok := extractPartialJSONStringField(buf, "message"); ok {
		decoded := decodePartialJSONString(raw)
		if decoded == "" {
			return
		}
		if p.lastEmittedMessage == "" {
			p.onDelta(decoded)
			p.lastEmittedMessage = decoded
			return
		}
		if strings.HasPrefix(decoded, p.lastEmittedMessage) {
			p.onDelta(decoded[len(p.lastEmittedMessage):])
			p.lastEmittedMessage = decoded
		} else if decoded != p.lastEmittedMessage {
			p.onDelta(decoded)
			p.lastEmittedMessage = decoded
		}
	}
}

// emitReasoningDeltas extracts and emits reasoning array entries.
func (p *streamParser) emitReasoningDeltas(buf string) {
	if p.onDelta == nil {
		return
	}
	if vals, _, ok := extractPartialJSONStringArrayField(buf, "reasoning"); ok {
		if p.lastEmittedReasoningCount < len(vals) {
			for i := p.lastEmittedReasoningCount; i < len(vals); i++ {
				if v := strings.TrimSpace(vals[i]); v != "" {
					p.onDelta("\n" + v)
				}
			}
			p.lastEmittedReasoningCount = len(vals)
		}
	}
}
