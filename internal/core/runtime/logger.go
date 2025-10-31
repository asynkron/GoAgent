package runtime

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"
)

// LogLevel represents the severity of a log entry.
type LogLevel string

const (
	LogLevelDebug LogLevel = "DEBUG"
	LogLevelInfo  LogLevel = "INFO"
	LogLevelWarn  LogLevel = "WARN"
	LogLevelError LogLevel = "ERROR"
)

// LogField represents a key-value pair in structured logging.
type LogField struct {
	Key   string
	Value any
}

// Field creates a LogField from a key-value pair.
func Field(key string, value any) LogField {
	return LogField{Key: key, Value: value}
}

// Logger provides structured logging capabilities with context support.
type Logger interface {
	Debug(ctx context.Context, msg string, fields ...LogField)
	Info(ctx context.Context, msg string, fields ...LogField)
	Warn(ctx context.Context, msg string, fields ...LogField)
	Error(ctx context.Context, msg string, err error, fields ...LogField)
	WithFields(fields ...LogField) Logger
}

// NoOpLogger is a logger that discards all log entries.
type NoOpLogger struct{}

func (n *NoOpLogger) Debug(_ context.Context, _ string, _ ...LogField)          {}
func (n *NoOpLogger) Info(_ context.Context, _ string, _ ...LogField)           {}
func (n *NoOpLogger) Warn(_ context.Context, _ string, _ ...LogField)           {}
func (n *NoOpLogger) Error(_ context.Context, _ string, _ error, _ ...LogField) {}
func (n *NoOpLogger) WithFields(_ ...LogField) Logger                           { return n }

// StdLogger is a logger that writes structured log entries to a writer.
// It includes trace IDs from context when available.
type StdLogger struct {
	fields   []LogField
	minLevel LogLevel
	logger   *log.Logger
	writer   io.Writer
}

// NewStdLogger creates a new logger with the specified minimum log level and writer.
// If writer is nil, logs are discarded (equivalent to NoOpLogger).
func NewStdLogger(minLevel LogLevel, writer io.Writer) *StdLogger {
	if writer == nil {
		return &StdLogger{
			minLevel: minLevel,
			logger:   log.New(io.Discard, "", 0),
			writer:   io.Discard,
		}
	}
	return &StdLogger{
		minLevel: minLevel,
		logger:   log.New(writer, "", 0), // No prefix, we format our own
		writer:   writer,
	}
}

func (s *StdLogger) log(ctx context.Context, level LogLevel, msg string, err error, fields ...LogField) {
	if !s.shouldLog(level) {
		return
	}

	allFields := append(s.fields, fields...)
	if traceID := getTraceID(ctx); traceID != "" {
		allFields = append(allFields, Field("trace_id", traceID))
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("[%s]", time.Now().Format(time.RFC3339)))
	parts = append(parts, fmt.Sprintf("[%s]", level))
	if err != nil {
		parts = append(parts, fmt.Sprintf("[error=%q]", err.Error()))
	}
	parts = append(parts, msg)

	if len(allFields) > 0 {
		var fieldParts []string
		for _, f := range allFields {
			fieldParts = append(fieldParts, fmt.Sprintf("%s=%v", f.Key, f.Value))
		}
		parts = append(parts, fmt.Sprintf("fields=[%s]", strings.Join(fieldParts, " ")))
	}

	s.logger.Println(strings.Join(parts, " "))
}

func (s *StdLogger) shouldLog(level LogLevel) bool {
	levels := map[LogLevel]int{
		LogLevelDebug: 0,
		LogLevelInfo:  1,
		LogLevelWarn:  2,
		LogLevelError: 3,
	}
	return levels[level] >= levels[s.minLevel]
}

func (s *StdLogger) Debug(ctx context.Context, msg string, fields ...LogField) {
	s.log(ctx, LogLevelDebug, msg, nil, fields...)
}

func (s *StdLogger) Info(ctx context.Context, msg string, fields ...LogField) {
	s.log(ctx, LogLevelInfo, msg, nil, fields...)
}

func (s *StdLogger) Warn(ctx context.Context, msg string, fields ...LogField) {
	s.log(ctx, LogLevelWarn, msg, nil, fields...)
}

func (s *StdLogger) Error(ctx context.Context, msg string, err error, fields ...LogField) {
	s.log(ctx, LogLevelError, msg, err, fields...)
}

func (s *StdLogger) WithFields(fields ...LogField) Logger {
	return &StdLogger{
		fields:   append(s.fields, fields...),
		minLevel: s.minLevel,
		logger:   s.logger,
		writer:   s.writer,
	}
}

// traceIDKey is the context key for trace IDs.
type traceIDKey struct{}

// WithTraceID adds a trace ID to the context for request correlation.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

// getTraceID extracts the trace ID from context, if present.
func getTraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(traceIDKey{}).(string); ok {
		return id
	}
	return ""
}

// generateTraceID creates a new trace ID for request correlation.
func generateTraceID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
