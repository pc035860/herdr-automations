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

	// detail is non-nil exactly while the history view is open.
	detail *detailModel
}

// detailModel is the history view's own state. It keeps the automation name
// rather than deriving it from the board's cursor, because a config edit can
// shrink the board's rows while the view is open.
type detailModel struct {
	name   string
	runs   []detailRow
	cursor int
}

// detailRow pairs a run with whether its output survived, resolved once when
// the view loads rather than by stat-ing the state dir from View.
type detailRow struct {
	rec       history.Record
	hasOutput bool
}

func loadDetail(name string) (*detailModel, error) {
	runs, err := history.Runs(name, detailLimit)
	if err != nil {
		return nil, err
	}
	d := &detailModel{name: name}
	for _, r := range runs {
		d.runs = append(d.runs, detailRow{rec: r, hasOutput: history.HasOutput(r.RunID)})
	}
	return d, nil
}

// reload refreshes the listed runs, holding the cursor where it was.
func (d *detailModel) reload() {
	next, err := loadDetail(d.name)
	if err != nil || len(next.runs) == 0 {
		return
	}
	d.runs = next.runs
	d.cursor = min(d.cursor, len(d.runs)-1)
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
	latest, err := history.Latest()
	if err != nil {
		// Every automation would otherwise read "never", which is a claim
		// about the automations rather than about the log.
		m.setNotice(failStyle, "cannot read the run log: "+err.Error())
	}
	for _, a := range cfg.Automations {
		r := row{auto: a}
		if rec, ok := latest[a.Name]; ok {
			r.last = &rec
		}
		m.rows = append(m.rows, r)
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
		if next.notice == "" {
			next.notice, next.noticeStyle = m.notice, m.noticeStyle
		}
		next.width = m.width
		if m.detail != nil {
			// Keep the history view open across refreshes, and pick up runs
			// that finished while it was.
			m.detail.reload()
			next.detail = m.detail
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
				d, err := loadDetail(name)
				if err != nil {
					m.setNotice(failStyle, err.Error())
					return m, nil
				}
				if len(d.runs) == 0 {
					m.setNotice(dimStyle, name+" has not run yet")
					return m, nil
				}
				m.detail = d
				m.setNotice(dimStyle, "")
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
	d := m.detail
	switch msg.String() {
	case "q", "esc", "h":
		m.detail = nil
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if d.cursor > 0 {
			d.cursor--
		}
	case "down", "j":
		if d.cursor < len(d.runs)-1 {
			d.cursor++
		}
	case "enter", "o":
		r := d.runs[d.cursor]
		if !r.hasOutput {
			m.setNotice(dimStyle, "no output captured for this run")
			return m, nil
		}
		cmd := pagerCommand(history.OutputPath(r.rec.RunID))
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

func (m model) noticeLine() string { return truncate(m.notice, m.viewWidth()-2) }

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
	nameCol := nameWidth(m.rows, m.viewWidth())
	for i, r := range m.rows {
		// Pad the plain text first: styling before padding would make the
		// escape codes count toward the column widths.
		name := fmt.Sprintf("%-*s", nameCol, truncate(r.auto.Name, nameCol))
		cron := fmt.Sprintf("%-*s", cronCol, truncate(r.auto.Cron, cronCol))
		status := fmt.Sprintf("%-*s", statusCol, statusText(r))
		model := fmt.Sprintf("%-*s", modelCol, truncate(modelText(r.auto), modelCol))
		next := scheduleText(r)

		var line string
		switch {
		case i == m.cursor:
			// One reverse-video span over the whole row: any nested color
			// would end the highlight mid-line.
			line = selectedStyle.Render(" " + name + " " + cron + " " + status + " " + model + " " + next + " ")
		case r.auto.Disabled:
			line = dimStyle.Render(" " + name + " " + cron + " " + status + " " + model + " " + next)
		default:
			line = " " + name + " " + cron + " " +
				statusStyle(r).Render(status) + " " + dimStyle.Render(model) + " " + dimStyle.Render(next)
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
	s := titleStyle.Render(m.detail.name) +
		dimStyle.Render("  enter: output · j/k: move · h/q: back") + "\n\n"
	// One budget for the note, selected or not, so the text does not change
	// length as the cursor moves over it.
	noteWidth := max(0, m.viewWidth()-30)
	for i, r := range m.detail.runs {
		when := fmt.Sprintf("%-17s", r.rec.At.Format("Mon 02 Jan 15:04"))
		status := fmt.Sprintf("%-9s", r.rec.Status)
		note := r.rec.Error
		if note == "" && r.hasOutput {
			note = "output saved"
		}
		note = truncate(note, noteWidth)

		if i == m.detail.cursor {
			s += selectedStyle.Render(" "+when+" "+status+" "+note) + "\n"
			continue
		}
		s += " " + when + " " + recordStyle(r.rec).Render(status) + " " +
			dimStyle.Render(note) + "\n"
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
	// happened this morning or last week — which is also why anything from an
	// earlier day shows its date instead of a clock reading that lies.
	return string(r.last.Status) + " " + shortTime(r.last.At, time.Now())
}

func shortTime(at, now time.Time) string {
	if at.YearDay() == now.YearDay() && at.Year() == now.Year() {
		return at.Format("15:04")
	}
	return at.Format("02 Jan")
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

// Board columns. Everything but the name is sized to its widest rendering and
// stays there; the name is the one field whose content the user chooses, and
// the one that runs long — an automation copied from a Claude Desktop routine
// arrives carrying the routine's name.
const (
	nameMin   = 24
	cronCol   = 16
	statusCol = 15
	// modelCol fits the short names a model is usually asked for by — opus,
	// sonnet, haiku. A fully qualified id runs far longer than any column the
	// board could spare, so it truncates.
	modelCol = 8
	// scheduleCol is not padded — it ends the line — but its widest rendering
	// is what the name column must leave room for: "in 6d 23h · once".
	scheduleCol = 16
)

// nameWidth grows the name column into whatever the viewport has spare, so a
// wide pane shows names in full. It never drops below the width the board
// always had: a narrow pane should look exactly as it did before, truncation
// and all, rather than shrinking the one column the user reads first.
func nameWidth(rows []row, view int) int {
	longest := 0
	for _, r := range rows {
		longest = max(longest, len([]rune(r.auto.Name)))
	}
	// One leading space, four gaps between the five columns, and the columns
	// whose width is fixed.
	spare := view - (1 + 4 + cronCol + statusCol + modelCol + scheduleCol)
	return min(max(longest, nameMin), max(nameMin, spare))
}

// modelText reads back the model the automation asks its agent for. There is
// no model field in the config — a model reaches the agent as a verbatim flag
// in agent_args — so the board parses the same flag the runner passes through.
//
// Every agent kind Herdr can start that takes a model at all spells it
// --model, and the two offering a short form (codex, gemini) spell that -m,
// which none of the others use for anything else — so accepting both is safe.
// The comparison is on the whole token deliberately: claude's --fallback-model
// is a different flag and must not be read as this one.
//
// Nothing found renders as a dash rather than blank. An agent left on its own
// default is a real answer, and a column of empty cells reads as a rendering
// bug instead.
func modelText(a config.Automation) string {
	for i, arg := range a.AgentArgs {
		switch {
		case arg == "--model" || arg == "-m":
			if i+1 < len(a.AgentArgs) {
				return a.AgentArgs[i+1]
			}
		case strings.HasPrefix(arg, "--model="):
			return strings.TrimPrefix(arg, "--model=")
		case strings.HasPrefix(arg, "-m="):
			return strings.TrimPrefix(arg, "-m=")
		}
	}
	return "-"
}

// scheduleText is the row's rightmost field: when this automation runs next,
// or why it doesn't.
func scheduleText(r row) string {
	switch {
	case r.auto.Disabled:
		return "(disabled)"
	case r.auto.Once && spent(r):
		return "(once · done)"
	}
	sched, err := config.CronParser.Parse(r.auto.Cron)
	if err != nil {
		return ""
	}
	at := sched.Next(time.Now())
	next := untilText(time.Until(at), at)
	if r.auto.Once {
		next += " · once"
	}
	return next
}

// untilText renders how far away the next run is. Relative time reads at a
// glance while the run is near; past a week "in 32d" carries less than the
// date itself, so it falls back to the calendar day (the cron column already
// shows the time).
func untilText(d time.Duration, at time.Time) string {
	switch {
	case d >= 7*24*time.Hour:
		return at.Format("1/2")
	case d >= 24*time.Hour:
		return fmt.Sprintf("in %dd %dh", d/(24*time.Hour), d%(24*time.Hour)/time.Hour)
	case d >= time.Hour:
		return fmt.Sprintf("in %dh %dm", d/time.Hour, d%time.Hour/time.Minute)
	case d >= time.Minute:
		return fmt.Sprintf("in %dm", d/time.Minute)
	}
	return "in <1m"
}

// spent reports whether a one-time automation has already had its run, so the
// board stops advertising a next occurrence the daemon will never fire.
//
// The daemon's own answer lives in its schedule state; this reads the last run
// instead, which agrees with it in every case the daemon can produce — once it
// retires an automation there are no further runs to change the answer. A
// manual `run` after the fact is the one way they diverge, and it diverges
// toward the truth: that run really did happen.
func spent(r row) bool {
	return r.last != nil && r.last.Status == history.StatusDone
}

func truncate(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
