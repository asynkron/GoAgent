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

type transcriptKind int

const (
	itemPlain transcriptKind = iota
	itemUser
	itemAssistantMD
)

type transcriptItem struct {
	kind transcriptKind
	text string // raw content; assistant content is markdown
}

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

	// Transcript items (dynamic rendering on resize)
	items []transcriptItem
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
	_ = m.rebuildRenderer(80)
	// Bright purple rounded border, transparent background, 1-char horizontal padding.
	m.userStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("129")).
		Foreground(lipgloss.Color("252")).
		PaddingLeft(1).
		PaddingRight(1)
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

// renderTranscript renders all transcript items according to current width.
func (m *model) renderTranscript() string {
	var out strings.Builder
	// Compute inner content width for the user block so that the final
	// rendered block (content + left/right padding + left/right border)
	// exactly fits inside the viewport width.
	// left/right padding = 2, left/right border = 2 -> subtract 4.
	userWidth := m.vp.Width - 4
	if userWidth < 1 {
		userWidth = 1
	}
	for _, it := range m.items {
		switch it.kind {
		case itemUser:
			block := m.userStyle.Width(userWidth).Render(it.text)
			out.WriteString(block)
			if !strings.HasSuffix(block, "\n") {
				out.WriteString("\n")
			}
		case itemAssistantMD:
			if m.glam == nil {
				out.WriteString(it.text)
			} else if rendered, err := m.glam.Render(it.text); err == nil {
				out.WriteString(rendered)
			} else {
				out.WriteString(it.text)
			}
			if !strings.HasSuffix(out.String(), "\n") {
				out.WriteString("\n")
			}
		default:
			out.WriteString(it.text)
		}
	}
	return out.String()
}

// refresh recomposes the viewport content from transcript + any streaming.
func (m *model) refresh() {
	content := m.renderTranscript()
	if m.currentRendered != "" {
		content += m.currentRendered
	}
	m.vp.SetContent(content)
	m.vp.GotoBottom()
}

func (m *model) appendLine(s string) {
	m.items = append(m.items, transcriptItem{kind: itemPlain, text: s})
	m.refresh()
}

// appendUserBlock appends a full-width user block to the transcript.
func (m *model) appendUserBlock(text string) {
	// Ensure separation if previous plain text didn't end with newline.
	if n := len(m.items); n > 0 {
		last := m.items[n-1]
		if last.kind == itemPlain && !strings.HasSuffix(last.text, "\n") {
			m.items = append(m.items, transcriptItem{kind: itemPlain, text: "\n"})
		}
	}
	m.items = append(m.items, transcriptItem{kind: itemUser, text: text})
	m.refresh()
}

// rebuildRenderer recreates the Glamour renderer with the given wrap width.
func (m *model) rebuildRenderer(wrap int) error {
	if wrap < 10 {
		wrap = 10
	}
	r, err := glam.NewTermRenderer(
		glam.WithStylePath("dark"), // fixed style to avoid OSC queries
		glam.WithWordWrap(wrap),
	)
	if err != nil {
		return err
	}
	m.glam = r
	return nil
}

// renderCurrent re-renders the current streaming markdown and updates the view.
func (m *model) renderCurrent() {
	if m.glam == nil {
		m.currentRendered = m.currentMD.String()
	} else if rendered, err := m.glam.Render(m.currentMD.String()); err == nil {
		m.currentRendered = rendered
	} else {
		m.currentRendered = m.currentMD.String()
	}
	m.refresh()
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
	return tea.Batch(waitForEvent(m.outputs), textinput.Blink)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		m.ti.Width = m.width
		vpH := m.height - 3
		if vpH < 3 {
			vpH = 3
		}
		m.vp.Width = m.width
		m.vp.Height = vpH
		_ = m.rebuildRenderer(m.vp.Width - 2)
		m.ready = true
		m.refresh()
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC || msg.Type == tea.KeyEsc {
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		}
		if msg.Type == tea.KeyEnter {
			prompt := strings.TrimSpace(m.ti.Value())
			if prompt != "" {
				m.agent.SubmitPrompt(prompt)
				m.appendUserBlock(prompt)
				m.ti.Reset()
			}
			return m, tea.Batch(cmds...)
		}
		return m, tea.Batch(cmds...)

	case eventMsg:
		evt := msg.evt
		switch evt.Type {
		case runtimepkg.EventTypeAssistantDelta:
			m.currentMD.WriteString(evt.Message)
			m.lastType = evt.Type
			if cmd := m.scheduleRender(); cmd != nil {
				return m, tea.Batch(append(cmds, cmd, waitForEvent(m.outputs))...)
			}
			return m, tea.Batch(append(cmds, waitForEvent(m.outputs))...)
		case runtimepkg.EventTypeAssistantMessage:
			final := m.currentMD.String()
			m.currentMD.Reset()
			m.currentRendered = ""
			m.items = append(m.items, transcriptItem{kind: itemAssistantMD, text: final})
			m.refresh()
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
		return m, tea.Batch(append(cmds, waitForEvent(m.outputs))...)

	case errMsg:
		m.appendLine(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("[closed] ") + msg.err.Error() + "\n")
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tea.Quit })
	case renderTick:
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

	options.UseStreaming = true
	options.DisableOutputForwarding = true
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
