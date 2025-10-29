package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	glam "github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	runtimepkg "github.com/asynkron/goagent/internal/core/runtime"
)

type eventMsg struct{ evt runtimepkg.RuntimeEvent }
type errMsg struct{ err error }

type model struct {
	// Agent
	agent   *runtimepkg.Runtime
	outputs <-chan runtimepkg.RuntimeEvent
	cancel  context.CancelFunc

	// UI
	vp       viewport.Model
	ti       textinput.Model
	width    int
	height   int
	ready    bool
	buf      strings.Builder // transcript (ANSI)
	lastType runtimepkg.EventType

	// Streaming markdown rendering
	glam            *glam.TermRenderer
	currentMD       strings.Builder // accumulating assistant deltas
	currentRendered string          // last rendered ANSI of currentMD
	lastRender      time.Time
	pendingRender   bool

	// Styling
	border    lipgloss.Style
	userStyle lipgloss.Style
}

func newModel(agent *runtimepkg.Runtime, outputs <-chan runtimepkg.RuntimeEvent, cancel context.CancelFunc) *model {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.Placeholder = "Type a prompt… (Enter to send)"
	ti.CharLimit = 0
	ti.Focus()

	vp := viewport.Model{}
	vp.YPosition = 0

	m := model{
		agent:   agent,
		outputs: outputs,
		cancel:  cancel,
		vp:      vp,
		ti:      ti,
		border:  lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")),
	}
	// Initialize a renderer with a reasonable default; we'll rebuild on resize.
	_ = m.rebuildRenderer(80)
	// Initialize user block style; width set on first resize.
	m.userStyle = lipgloss.NewStyle().
		Background(lipgloss.Color("236")).
		Foreground(lipgloss.Color("252")).
		PaddingLeft(1).
		PaddingRight(1).
		PaddingTop(1).
		PaddingBottom(1)
	return &m
}

func waitForEvent(ch <-chan runtimepkg.RuntimeEvent) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return errMsg{fmt.Errorf("runtime outputs closed")}
		}
		return eventMsg{evt: evt}
	}
}

func (m *model) appendLine(s string) {
	m.buf.WriteString(s)
	// Include current streaming render (if any) so the transcript and the
	// in-progress assistant message are visible together.
	m.vp.SetContent(m.buf.String() + m.currentRendered)
	m.vp.GotoBottom()
}

// appendUserBlock renders the given text as a full-width block with
// background and one space padding on left/right, then appends it to the
// transcript and updates the viewport.
func (m *model) appendUserBlock(text string) {
	if !strings.HasSuffix(m.buf.String(), "\n") && m.buf.Len() > 0 {
		m.buf.WriteString("\n")
	}
	block := m.userStyle.Render(text)
	m.buf.WriteString(block)
	if !strings.HasSuffix(m.buf.String(), "\n") {
		m.buf.WriteString("\n")
	}
	m.vp.SetContent(m.buf.String() + m.currentRendered)
	m.vp.GotoBottom()
}

// rebuildRenderer recreates the Glamour renderer with the given wrap width.
func (m *model) rebuildRenderer(wrap int) error {
	if wrap < 10 {
		wrap = 10
	}
	r, err := glam.NewTermRenderer(
		// Avoid auto background detection (which queries the terminal and can
		// inject OSC responses into input). Use a fixed dark style.
		glam.WithStylePath("dark"),
		glam.WithWordWrap(wrap),
	)
	if err != nil {
		return err
	}
	m.glam = r
	return nil
}

// renderCurrent re-renders the current streaming markdown and updates the
// viewport content by composing the transcript with the rendered fragment.
func (m *model) renderCurrent() {
	if m.glam == nil {
		m.currentRendered = m.currentMD.String()
	} else {
		rendered, err := m.glam.Render(m.currentMD.String())
		if err != nil {
			m.currentRendered = m.currentMD.String()
		} else {
			m.currentRendered = rendered
		}
	}
	m.vp.SetContent(m.buf.String() + m.currentRendered)
	m.vp.GotoBottom()
	m.lastRender = time.Now()
	m.pendingRender = false
}

type renderTick struct{}

// scheduleRender throttles re-rendering to avoid excessive work while streaming.
func (m *model) scheduleRender() tea.Cmd {
	const throttle = 80 * time.Millisecond
	now := time.Now()
	if now.Sub(m.lastRender) >= throttle && !m.pendingRender {
		m.renderCurrent()
		return nil
	}
	if m.pendingRender {
		return nil
	}
	m.pendingRender = true
	wait := throttle - now.Sub(m.lastRender)
	if wait < 10*time.Millisecond {
		wait = throttle
	}
	return tea.Tick(wait, func(time.Time) tea.Msg { return renderTick{} })
}

