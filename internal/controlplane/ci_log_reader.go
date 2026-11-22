package controlplane

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	ciLogRangeLines  = 32
	ciLogLineBytes   = 2048
	ciLogSearchBytes = 256
)

type ciLogLine struct {
	Offset int    `json:"offset"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

type ciLogPage struct {
	Lines       []ciLogLine `json:"lines"`
	Previous    *int        `json:"previous,omitempty"`
	Next        *int        `json:"next,omitempty"`
	Matches     int         `json:"matches"`
	Match       *int        `json:"match,omitempty"`
	MatchOffset *int        `json:"matchOffset,omitempty"`
	Truncated   bool        `json:"truncated"`
}

type ciLogRun struct {
	stream string
	text   string
	offset int
}

func (s *Store) SetCILogReaderEnabled(enabled bool) {
	s.ciLogReaderEnabled.Store(enabled)
}

func (s *Store) canReadCILogRanges(workspace string) bool {
	return s.canaryFeatureEnabled(workspace, s.ciLogReaderEnabled.Load())
}

func cleanCILog(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return '?'
		}
		return r
	}, text)
}

// Consecutive chunks of the same stream form one searchable run. Stream
// changes preserve the recorded order without fabricating cross-stream lines.
func (s *Store) ciLogRuns(ctx context.Context, workspace, job string) ([]ciLogRun, bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT stream,content FROM ci_job_logs
		WHERE workspace_id=$1 AND job_id=$2 AND content<>'' ORDER BY sequence`, workspace, job)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []ciLogRun
	var pending strings.Builder
	var stream string
	total := 0
	truncated := false
	flush := func() {
		if pending.Len() > 0 {
			out = append(out, ciLogRun{stream: stream, offset: total, text: pending.String()})
			total += pending.Len()
			pending.Reset()
		}
	}
	for rows.Next() {
		var kind, text string
		if err := rows.Scan(&kind, &text); err != nil {
			return nil, false, err
		}
		if kind != stream {
			flush()
			stream = kind
		}
		text = cleanCILog(text)
		remaining := ciJobLogMaxBytes - total - pending.Len()
		if len(text) > remaining {
			text = text[:remaining]
			for !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
			truncated = true
		}
		pending.WriteString(text)
		if truncated {
			break
		}
	}
	flush()
	return out, truncated, rows.Err()
}

func pageCILogs(runs []ciLogRun, offset int, query string, requestedMatch int) ciLogPage {
	result := ciLogPage{Lines: []ciLogLine{}}
	matchByte := -1
	if query != "" {
		for _, run := range runs {
			for start := 0; start < len(run.text); {
				relative := strings.Index(run.text[start:], query)
				if relative < 0 {
					break
				}
				found := start + relative
				if result.Matches == requestedMatch {
					matchByte = run.offset + found
				}
				result.Matches++
				start = found + len(query)
			}
		}
	}
	var previous [ciLogRangeLines]int
	skipped := 0
	for _, run := range runs {
		for text, cursor := run.text, run.offset; text != ""; {
			end := min(len(text), ciLogLineBytes)
			if newline := strings.IndexByte(text[:end], '\n'); newline >= 0 {
				end = newline + 1
			} else {
				for end < len(text) && !utf8.RuneStart(text[end]) {
					end--
				}
			}
			// If search found a match, start on the line containing it or the
			// following lines. Otherwise retain byte-offset paging semantics.
			if (matchByte >= 0 && cursor+end <= matchByte) || (matchByte < 0 && cursor < offset) {
				previous[skipped%ciLogRangeLines] = cursor
				skipped++
			} else {
				if len(result.Lines) == ciLogRangeLines {
					next := cursor
					result.Next = &next
					return result
				}
				if len(result.Lines) == 0 {
					result.Lines = make([]ciLogLine, 0, ciLogRangeLines)
					if matchByte >= 0 {
						matchOffset := cursor
						result.Match, result.MatchOffset = &requestedMatch, &matchOffset
					}
					if skipped > 0 {
						prev := previous[max(0, skipped-ciLogRangeLines)%ciLogRangeLines]
						result.Previous = &prev
					}
				}
				result.Lines = append(result.Lines, ciLogLine{Offset: cursor, Stream: run.stream, Text: text[:end]})
			}
			text, cursor = text[end:], cursor+end
		}
	}
	if len(result.Lines) == 0 && skipped > 0 {
		prev := previous[max(0, skipped-ciLogRangeLines)%ciLogRangeLines]
		result.Previous = &prev
	}
	return result
}

func (h *HTTPServer) ciJobLogPage(w http.ResponseWriter, r *http.Request, p Principal, workspace, job string) {
	ctx := r.Context()
	row, _, err := h.service.Store.CIJobRow(ctx, workspace, job)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	visible, err := h.ciJobVisible(ctx, p, workspace, row)
	if errors.Is(err, errCIRepoUnavailable) {
		writeError(w, http.StatusServiceUnavailable, errors.New("Repository access could not be verified."))
		return
	}
	if err != nil || !visible {
		if err == nil {
			err = ErrNotFound
		}
		writeStoreError(w, err)
		return
	}
	params := r.URL.Query()
	readNumber := func(key string) (int, bool) {
		value := params.Get(key)
		if value == "" {
			return 0, true
		}
		number, err := strconv.Atoi(value)
		return number, err == nil && number >= 0 && number <= ciJobLogMaxBytes
	}
	offset, offsetOK := readNumber("offset")
	match, matchOK := readNumber("match")
	query := params.Get("q")
	if !offsetOK || !matchOK || len(query) > ciLogSearchBytes || !utf8.ValidString(query) {
		writeError(w, http.StatusBadRequest, errors.New("Invalid log range or search."))
		return
	}
	runs, truncated, err := h.service.Store.ciLogRuns(ctx, workspace, job)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	page := pageCILogs(runs, offset, query, match)
	page.Truncated = truncated
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, page)
}
