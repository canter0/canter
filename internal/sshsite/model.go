package sshsite

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/canter0/canter/pricing"
	"github.com/charmbracelet/x/ansi"
)

var (
	ink        = lipgloss.Color("#EDF1E7")
	green      = lipgloss.Color("#ACEC8C")
	muted      = lipgloss.Color("#9AA693")
	dim        = lipgloss.Color("#58644F")
	background = lipgloss.Color("#11180F")
	title      = lipgloss.NewStyle().Foreground(ink).Bold(true)
	accent     = lipgloss.NewStyle().Foreground(green)
	quiet      = lipgloss.NewStyle().Foreground(muted)
	faint      = lipgloss.NewStyle().Foreground(dim)
)

var menu = []struct{ name, description string }{
	{"What is Canter?", "A home for your apps. A say in every change."},
	{"Simple, usage-based pricing", "Start small. Pay for what runs."},
	{"Get started", "Open your workspace or bring your own agent."},
}

// Model contains navigation state for the public terminal site.
type Model struct {
	width, height          int
	page, selected, scroll int
}

func NewModel(width, height int) Model {
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	return Model{width: min(width, 500), height: min(height, 200)}
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, min(msg.Width, 500)), max(1, min(msg.Height, 200))
		m.scroll = 0
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "ctrl+d", "q":
			return m, tea.Quit
		case "esc", "h":
			m.page, m.scroll = 0, 0
		case "1", "2", "3":
			if m.page == 0 {
				m.selected = int(msg.String()[0] - '1')
				m.page, m.scroll = m.selected+1, 0
			}
		case "up", "k":
			if m.page == 0 {
				m.selected = (m.selected + len(menu) - 1) % len(menu)
			} else {
				m.scroll = max(0, m.scroll-1)
			}
		case "down", "j":
			if m.page == 0 {
				m.selected = (m.selected + 1) % len(menu)
			} else {
				m.scroll = min(m.scroll+1, 200)
			}
		case "pgdown":
			if m.page != 0 {
				m.scroll = min(m.scroll+max(1, m.height-10), 200)
			}
		case "pgup":
			m.scroll = max(0, m.scroll-max(1, m.height-10))
		case "left", "backspace":
			m.page, m.scroll = 0, 0
		case "enter", "right", "l":
			if m.page == 0 {
				m.page, m.scroll = m.selected+1, 0
			}
		}
	}
	if m.page != 0 {
		lines := len(strings.Split(m.body(max(1, min(80, m.width-6))), "\n"))
		m.scroll = min(m.scroll, max(0, lines-max(1, min(m.height-9, 29))))
	}
	return m, nil
}

func (m Model) View() tea.View {
	w := max(1, min(80, m.width-6))
	if m.width < 36 || m.height < 14 {
		v := tea.NewView(ansi.Truncate("Canter · resize to 36×14 or larger. q quits.", m.width, ""))
		v.AltScreen = true
		return v
	}
	label := "canter.dev / terminal"
	if m.page > 0 {
		label = []string{"", "about", "pricing", "get started"}[m.page]
	}
	brand := title.Render("canter")
	header := brand + strings.Repeat(" ", max(1, w-6-len(label))) + quiet.Render(label)
	rule := faint.Render(strings.Repeat("─", w))
	body := m.body(w)
	lines := strings.Split(body, "\n")
	available := max(1, min(m.height-9, 29))
	start := min(m.scroll, max(0, len(lines)-available))
	if m.page == 0 && len(lines) > available {
		// Keep the selected menu row in view on short terminals.
		selectedLine := 0
		for i, line := range lines {
			if strings.HasPrefix(ansi.Strip(line), "›") {
				selectedLine = i
				break
			}
		}
		start = min(max(0, selectedLine-available+2), max(0, len(lines)-available))
	}
	end := min(len(lines), start+available)
	visible := strings.Join(lines[start:end], "\n")
	visible += strings.Repeat("\n", max(0, available-(end-start)))
	help := "↑↓ choose   enter explore   1–3 jump   q quit"
	if m.page != 0 {
		help = "esc home   ↑↓ scroll   q quit"
	}
	if w < 50 {
		help = "↑↓ choose · enter · q quit"
		if m.page != 0 {
			help = "↑↓ scroll · esc home · q quit"
		}
	}
	if end < len(lines) && m.page != 0 && w >= 50 {
		help = "↓ more   " + help
	}
	content := header + "\n" + rule + "\n\n" + visible + "\n\n" + rule + "\n" + quiet.Render(ansi.Truncate(help, w, "…"))
	view := tea.NewView(lipgloss.NewStyle().Padding(1, max(3, (m.width-w)/2)).Render(content))
	view.AltScreen = true
	view.BackgroundColor, view.ForegroundColor = background, ink
	view.WindowTitle = "Canter · Infrastructure you can talk to"
	return view
}