func (m model) Init() tea.Cmd {
	// Start listening to events immediately
	return tea.Batch(waitForEvent(m.outputs), textinput.Blink)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Always let sub-components process the message first so they keep
	// internal state (cursor blink, input, etc.) consistent. We'll add our
	// custom behavior after that when appropriate.
	var cmds []tea.Cmd
	var cmd tea.Cmd
	m.ti, cmd = m.ti.Update(msg)
	cmds = append(cmds, cmd)
	m.vp, cmd = m.vp.Update(msg)
	cmds = append(cmds, cmd)

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Layout: viewport above, textarea locked at bottom
		m.ti.Width = m.width
		// Reserve one line for the input + one for borders spacing
		vpH := m.height - 3
		if vpH < 3 {
			vpH = 3
		}
		m.vp.Width = m.width
		m.vp.Height = vpH
		// Rebuild markdown renderer with the viewport width minus borders.
		_ = m.rebuildRenderer(m.vp.Width - 2)
		// Ensure the user block spans the full viewport width.
		m.userStyle = m.userStyle.Width(m.vp.Width)
		m.ready = true
		return m, nil

	case tea.KeyMsg:
		// Quit keys
		if msg.Type == tea.KeyCtrlC || msg.Type == tea.KeyEsc {
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		}

		// Enter submits the single-line input and renders a full-width user block.
		if msg.Type == tea.KeyEnter {
			prompt := strings.TrimSpace(m.ti.Value())
			if prompt != "" {
				m.agent.SubmitPrompt(prompt)
				m.appendUserBlock(prompt)
				m.ti.Reset()
			}
			return m, tea.Batch(cmds...)
		}
		// For all other key events we already updated components above.
		return m, tea.Batch(cmds...)

	case eventMsg:
		evt := msg.evt
		switch evt.Type {
		case runtimepkg.EventTypeAssistantDelta:
			// Accumulate deltas, throttle Glamour rendering.
			m.currentMD.WriteString(evt.Message)
			m.lastType = evt.Type
			if cmd := m.scheduleRender(); cmd != nil {
				return m, tea.Batch(append(cmds, cmd, waitForEvent(m.outputs))...)
			}
			return m, tea.Batch(append(cmds, waitForEvent(m.outputs))...)
		case runtimepkg.EventTypeAssistantMessage:
			// Finalize: render once more and append to transcript.
			m.renderCurrent()
			// Ensure a separating newline between messages.
			if !strings.HasSuffix(m.buf.String(), "\n") {
				m.buf.WriteString("\n")
			}
			m.buf.WriteString(m.currentRendered)
			if !strings.HasSuffix(m.buf.String(), "\n") {
				m.buf.WriteString("\n")
			}
			// Clear streaming state and refresh viewport to transcript-only.
			m.currentMD.Reset()
			m.currentRendered = ""
			m.vp.SetContent(m.buf.String())
			m.vp.GotoBottom()
			m.lastType = evt.Type
		case runtimepkg.EventTypeStatus:
			line := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render("[status] ") + evt.Message + "\n"
			m.appendLine(line)
		case runtimepkg.EventTypeError:
			line := lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true).Render("[error] ") + evt.Message + "\n"
			m.appendLine(line)
		case runtimepkg.EventTypeRequestInput:
			line := lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Render("[input] ") + evt.Message + "\n"
			m.appendLine(line)
		default:
			m.appendLine(evt.Message + "\n")
		}
		// Keep listening
		return m, tea.Batch(append(cmds, waitForEvent(m.outputs))...)

	case errMsg:
		// Outputs channel closed; keep UI around briefly
		m.appendLine(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("[closed] ") + msg.err.Error() + "\n")
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tea.Quit })
	case renderTick:
		// Time to re-render the streaming markdown.
		m.renderCurrent()
		return m, tea.Batch(cmds...)
	}

	return m, tea.Batch(cmds...)
}

func (m model) View() string {
	if !m.ready {
		return "Initializing…"
	}
	top := m.border.Render(m.vp.View())
	bottom := m.border.Render(m.ti.View())
	return top + "\n" + bottom
}

// Run launches the Bubble Tea TUI with the provided runtime options.
// Returns a POSIX-style exit code.
func Run(ctx context.Context, options runtimepkg.RuntimeOptions) int {
	if strings.TrimSpace(options.APIKey) == "" {
		fmt.Fprintln(os.Stderr, "OPENAI_API_KEY must be set")
		return 1
	}

	// Force streaming for the TUI and disable default output forwarding.
	options.UseStreaming = true
	options.DisableOutputForwarding = true
	// Critical: prevent the runtime from reading stdin while Bubble Tea manages
	// terminal input. Concurrent reads from stdin cause severe input lag.
	options.DisableInputReader = true

	agent, err := runtimepkg.NewRuntime(options)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to create runtime:", err)
		return 1
	}
	outputs := agent.Outputs()

	runCtx, cancel := context.WithCancel(ctx)
	go func() { _ = agent.Run(runCtx) }()

	p := tea.NewProgram(newModel(agent, outputs, cancel), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tui error:", err)
		return 1
	}
	return 0
}
