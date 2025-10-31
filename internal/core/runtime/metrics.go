// Package runtime implements metrics collection for observability.
package runtime

import (
	"sync"
	"sync/atomic"
	"time"
)

// Metrics collects runtime metrics for monitoring and observability.
type Metrics interface {
	// RecordAPICall records an OpenAI API call with duration and success status.
	RecordAPICall(duration time.Duration, success bool)
	// RecordCommandExecution records a command execution with step ID, duration, and success status.
	RecordCommandExecution(stepID string, duration time.Duration, success bool)
	// RecordContextCompaction records a context compaction event.
	RecordContextCompaction(removed int, remaining int)
	// RecordPlanStep records a plan step status change.
	RecordPlanStep(stepID string, status PlanStatus)
	// RecordPass records a plan execution pass.
	RecordPass(passNumber int)
	// GetSnapshot returns the current metrics snapshot.
	GetSnapshot() MetricsSnapshot
	// Reset clears all metrics (useful for testing).
	Reset()
}

// MetricsSnapshot contains a point-in-time view of collected metrics.
type MetricsSnapshot struct {
	APICalls           APICallMetrics
	CommandExecutions  CommandExecutionMetrics
	ContextCompactions int64
	PlanSteps          map[string]int64 // status -> count
	TotalPasses        int64
	LastAPICallTime    time.Time
	LastCommandTime    time.Time
}

// APICallMetrics tracks OpenAI API call statistics.
type APICallMetrics struct {
	Total     int64
	Success   int64
	Failed    int64
	TotalTime time.Duration
	MinTime   time.Duration
	MaxTime   time.Duration
}

// CommandExecutionMetrics tracks command execution statistics.
type CommandExecutionMetrics struct {
	Total     int64
	Success   int64
	Failed    int64
	TotalTime time.Duration
	MinTime   time.Duration
	MaxTime   time.Duration
}

// NoOpMetrics is a metrics collector that discards all metrics.
type NoOpMetrics struct{}

func (n *NoOpMetrics) RecordAPICall(_ time.Duration, _ bool)                    {}
func (n *NoOpMetrics) RecordCommandExecution(_ string, _ time.Duration, _ bool) {}
func (n *NoOpMetrics) RecordContextCompaction(_, _ int)                         {}
func (n *NoOpMetrics) RecordPlanStep(_ string, _ PlanStatus)                    {}
func (n *NoOpMetrics) RecordPass(_ int)                                         {}
func (n *NoOpMetrics) GetSnapshot() MetricsSnapshot                             { return MetricsSnapshot{} }
func (n *NoOpMetrics) Reset()                                                   {}

// InMemoryMetrics is a thread-safe in-memory metrics collector.
type InMemoryMetrics struct {
	mu                 sync.RWMutex
	apiCalls           APICallMetrics
	commandExecutions  CommandExecutionMetrics
	contextCompactions int64
	planSteps          map[string]int64
	totalPasses        int64
	lastAPICallTime    time.Time
	lastCommandTime    time.Time

	// For tracking min/max durations
	apiMinTime atomic.Int64 // nanoseconds
	apiMaxTime atomic.Int64 // nanoseconds
	cmdMinTime atomic.Int64 // nanoseconds
	cmdMaxTime atomic.Int64 // nanoseconds
}

// NewInMemoryMetrics creates a new in-memory metrics collector.
func NewInMemoryMetrics() *InMemoryMetrics {
	m := &InMemoryMetrics{
		planSteps: make(map[string]int64),
	}
	// Initialize min times to a large value so first measurement sets them properly
	m.apiMinTime.Store(int64(time.Hour))
	m.cmdMinTime.Store(int64(time.Hour))
	return m
}

