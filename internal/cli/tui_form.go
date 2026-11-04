package cli

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type formField struct {
	label, value, hint string
	secret             bool
	options, selected  []string
	validate           func(string) error
	// resendChosen is set by inlineForm's resend button; see formModel.resend.
	resendChosen bool
}

type formModel struct {
	title, action, message              string
	fields                              []formField
	width, height, cursor, optionCursor int
	input                               textinput.Model
	editing, accepted                   bool
	validate                            func([]formField) error
	// resend adds a second button that submits with resendChosen instead of
	// accepting, so a caller can re-run a side effect (resend an email)
	// without leaving the form.
	resend bool
	// resendDeadline is when the resend button becomes usable again. A caller
	// sets it to lastSend+cooldown so the countdown survives a re-prompt.
	resendDeadline time.Time
}

// resendRemaining is how many whole seconds the resend button stays disabled.
func (m formModel) resendRemaining() int {
	if !m.resend || m.resendDeadline.IsZero() {
		return 0
	}
	wait := time.Until(m.resendDeadline)
	if wait <= 0 {
		return 0
	}
	return int((wait + time.Second - 1) / time.Second)
}

// resendTickMsg drives the resend countdown.
type resendTickMsg struct{}

func resendTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return resendTickMsg{} })
}

func newFormModel(title, action string, fields []formField) formModel {
	input := textinput.New()
	input.Prompt = "> "
	input.KeyMap.Paste.SetKeys("ctrl+v", "ctrl+shift+v", "shift+insert")
	input.SetWidth(60)
	return formModel{title: title, action: action, fields: fields, width: 80, height: 24, input: input}
}

func (m formModel) Init() tea.Cmd { return nil }
func (m *formModel) submit() tea.Cmd {
	for i, f := range m.fields {
		if f.validate != nil {
			if err := f.validate(f.value); err != nil {
				m.cursor = i
				m.message = err.Error()
				return nil
			}
		}
	}
	if m.validate != nil {
		if err := m.validate(m.fields); err != nil {
			m.message = err.Error()
			return nil
		}
	}
	m.accepted = true
	return tea.Quit
}

// submitResend ends the run with the resend flag set. It skips field
// validation on purpose: the caller resends precisely because the code in the
// field was wrong or has not arrived. It does nothing while the cooldown runs.
func (m *formModel) submitResend() tea.Cmd {
	if !m.resend {
		return nil
	}
	if remaining := m.resendRemaining(); remaining > 0 {
		m.message = fmt.Sprintf("You can resend the code in %ds.", remaining)
		return nil
	}
	m.fields[len(m.fields)-1].resendChosen = true
	m.accepted = true
	return tea.Quit
}

func (m formModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.SetWidth(max(1, msg.Width-4))
	case tea.KeyPressMsg:
		k := msg.String()
		if k == "ctrl+c" {
			return m, tea.Quit
		}
		if m.editing {
			f := &m.fields[m.cursor]
			if k == "esc" {
				m.editing = false
				m.input.Blur()
				return m, nil
			}
			if len(f.options) > 0 {
				if k == "enter" {
					m.editing = false
					return m, nil
				}
				if k == "space" {
					value := f.options[m.optionCursor]
					if i := slices.Index(f.selected, value); i >= 0 {
						f.selected = slices.Delete(f.selected, i, i+1)
					} else {
						f.selected = append(f.selected, value)
					}
				}
				m.optionCursor = listCursor(k, m.optionCursor, len(f.options), max(1, m.height-8))
				return m, nil
			}
			if k == "enter" || k == "tab" {
				if f.validate != nil {
					if err := f.validate(m.input.Value()); err != nil {
						m.message = err.Error()
						return m, nil
					}
				}
				f.value = m.input.Value()
				m.editing = false
				m.input.Blur()
				m.message = ""
				if k == "tab" {
					m.cursor = min(m.cursor+1, len(m.fields))
				}
				return m, nil
			}
		} else {
			switch k {
			case "esc", "q":
				return m, tea.Quit
			case "ctrl+s":
				cmd := m.submit()
				return m, cmd
			case "ctrl+r":
				if m.resend {
					cmd := m.submitResend()
					return m, cmd
				}
			case "enter":
				if m.cursor == len(m.fields) {
					cmd := m.submit()
					return m, cmd
				}
				m.editing = true
				m.message = ""
				m.optionCursor = 0
				f := m.fields[m.cursor]
				m.input.SetValue(f.value)
				m.input.CursorEnd()
				m.input.EchoMode = textinput.EchoNormal
				if f.secret {
					m.input.EchoMode = textinput.EchoPassword
				}
				return m, m.input.Focus()
			}
			m.cursor = listCursor(k, m.cursor, len(m.fields)+1, max(1, m.height-10))
			return m, nil
		}
	}
	if m.editing && len(m.fields[m.cursor].options) == 0 {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		if m.input.Err != nil {
			m.message = "Unable to paste. Try your terminal's Paste command."
		} else {
			m.message = ""
		}
		return m, cmd
	}
	return m, nil
}

func (m formModel) View() tea.View {
	if m.editing {
		f := m.fields[m.cursor]
		body := m.input.View()
		if len(f.options) > 0 {
			rows := max(1, m.height-7)
			start := max(0, m.optionCursor-rows+1)
			var lines []string
			for i := start; i < min(len(f.options), start+rows); i++ {
				mark := "[ ] "
				if slices.Contains(f.selected, f.options[i]) {
					mark = "[x] "
				}
				lines = append(lines, selectionRow(mark+f.options[i], i == m.optionCursor, m.width))
			}
			body = strings.Join(lines, "\n")
			return screenView(m.width, m.height, m.title+" / "+f.label, "No selection includes all formats", body, m.message, uiKey("esc", "back"), uiKey("space", "toggle"), uiKey("enter", "done"), uiKey("↑/↓", "select"))
		}
		return screenView(m.width, m.height, m.title+" / "+f.label, f.hint, body, m.message, uiKey("esc", "cancel edit"), uiKey("enter", "apply"), uiKey("tab", "next"), uiKey("ctrl+v", "paste"))
	}
	rows := max(1, m.height-10)
	start := max(0, m.cursor-rows+1)
	var lines []string
	for i := start; i < min(len(m.fields)+1, start+rows); i++ {
		label := m.action
		if i < len(m.fields) {
			f := m.fields[i]
			value := f.value
			if f.secret && value != "" {
				value = "••••••••"
			}
			if len(f.options) > 0 {
				value = strings.Join(f.selected, ", ")
				if value == "" {
					value = "All formats"
				}
			}
			if value == "" {
				value = "Not set"
			}
			label = f.label + "  " + value
		}
		if i == len(m.fields) {
			lines = append(lines, renderActionButton(label, i == m.cursor, false))
		} else {
			lines = append(lines, selectionRow(label, i == m.cursor, m.width))
		}
	}
	if m.cursor < len(m.fields) {
		lines = append(lines, "", m.fields[m.cursor].hint)
	}
	return screenView(m.width, m.height, m.title, "", strings.Join(lines, "\n"), m.message, uiKey("esc", "cancel"), uiKey("enter", "open"), uiKey("ctrl+s", strings.ToLower(m.action)), uiKey("↑/↓", "select"))
}

func requiredField(label string) func(string) error {
	return func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("enter %s", strings.ToLower(label))
		}
		return nil
	}
}
