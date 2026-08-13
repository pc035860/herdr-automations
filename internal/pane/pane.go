// Package pane is the plugin's Herdr overlay pane: a live board of
// automations with their schedule and last run, plus one-key "run now".
package pane

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/DnzzL/herdr-automations/internal/config"
	"github.com/DnzzL/herdr-automations/internal/herdr"
	"github.com/DnzzL/herdr-automations/internal/history"
	"github.com/DnzzL/herdr-automations/internal/runner"
)

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Padding(0, 1)
	selectedStyle = lipgloss.NewStyle().Reverse(true)
	dimStyle      = lipgloss.NewStyle().Faint(true)
	failStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
)

type row struct {
	auto config.Automation
	last *history.Record
}

// detailLimit is how many past runs the history view lists. Enough to see a
// pattern — the same failure three mornings running — without paging.
const detailLimit = 10

type model struct {
	rows        []row
	cursor      int
	width       int
	err         error
	notice      string
	noticeStyle lipgloss.Style

	// detail holds the selected automation's past runs while the history view
	// is open, and is nil on the board itself.
	detail       []history.Record
	detailOf     string
	detailCursor int
}

type refreshMsg struct{}
type ranMsg struct{ err error }
type editedMsg struct{ err error }

func Run() error {
	_, err := tea.NewProgram(load(), tea.WithAltScreen()).Run()
	return err
}

func load() model {
	m := model{}
	cfg, err := config.Load()
	if err != nil {
		m.err = err
		return m
	}
	for _, a := range cfg.Automations {
		last, _ := history.LastRun(a.Name)
		m.rows = append(m.rows, row{auto: a, last: last})
	}
	return m
}

func (m model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return refreshMsg{} })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case refreshMsg:
		next := load()
		if next.cursor = m.cursor; next.cursor >= len(next.rows) {
			next.cursor = max(0, len(next.rows)-1)
		}
		next.notice, next.noticeStyle = m.notice, m.noticeStyle
		next.width = m.width
		if m.detail != nil {
			// Keep the history view open across refreshes, and pick up runs
			// that finished while it was.
			if runs, err := history.Runs(m.detailOf, detailLimit); err == nil && len(runs) > 0 {
				next.detail = runs
			} else {
				next.detail = m.detail
			}
			next.detailOf = m.detailOf
			next.detailCursor = min(m.detailCursor, len(next.detail)-1)
		}
		return next, tick()
	case editedMsg:
		next := load() // pick up whatever was just saved, including new entries
		next.cursor = min(m.cursor, max(0, len(next.rows)-1))
		if msg.err != nil {
			next.setNotice(failStyle, msg.err.Error())
		} else if next.err == nil {
			next.setNotice(okStyle, "config reloaded")
		}
		return next, tick()
	case ranMsg:
		if msg.err != nil {
			m.setNotice(failStyle, msg.err.Error())
		} else {
			m.setNotice(okStyle, "run finished")
		}
		return m, nil
	case tea.KeyMsg:
		if m.detail != nil {
			return m.updateDetail(msg)
		}
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
		case "h":
			// Past runs for this automation. The board only ever shows the
			// latest, and once a run's pane is retired this is the only place
			// its output can still be reached.
			if m.cursor < len(m.rows) {
				name := m.rows[m.cursor].auto.Name
				runs, err := history.Runs(name, detailLimit)
				if err != nil {
					m.setNotice(failStyle, err.Error())
					return m, nil
				}
				if len(runs) == 0 {
					m.setNotice(dimStyle, name+" has not run yet")
					return m, nil
				}
				m.detail, m.detailOf, m.detailCursor = runs, name, 0
				m.notice = ""
			}
		case "r":
			if m.cursor < len(m.rows) {
				a := m.rows[m.cursor].auto
				m.setNotice(dimStyle, "running "+a.Name+"…")
				return m, func() tea.Msg { return ranMsg{err: runner.Run(a, "manual")} }
			}
		case "e":
			// Open the YAML in $EDITOR at the selected automation's line,
			// taking over the pane until the editor exits.
			line := 0
			if m.cursor < len(m.rows) {
				line = config.LineOf(m.rows[m.cursor].auto.Name)
			}
			cmd := editorCommand(config.Path(), line)
			return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return editedMsg{err} })
		case "enter":
			// Jump to the workspace the last run happened in, and close the
			// board so the agent lands in front of you.
			if m.cursor < len(m.rows) {
				last := m.rows[m.cursor].last
				if last == nil || last.WorkspaceID == "" {
					m.setNotice(dimStyle, "no run to jump to yet")
					return m, nil
				}
				if err := herdr.Focus(last.WorkspaceID, last.PaneID); err != nil {
					if errors.Is(err, herdr.ErrGone) {
						m.setNotice(dimStyle,
							"workspace "+last.WorkspaceID+" was closed — nothing to jump to")
					} else {
						m.setNotice(failStyle, err.Error())
					}
					return m, nil
				}
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

// updateDetail drives the history view. It owns every key while open, so the
// board's own bindings cannot fire behind it.
func (m model) updateDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "h":
		m.detail, m.detailOf = nil, ""
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.detailCursor > 0 {
			m.detailCursor--
		}
	case "down", "j":
		if m.detailCursor < len(m.detail)-1 {
			m.detailCursor++
		}
	case "enter", "o":
		r := m.detail[m.detailCursor]
		if !history.HasOutput(r.RunID) {
			m.setNotice(dimStyle, "no output captured for this run")
			return m, nil
		}
		cmd := pagerCommand(history.OutputPath(r.RunID))
		return m, tea.ExecProcess(cmd, func(error) tea.Msg { return refreshMsg{} })
	}
	return m, nil
}

