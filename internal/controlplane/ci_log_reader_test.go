package controlplane

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestPageCILogs exercises the streaming pager without a database: byte-offset
// paging cursors, search anchoring, run boundaries, and UTF-8 line splits.
func TestPageCILogs(t *testing.T) {
	t.Run("offset paging reports previous and next cursors", func(t *testing.T) {
		runs := []ciLogRun{{stream: "stdout", text: strings.Repeat("x\n", 100)}}
		page := pageCILogs(runs, 81, "", 0)
		if len(page.Lines) != ciLogRangeLines || page.Lines[0].Offset != 82 {
			t.Fatalf("lines: %+v", page.Lines)
		}
		if page.Previous == nil || *page.Previous != 18 {
			t.Fatalf("previous: %+v", page.Previous)
		}
		if page.Next == nil || *page.Next != 146 {
			t.Fatalf("next: %+v", page.Next)
		}
	})

	t.Run("offset past end returns an empty page with a previous cursor", func(t *testing.T) {
		runs := []ciLogRun{{stream: "stdout", text: strings.Repeat("x\n", 100)}}
		page := pageCILogs(runs, 1000, "", 0)
		if len(page.Lines) != 0 {
			t.Fatalf("lines: %+v", page.Lines)
		}
		if page.Previous == nil || *page.Previous != 136 {
			t.Fatalf("previous: %+v", page.Previous)
		}
		if page.Next != nil {
			t.Fatalf("next: %+v", *page.Next)
		}
	})

	t.Run("a match anchors the page on the line containing its first byte", func(t *testing.T) {
		runs := []ciLogRun{{stream: "stdout", text: "zz\naa cross\nchunk bb\ncc\n"}}
		page := pageCILogs(runs, 0, "cross\nchunk", 0)
		if page.Matches != 1 || page.Match == nil || *page.Match != 0 {
			t.Fatalf("match: %+v", page)
		}
		if page.MatchOffset == nil || *page.MatchOffset != 3 {
			t.Fatalf("match offset: %+v", page.MatchOffset)
		}
		if len(page.Lines) != 3 || page.Lines[0].Text != "aa cross\n" || page.Lines[0].Offset != 3 {
			t.Fatalf("lines: %+v", page.Lines)
		}
		if page.Previous == nil || *page.Previous != 0 {
			t.Fatalf("previous: %+v", page.Previous)
		}
		if page.Next != nil {
			t.Fatalf("next: %+v", *page.Next)
		}
	})

	t.Run("matches never span separate runs", func(t *testing.T) {
		runs := []ciLogRun{
			{stream: "stdout", text: "xx cros", offset: 0},
			{stream: "stderr", text: "schunk yy\n", offset: 7},
		}
		page := pageCILogs(runs, 0, "crosschunk", 0)
		if page.Matches != 0 || page.Match != nil || page.MatchOffset != nil {
			t.Fatalf("matches across runs: %+v", page)
		}
		if len(page.Lines) != 2 {
			t.Fatalf("lines: %+v", page.Lines)
		}
		same := pageCILogs([]ciLogRun{{stream: "stdout", text: "xx cross" + "chunk yy\n"}}, 0, "crosschunk", 0)
		if same.Matches != 1 || same.MatchOffset == nil || *same.MatchOffset != 0 {
			t.Fatalf("same-run match: %+v", same)
		}
	})

	t.Run("an out-of-range match falls back to the supplied offset", func(t *testing.T) {
		runs := []ciLogRun{{stream: "stdout", text: "needle\n" + strings.Repeat("x\n", 40)}}
		page := pageCILogs(runs, 19, "needle", 5)
		if page.Matches != 1 || page.Match != nil || page.MatchOffset != nil {
			t.Fatalf("match state: %+v", page)
		}
		if len(page.Lines) != ciLogRangeLines || page.Lines[0].Offset != 19 {
			t.Fatalf("lines: %+v", page.Lines)
		}
		if page.Previous == nil || *page.Previous != 0 {
			t.Fatalf("previous: %+v", page.Previous)
		}
		if page.Next == nil || *page.Next != 83 {
			t.Fatalf("next: %+v", page.Next)
		}
	})

	t.Run("a line split backs off to a UTF-8 boundary", func(t *testing.T) {
		text := strings.Repeat("a", ciLogLineBytes-1) + "é" + "tail\n"
		page := pageCILogs([]ciLogRun{{stream: "stdout", text: text}}, 0, "", 0)
		if len(page.Lines) != 2 {
			t.Fatalf("lines: %+v", page.Lines)
		}
		if len(page.Lines[0].Text) != ciLogLineBytes-1 || page.Lines[1].Offset != ciLogLineBytes-1 {
			t.Fatalf("split: %+v", page.Lines)
		}
		if !strings.HasPrefix(page.Lines[1].Text, "é") ||
			!utf8.ValidString(page.Lines[0].Text) || !utf8.ValidString(page.Lines[1].Text) {
			t.Fatalf("utf8 boundary: %q", page.Lines[1].Text)
		}
	})
}
