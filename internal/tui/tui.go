package tui

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	glam "github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	runtimepkg "github.com/asynkron/goagent/internal/core/runtime"
)

type eventMsg struct{ evt runtimepkg.RuntimeEvent }
type errMsg struct{ err error }

type transcriptKind int

const (
	itemPlain transcriptKind = iota
	itemUser
	itemAssistantMD
	itemPlan
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
	ta       textarea.Model
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

	// Activity
	spin       spinner.Model
	streaming  bool
	flashFrame int

	// Styling
	border    lipgloss.Style
	userStyle lipgloss.Style
	planStyle lipgloss.Style

	// Transcript items (dynamic rendering on resize)
	items []transcriptItem

	// Plan tracking
	planSteps []runtimepkg.PlanStep
	planIndex map[string]int
	executing map[string]bool

	// Inline plan snapshot anchoring
	planSnapshotIndex int
}

func newModel(agent *runtimepkg.Runtime, outputs <-chan runtimepkg.RuntimeEvent, cancel context.CancelFunc) *model {
	ta := textarea.New()
	ta.Placeholder = "Type a prompt… (Enter to send)"
	ta.CharLimit = 0
	ta.SetHeight(3)
	ta.Focus()

	vp := viewport.Model{}
	vp.YPosition = 0

	m := model{
		agent:   agent,
		outputs: outputs,
		cancel:  cancel,
		vp:      vp,
		ta:      ta,
		border:  lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")),
	}
	sp := spinner.New()
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
	m.spin = sp
	_ = m.rebuildRenderer(80)
	// Bright purple rounded border, transparent background, 1-char horizontal padding.
	m.userStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("129")).
		Foreground(lipgloss.Color("252")).
		PaddingLeft(1).
		PaddingRight(1)
	// Plan panel style (panel block similar to user input), purple rounded border
	m.planStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("129")).
		Foreground(lipgloss.Color("252")).
		PaddingLeft(1).
		PaddingRight(1)
	m.planSnapshotIndex = -1
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
		case itemPlan:
			// Render stored snapshot text (keeps historical integrity)
			out.WriteString(it.text)
			if !strings.HasSuffix(it.text, "\n") {
				out.WriteString("\n")
			}
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

// recalcLayout recomputes viewport sizes based on current terminal size and
// the number of lines needed to render the plan panel so it stays visible.
func (m *model) recalcLayout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	// Set textarea width to fit inside the bordered container
	inner := m.width - 2
	if inner < 1 {
		inner = 1
	}
	m.ta.SetWidth(inner)
	// Inline plan: do not reserve rows; it's part of transcript content.
	vpH := m.height - 3
	if vpH < 3 {
		vpH = 3
	}
	m.vp.Width = m.width
	m.vp.Height = vpH
	_ = m.rebuildRenderer(m.vp.Width - 2)
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

// renderPlan builds an inline checklist for the current plan.
func (m *model) renderPlan() string {
	if len(m.planSteps) == 0 {
		return ""
	}
	var inner strings.Builder
	// Header
	inner.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63")).Render("Plan:"))
	inner.WriteString("\n")
	// Lines
	for _, step := range m.planSteps {
		id := step.ID
		title := strings.TrimSpace(step.Title)
		if title == "" {
			title = id
		}
		// Determine status
		status := string(step.Status)
		if m.executing != nil && m.executing[id] {
			status = "executing"
		} else if status == "" {
			status = "pending"
		}
		var box, color string
		switch status {
		case string(runtimepkg.PlanCompleted):
			box, color = "[x]", "70" // green
		case string(runtimepkg.PlanFailed):
			box, color = "[!]", "196" // red
		case "executing":
			box, color = "[~]", "214" // yellow/orange
		default:
			if len(step.WaitingForID) > 0 {
				box, color = "[ ]", "244" // waiting
			} else {
				box, color = "[ ]", "33" // ready
			}
		}
		line := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(box)
		titleStyled := lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(" " + title)
		inner.WriteString(line)
		inner.WriteString(titleStyled)
		inner.WriteString("\n")
	}
	// Render as a bordered panel. Set the width so the final block (including
	// inner border and left/right padding) fits inside the viewport content.
	// Subtract 4 = 2 for padding (1+1) + 2 for the panel's own border.
	panelWidth := m.vp.Width - 4
	if panelWidth < 1 {
		panelWidth = 1
	}
	return m.planStyle.Width(panelWidth).Render(inner.String())
}