func (m *InMemoryMetrics) RecordAPICall(duration time.Duration, success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.apiCalls.Total++
	if success {
		m.apiCalls.Success++
	} else {
		m.apiCalls.Failed++
	}
	m.apiCalls.TotalTime += duration
	m.lastAPICallTime = time.Now()

	// Update min/max atomically
	durNanos := int64(duration)
	for {
		oldMin := m.apiMinTime.Load()
		if durNanos >= oldMin {
			break
		}
		if m.apiMinTime.CompareAndSwap(oldMin, durNanos) {
			break
		}
	}
	for {
		oldMax := m.apiMaxTime.Load()
		if durNanos <= oldMax {
			break
		}
		if m.apiMaxTime.CompareAndSwap(oldMax, durNanos) {
			break
		}
	}
}

func (m *InMemoryMetrics) RecordCommandExecution(stepID string, duration time.Duration, success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.commandExecutions.Total++
	if success {
		m.commandExecutions.Success++
	} else {
		m.commandExecutions.Failed++
	}
	m.commandExecutions.TotalTime += duration
	m.lastCommandTime = time.Now()

	// Update min/max atomically
	durNanos := int64(duration)
	for {
		oldMin := m.cmdMinTime.Load()
		if durNanos >= oldMin {
			break
		}
		if m.cmdMinTime.CompareAndSwap(oldMin, durNanos) {
			break
		}
	}
	for {
		oldMax := m.cmdMaxTime.Load()
		if durNanos <= oldMax {
			break
		}
		if m.cmdMaxTime.CompareAndSwap(oldMax, durNanos) {
			break
		}
	}
}

func (m *InMemoryMetrics) RecordContextCompaction(removed, remaining int) {
	atomic.AddInt64(&m.contextCompactions, 1)
}

func (m *InMemoryMetrics) RecordPlanStep(stepID string, status PlanStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.planSteps[string(status)]++
}

func (m *InMemoryMetrics) RecordPass(passNumber int) {
	atomic.AddInt64(&m.totalPasses, 1)
}

func (m *InMemoryMetrics) GetSnapshot() MetricsSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	snapshot := MetricsSnapshot{
		APICalls:           m.apiCalls,
		CommandExecutions:  m.commandExecutions,
		ContextCompactions: atomic.LoadInt64(&m.contextCompactions),
		PlanSteps:          make(map[string]int64),
		TotalPasses:        atomic.LoadInt64(&m.totalPasses),
		LastAPICallTime:    m.lastAPICallTime,
		LastCommandTime:    m.lastCommandTime,
	}

	// Copy plan steps map
	for k, v := range m.planSteps {
		snapshot.PlanSteps[k] = v
	}

	// Set min/max from atomic values
	snapshot.APICalls.MinTime = time.Duration(m.apiMinTime.Load())
	snapshot.APICalls.MaxTime = time.Duration(m.apiMaxTime.Load())
	snapshot.CommandExecutions.MinTime = time.Duration(m.cmdMinTime.Load())
	snapshot.CommandExecutions.MaxTime = time.Duration(m.cmdMaxTime.Load())

	// If no calls were made, reset min times
	if snapshot.APICalls.Total == 0 {
		snapshot.APICalls.MinTime = 0
		snapshot.APICalls.MaxTime = 0
	}
	if snapshot.CommandExecutions.Total == 0 {
		snapshot.CommandExecutions.MinTime = 0
		snapshot.CommandExecutions.MaxTime = 0
	}

	return snapshot
}

func (m *InMemoryMetrics) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.apiCalls = APICallMetrics{}
	m.commandExecutions = CommandExecutionMetrics{}
	atomic.StoreInt64(&m.contextCompactions, 0)
	m.planSteps = make(map[string]int64)
	atomic.StoreInt64(&m.totalPasses, 0)
	m.lastAPICallTime = time.Time{}
	m.lastCommandTime = time.Time{}
	m.apiMinTime.Store(int64(time.Hour))
	m.apiMaxTime.Store(0)
	m.cmdMinTime.Store(int64(time.Hour))
	m.cmdMaxTime.Store(0)
}