// setNotice stores the message as plain text; View decides how much of it
// fits. A herdr API error is long enough to blow up the pane otherwise.
func (m *model) setNotice(style lipgloss.Style, text string) {
	m.notice = strings.Join(strings.Fields(text), " ")
	m.noticeStyle = style
}

func (m model) noticeLine() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	return truncate(m.notice, width-2)
}

func (m model) View() string {
	if m.detail != nil {
		return m.detailView()
	}
	s := titleStyle.Render("Automations") +
		dimStyle.Render("  r: run · enter: jump · h: history · e: edit · j/k: move · q: quit") + "\n\n"
	if m.err != nil {
		return s + failStyle.Render("config error: "+m.err.Error()) + "\n"
	}
	if len(m.rows) == 0 {
		return s + dimStyle.Render("No automations yet — run `herdr-automations add` or edit "+config.Path()) + "\n"
	}
	for i, r := range m.rows {
		// Pad the plain text first: styling before padding would make the
		// escape codes count toward the column widths.
		name := fmt.Sprintf("%-24s", truncate(r.auto.Name, 24))
		cron := fmt.Sprintf("%-16s", truncate(r.auto.Cron, 16))
		status := fmt.Sprintf("%-14s", statusText(r))
		next := nextRun(r.auto)
		if r.auto.Disabled {
			next = "(disabled)"
		}

		var line string
		switch {
		case i == m.cursor:
			// One reverse-video span over the whole row: any nested color
			// would end the highlight mid-line.
			line = selectedStyle.Render(" " + name + " " + cron + " " + status + " " + next + " ")
		case r.auto.Disabled:
			line = dimStyle.Render(" " + name + " " + cron + " " + status + " " + next)
		default:
			line = " " + name + " " + cron + " " +
				statusStyle(r).Render(status) + " " + dimStyle.Render(next)
		}
		s += line + "\n"
	}
	if m.notice != "" {
		s += "\n" + m.noticeStyle.Render(m.noticeLine()) + "\n"
	}
	return s
}

// detailView lists the selected automation's past runs: when each ran, how it
// ended, and whether its output is still readable.
func (m model) detailView() string {
	s := titleStyle.Render(m.detailOf) +
		dimStyle.Render("  enter: output · j/k: move · h/q: back") + "\n\n"
	for i, r := range m.detail {
		when := r.At.Format("Mon 02 Jan 15:04")
		status := fmt.Sprintf("%-9s", r.Status)
		note := r.Error
		if note == "" && history.HasOutput(r.RunID) {
			note = "output saved"
		}

		plain := fmt.Sprintf(" %-17s %s %s", when, status, note)
		if i == m.detailCursor {
			s += selectedStyle.Render(truncate(plain, m.viewWidth()-1)) + "\n"
			continue
		}
		s += " " + fmt.Sprintf("%-17s", when) + " " +
			recordStyle(r).Render(status) + " " +
			dimStyle.Render(truncate(note, max(0, m.viewWidth()-30))) + "\n"
	}
	if m.notice != "" {
		s += "\n" + m.noticeStyle.Render(m.noticeLine()) + "\n"
	}
	return s
}

func (m model) viewWidth() int {
	if m.width <= 0 {
		return 80
	}
	return m.width
}

func statusText(r row) string {
	if r.last == nil {
		return "never"
	}
	// The time is the point: without it the board cannot say whether "done"
	// happened this morning or last week.
	return string(r.last.Status) + " " + r.last.At.Format("15:04")
}

func statusStyle(r row) lipgloss.Style {
	if r.last == nil {
		return dimStyle
	}
	return recordStyle(*r.last)
}

func recordStyle(r history.Record) lipgloss.Style {
	switch r.Status {
	case history.StatusFailed:
		return failStyle
	case history.StatusDone:
		return okStyle
	default:
		return lipgloss.NewStyle()
	}
}

func nextRun(a config.Automation) string {
	sched, err := config.CronParser.Parse(a.Cron)
	if err != nil {
		return ""
	}
	return "next " + sched.Next(time.Now()).Format("Mon 15:04")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