// setPlan loads the plan steps and builds a fast index.
func (m *model) setPlan(steps []runtimepkg.PlanStep) {
	m.planSteps = make([]runtimepkg.PlanStep, len(steps))
	copy(m.planSteps, steps)
	m.planIndex = make(map[string]int, len(steps))
	for i, s := range m.planSteps {
		m.planIndex[s.ID] = i
	}
	if m.executing == nil {
		m.executing = make(map[string]bool)
	} else {
		for k := range m.executing {
			delete(m.executing, k)
		}
	}
	// Anchor a new inline plan snapshot in the transcript and track its index.
	snapshot := m.renderPlan()
	m.items = append(m.items, transcriptItem{kind: itemPlan, text: snapshot})
	m.planSnapshotIndex = len(m.items) - 1
	m.recalcLayout()
}

// updateStepStatus adjusts the tracked status for a plan step.
func (m *model) updateStepStatus(stepID string, status any) {
	if m.planIndex == nil {
		return
	}
	idx, ok := m.planIndex[stepID]
	if !ok || idx < 0 || idx >= len(m.planSteps) {
		return
	}
	switch v := status.(type) {
	case runtimepkg.PlanStatus:
		m.planSteps[idx].Status = v
		delete(m.executing, stepID)
	case string:
		switch strings.ToLower(v) {
		case "completed":
			m.planSteps[idx].Status = runtimepkg.PlanCompleted
			delete(m.executing, stepID)
		case "failed":
			m.planSteps[idx].Status = runtimepkg.PlanFailed
			delete(m.executing, stepID)
		case "executing":
			if m.executing == nil {
				m.executing = make(map[string]bool)
			}
			m.executing[stepID] = true
		default:
			// pending/waiting
		}
	}
	// Update the inline plan snapshot in place so the anchored panel reflects
	// the latest statuses for this pass.
	if m.planSnapshotIndex >= 0 && m.planSnapshotIndex < len(m.items) {
		m.items[m.planSnapshotIndex].text = m.renderPlan()
	}
	m.recalcLayout()
}