func (m Model) body(w int) string {
	p := func(s string) string { return quiet.Render(ansi.Wrap(s, w, "")) }
	link := func(s string) string {
		return ansi.SetHyperlink(s) + accent.Render(ansi.Wrap(s, w, "")) + ansi.ResetHyperlink()
	}
	heading := func(s string) string { return title.Render(ansi.Wrap(s, w, "")) }
	if m.page == 0 {
		var b strings.Builder
		if m.height >= 18 {
			b.WriteString(heading("Infrastructure") + "\n" + accent.Bold(true).Render("you can talk to.") + "\n\n")
		}
		if m.height >= 24 {
			b.WriteString(p("Your apps, running. Your next move, in your hands.") + "\n\n")
		}
		for i, item := range menu {
			row := fmt.Sprintf("  %02d  %s", i+1, item.name)
			if m.selected == i {
				row = accent.Bold(true).Render(fmt.Sprintf("› %02d  %s", i+1, item.name))
			} else {
				row = quiet.Render(row)
			}
			b.WriteString(ansi.Truncate(row, w, "…") + "\n")
			if m.height >= 22 {
				b.WriteString("\n")
			}
		}
		if m.height >= 26 {
			b.WriteString(p(menu[m.selected].description))
		}
		return strings.TrimRight(b.String(), "\n")
	}
	if m.page == 1 {
		return heading("A home for what you're building.") + "\n\n" +
			p("Deploy and manage your apps through Canter or your own agent. Describe what you need, review the plan, and see what's running.") + "\n\n" +
			heading("01  Tell Canter what you need") + "\n" + p("Bring an app and a goal. Canter helps prepare the next change.") + "\n\n" +
			heading("02  Keep control of the plan") + "\n" + p("Review the resources, cost, and impact. You approve the exact change, or set a bounded policy ahead of time.") + "\n\n" +
			heading("03  See what actually happened") + "\n" + p("Canter executes approved changes and records their outcome. Your agent can inspect the same workspace.") + "\n\n" + link("https://canter.dev")
	}
	if m.page == 2 {
		var b strings.Builder
		b.WriteString(heading("Start small. Room to grow.") + "\n\n")
		for _, plan := range pricing.Current().Plans {
			b.WriteString(heading(plan.Name+"  ·  "+money(plan.MonthlyCents)+"/month") + "\n")
			if plan.IncludedUsageCents > 0 {
				b.WriteString(p("Includes " + money(plan.IncludedUsageCents) + " of infrastructure usage each month. Additional usage is billed separately."))
			} else {
				b.WriteString(p("No monthly subscription. Pay for your infrastructure usage."))
			}
			b.WriteString("\n\n")
		}
		b.WriteString(heading("Compute, by the hour") + "\n" + p(fmt.Sprintf("%s per unit over %d hours. Units = the larger of vCPU count or memory rounded up to GiB.", money(pricing.ComputeCentsPerUnitPer720Hours), pricing.HoursPerResourceMonth)) + "\n\n")
		b.WriteString(p("USD. Storage and AI usage are separate. This is a compute estimate, not a complete bill.") + "\n\n" + link("https://canter.dev/pricing"))
		return b.String()
	}
	return heading("Make yourself at home.") + "\n\n" +
		heading("Create your workspace") + "\n" + p("Continue in your browser to create an account.") + "\n" + link("https://canter.dev/create-account") + "\n\n" +
		heading("Already here?") + "\n" + link("https://canter.dev/sign-in") + "\n\n" +
		heading("Bring your own agent") + "\n" + p("Give your coding agent this prompt:") + "\n\n" +
		accent.Render(ansi.Wrap("Read https://canter.dev/llms.txt and help me connect you to Canter. Show me the authorization link and wait for my approval.", w, "")) + "\n\n" +
		p("Sign in on canter.dev to access your workspace.")
}

func money(cents int64) string {
	if cents%100 == 0 {
		return fmt.Sprintf("$%d", cents/100)
	}
	return fmt.Sprintf("$%.2f", float64(cents)/100)
}
