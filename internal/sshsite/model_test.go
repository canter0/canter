package sshsite

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func press(m Model, code rune) Model {
	next, _ := m.Update(tea.KeyPressMsg{Code: code})
	return next.(Model)
}

func TestVisitorJourney(t *testing.T) {
	m := NewModel(100, 35)
	m = press(m, '2')
	if m.page != 2 || !strings.Contains(ansi.Strip(m.View().Content), "Pay as you go") {
		t.Fatal("pricing should be reachable from the homepage")
	}
	m = press(m, tea.KeyEscape)
	m = press(m, '3')
	if m.page != 3 || !strings.Contains(m.View().Content, ansi.SetHyperlink("https://canter.dev/create-account")) {
		t.Fatal("onboarding should link to the real account creation page")
	}
	m = press(m, tea.KeyEscape)
	if m.page != 0 {
		t.Fatal("escape should return home")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q'})
	if cmd == nil {
		t.Fatal("q should exit")
	}
}

func TestTerminalSizes(t *testing.T) {
	for _, size := range [][2]int{{20, 8}, {36, 14}, {40, 18}, {80, 24}, {120, 40}} {
		for page := 0; page <= 3; page++ {
			m := NewModel(size[0], size[1])
			m.page = page
			for _, line := range strings.Split(m.View().Content, "\n") {
				if ansi.StringWidth(line) > size[0] {
					t.Fatalf("%dx%d page %d overflows: %q", size[0], size[1], page, ansi.Strip(line))
				}
			}
			if len(strings.Split(m.View().Content, "\n")) > size[1] {
				t.Fatalf("%dx%d page %d exceeds terminal height", size[0], size[1], page)
			}
		}
	}
}

func TestScrollAndSessionIsolation(t *testing.T) {
	one, two := NewModel(40, 18), NewModel(40, 18)
	one = press(one, '3')
	before := one.View().Content
	for i := 0; i < 10; i++ {
		one = press(one, tea.KeyDown)
	}
	if before == one.View().Content {
		t.Fatal("long content should scroll")
	}
	if two.page != 0 || two.scroll != 0 {
		t.Fatal("visitors must have independent state")
	}
	next, _ := one.Update(tea.WindowSizeMsg{Width: 1 << 30, Height: 1 << 30})
	resized := next.(Model)
	if resized.width > 500 || resized.height > 200 {
		t.Fatal("untrusted resize exceeds layout bounds")
	}
}