// ensureStep adds a step placeholder if it's missing so we can render it
// inline even when the initial plan payload wasn't parsed.
func (m *model) ensureStep(stepID, title string) {
	if stepID == "" {
		return
	}
	if m.planIndex == nil {
		m.planIndex = make(map[string]int)
	}
	if _, ok := m.planIndex[stepID]; ok {
		return
	}
	s := runtimepkg.PlanStep{ID: stepID, Title: title, Status: runtimepkg.PlanPending}
	m.planSteps = append(m.planSteps, s)
	m.planIndex[stepID] = len(m.planSteps) - 1
	if m.planSnapshotIndex >= 0 && m.planSnapshotIndex < len(m.items) {
		m.items[m.planSnapshotIndex].text = m.renderPlan()
	} else {
		// Create a snapshot if none exists yet for this pass
		snapshot := m.renderPlan()
		m.items = append(m.items, transcriptItem{kind: itemPlan, text: snapshot})
		m.planSnapshotIndex = len(m.items) - 1
	}
	m.recalcLayout()
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
	return tea.Batch(waitForEvent(m.outputs), textarea.Blink, m.spin.Tick)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	cmds = append(cmds, cmd)
	m.spin, cmd = m.spin.Update(msg)
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	m.vp, cmd = m.vp.Update(msg)
	cmds = append(cmds, cmd)

	switch msg := msg.(type) {
	case spinner.TickMsg:
		if m.streaming {
			m.flashFrame++
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.recalcLayout()
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
			prompt := strings.TrimSpace(m.ta.Value())
			if prompt != "" {
				m.agent.SubmitPrompt(prompt)
				m.appendUserBlock(prompt)
				m.ta.Reset()
			}
			return m, tea.Batch(cmds...)
		}
		return m, tea.Batch(cmds...)

	case eventMsg:
		evt := msg.evt
		switch evt.Type {
		case runtimepkg.EventTypeAssistantDelta:
			if !m.streaming {
				m.streaming = true
			}
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
			if strings.TrimSpace(final) != "" {
				m.items = append(m.items, transcriptItem{kind: itemAssistantMD, text: final})
			}
			m.refresh()
			m.lastType = evt.Type
			m.streaming = false
		case runtimepkg.EventTypeStatus:
			// Update/seed plan step status inline when possible.
			if evt.Metadata != nil {
				// If a full plan is included in metadata, load it.
				if rawPlan, ok := evt.Metadata["plan"]; ok {
					switch p := rawPlan.(type) {
					case []runtimepkg.PlanStep:
						m.setPlan(p)
						m.refresh()
						return m, tea.Batch(append(cmds, waitForEvent(m.outputs))...)
					case []any:
						steps := make([]runtimepkg.PlanStep, 0, len(p))
						for _, it := range p {
							if m1, ok := it.(map[string]any); ok {
								var s runtimepkg.PlanStep
								if id, ok := m1["id"].(string); ok {
									s.ID = id
								}
								if title, ok := m1["title"].(string); ok {
									s.Title = title
								}
								if status, ok := m1["status"].(string); ok {
									s.Status = runtimepkg.PlanStatus(status)
								}
								if deps, ok := m1["waitingForId"].([]any); ok {
									for _, d := range deps {
										if ds, ok := d.(string); ok {
											s.WaitingForID = append(s.WaitingForID, ds)
										}
									}
								}
								steps = append(steps, s)
							}
						}
						if len(steps) > 0 {
							m.setPlan(steps)
							m.refresh()
							return m, tea.Batch(append(cmds, waitForEvent(m.outputs))...)
						}
					}
				}
				if stepID, ok := evt.Metadata["step_id"].(string); ok && stepID != "" {
					title, _ := evt.Metadata["title"].(string)
					m.ensureStep(stepID, title)
					if st, has := evt.Metadata["status"]; has {
						m.updateStepStatus(stepID, st)
					} else {
						m.updateStepStatus(stepID, "executing")
					}
					m.refresh()
					return m, tea.Batch(append(cmds, waitForEvent(m.outputs))...)
				}
			}
			// Fallback: append status line
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
	var inputBlock string
	if m.streaming {
		innerWidth := m.width - 2
		if innerWidth < 1 {
			innerWidth = 1
		}
		bar := m.renderGradientBar(innerWidth)
		inputBlock = bar + "\n" + m.ta.View()
	} else {
		inputBlock = m.ta.View()
	}
	bottom := m.border.Render(inputBlock)
	return top + "\n" + bottom
}

// renderGradientBar renders a full-width, color-cycling bar for streaming state.
func (m *model) renderGradientBar(width int) string {
	if width < 1 {
		width = 1
	}
	var b strings.Builder
	b.Grow(width * 10)
	// Animate hue offset with frame; wave lightness to get a subtle fade.
	baseHue := float64((m.flashFrame * 5) % 360)
	for i := 0; i < width; i++ {
		// Spread hues along the bar and offset over time.
		hue := math.Mod(baseHue+float64(i*3), 360.0)
		sat := 0.85
		// Fade using a sine wave across the bar + time.
		phase := (float64(i)/float64(width))*2*math.Pi + float64(m.flashFrame)/8.0
		light := 0.50 + 0.15*math.Sin(phase)
		hex := hslToHex(hue, sat, light)
		seg := lipgloss.NewStyle().Foreground(lipgloss.Color(hex)).Render("█")
		b.WriteString(seg)
	}
	return b.String()
}

// hslToHex converts H,S,L (H in [0,360), S/L in [0,1]) to a #RRGGBB string.
func hslToHex(h, s, l float64) string {
	r, g, b := hslToRGB(h, s, l)
	return fmt.Sprintf("#%02X%02X%02X", r, g, b)
}

func hslToRGB(h, s, l float64) (uint8, uint8, uint8) {
	c := (1 - math.Abs(2*l-1)) * s
	hp := h / 60.0
	x := c * (1 - math.Abs(math.Mod(hp, 2)-1))
	var r1, g1, b1 float64
	switch {
	case 0 <= hp && hp < 1:
		r1, g1, b1 = c, x, 0
	case 1 <= hp && hp < 2:
		r1, g1, b1 = x, c, 0
	case 2 <= hp && hp < 3:
		r1, g1, b1 = 0, c, x
	case 3 <= hp && hp < 4:
		r1, g1, b1 = 0, x, c
	case 4 <= hp && hp < 5:
		r1, g1, b1 = x, 0, c
	default:
		r1, g1, b1 = c, 0, x
	}
	m := l - c/2
	r := uint8(clamp01((r1 + m)) * 255)
	g := uint8(clamp01((g1 + m)) * 255)
	b := uint8(clamp01((b1 + m)) * 255)
	return r, g, b
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
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

	// Prevent OSC background color queries from contaminating stdin by
	// explicitly setting color profile and background for lipgloss/termenv.
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)

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
