package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/iryzhkov/jocasta/internal/markdown"
	"github.com/iryzhkov/jocasta/internal/store"
	"github.com/iryzhkov/jocasta/pkg/api"
)

const viewBudget = 64 << 10

type viewReference struct {
	Version     string `json:"version"`
	ID          string `json:"id"`
	Path        string `json:"path"`
	Revision    int64  `json:"revision"`
	ContentHash string `json:"content_hash"`
	TotalLines  int    `json:"total_lines"`
}

func reference(r api.Receipt) viewReference {
	return viewReference{api.Version, r.Document.ID, r.Document.Path, r.Document.Revision, r.FullDocumentHash, r.TotalLines}
}

type outlineView struct {
	viewReference
	Headings         []markdown.Heading `json:"headings"`
	TotalHeadings    int                `json:"total_headings"`
	ReturnedHeadings int                `json:"returned_headings"`
	Truncated        bool               `json:"truncated"`
	Complete         bool               `json:"complete"`
	NextLines        string             `json:"next_lines,omitempty"`
}
type sectionView struct {
	viewReference
	Section        markdown.Selector `json:"section"`
	Content        string            `json:"content"`
	StartLine      int               `json:"start_line"`
	EndLine        int               `json:"end_line"`
	StartByte      int               `json:"start_byte"`
	EndByte        int               `json:"end_byte"`
	SectionEndByte int               `json:"section_end_byte"`
	SectionEndLine int               `json:"section_end_line"`
	Truncated      bool              `json:"truncated"`
	Complete       bool              `json:"complete"`
	NextLines      string            `json:"next_lines,omitempty"`
}

func (s *Server) getView(ctx context.Context, a mcpGetArgs) (any, error) {
	modes := 0
	for _, on := range []bool{a.View != "", a.Section != nil, a.Lines != ""} {
		if on {
			modes++
		}
	}
	if modes > 1 || (a.View != "" && a.View != "outline") || (a.View != "outline" && (len(a.Levels) > 0 || a.OutlineLimit != 0)) {
		return nil, store.Invalid("choose lines, section or view:outline; levels/outline_limit require outline")
	}
	revision := int64(0)
	if a.Revision != nil {
		if *a.Revision < 1 {
			return nil, store.Invalid("revision must be positive")
		}
		revision = *a.Revision
	}
	// Authorization precedes locator work, including aliases and old snapshots.
	r, err := s.s.Get(ctx, a.Path, a.ID, revision)
	if err != nil {
		return nil, err
	}
	if a.View == "" && a.Section == nil {
		if a.Lines != "" {
			return store.Range(r, a.Lines)
		}
		return r, nil
	}
	idx, err := markdown.Scan(r.Document.Content)
	if err != nil {
		return nil, store.Invalid(err.Error())
	}
	if a.View == "outline" {
		limit := a.OutlineLimit
		if limit == 0 {
			limit = 200
		}
		if limit < 1 || limit > 200 {
			return nil, store.Invalid("outline_limit must be 1..200")
		}
		levels := map[int]bool{}
		for _, n := range a.Levels {
			if n < 1 || n > 6 {
				return nil, store.Invalid("levels must be 1..6")
			}
			levels[n] = true
		}
		v := outlineView{viewReference: reference(r), Headings: []markdown.Heading{}, Complete: true}
		blocked := false
		for _, h := range idx.Headings {
			if len(levels) > 0 && !levels[h.Level] {
				continue
			}
			v.TotalHeadings++
			if blocked {
				continue
			}
			v.Headings = append(v.Headings, h)
			if len(v.Headings) > limit || jsonSize(v) > viewBudget-1024 {
				v.Headings = v.Headings[:len(v.Headings)-1]
				v.NextLines = fmt.Sprintf("%d:%d", h.Line, idx.TotalLines)
				blocked = true
			}
		}
		v.ReturnedHeadings = len(v.Headings)
		v.Truncated = blocked
		v.Complete = !blocked
		return v, nil
	}
	h, err := idx.Select(*a.Section)
	if err != nil {
		return nil, selectorError(err)
	}
	v := sectionView{viewReference: reference(r), Section: markdown.Selector{Heading: h.Text, HeadingLine: h.Line}, StartLine: h.Line, StartByte: h.Start, SectionEndByte: h.End, SectionEndLine: h.EndLine, Complete: true}
	// Heading text itself may be a large source line. Refuse oversized metadata.
	if jsonSize(v) > viewBudget/2 {
		return nil, store.Invalid("section selector exceeds view budget; use pinned line reads")
	}
	v.Content = bytePrefix(r.Document.Content[h.Start:h.End], (viewBudget-jsonSize(v)-1024)/6)
	v.EndByte = h.Start + len(v.Content)
	v.EndLine = lastLine(r.Document.Content, h.Start, v.EndByte)
	v.Truncated = v.EndByte < h.End
	v.Complete = !v.Truncated
	if v.Truncated {
		v.NextLines = fmt.Sprintf("%d:%d", v.EndLine, h.EndLine)
	}
	return v, nil
}
func selectorError(err error) error {
	var se *markdown.SelectionError
	if !errors.As(err, &se) {
		return store.Invalid(err.Error())
	}
	lines := []int{}
	for _, h := range se.Candidates {
		lines = append(lines, h.Line)
	}
	b, _ := json.Marshal(lines)
	return &api.Error{Code: se.Code, Message: "exact section refused; candidate heading_line values: " + string(b)}
}
func jsonSize(v any) int { b, _ := json.Marshal(v); return len(b) }
func bytePrefix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
func lastLine(body string, start, end int) int {
	n := 1 + strings.Count(body[:start], "\n")
	if end > start {
		n += strings.Count(body[start:end], "\n")
		if body[end-1] == '\n' {
			n--
		}
	}
	return n
}

type searchExcerpt struct {
	ID          string            `json:"id"`
	Revision    int64             `json:"revision"`
	ContentHash string            `json:"content_hash"`
	Content     string            `json:"content"`
	StartLine   int               `json:"start_line,omitempty"`
	EndLine     int               `json:"end_line,omitempty"`
	StartByte   int               `json:"start_byte"`
	EndByte     int               `json:"end_byte"`
	TotalBytes  int               `json:"total_bytes"`
	Heading     *markdown.Heading `json:"nearest_heading,omitempty"`
	Truncated   bool              `json:"truncated"`
	Complete    bool              `json:"complete"`
	Reason      string            `json:"reason,omitempty"`
	NextLines   string            `json:"next_lines,omitempty"`
}
type enrichedPage struct {
	api.Page
	Excerpts []searchExcerpt `json:"excerpts"`
}

func (s *Server) searchView(ctx context.Context, a mcpPageArgs) (any, error) {
	if !a.IncludeExcerpts {
		if a.ExcerptBytes != 0 {
			return nil, store.Invalid("excerpt_bytes requires include_excerpts")
		}
		return s.s.Page(ctx, a.filter(), true)
	}
	if a.Limit == nil {
		n := 20
		a.Limit = &n
	}
	if *a.Limit < 1 || *a.Limit > 20 {
		return nil, store.Invalid("enriched search limit must be 1..20")
	}
	size := a.ExcerptBytes
	if size == 0 {
		size = 2048
	}
	if size < 1 || size > 2048 {
		return nil, store.Invalid("excerpt_bytes must be 1..2048")
	}
	p, err := s.s.Page(ctx, a.filter(), true)
	if err != nil {
		return nil, err
	}
	out := enrichedPage{Page: p, Excerpts: []searchExcerpt{}}
	for _, d := range p.Documents {
		// Fetch the pinned revision through the same ACL, never current bytes.
		r, e := s.s.Get(ctx, "", d.ID, d.Revision)
		if e != nil {
			return nil, e
		}
		if r.FullDocumentHash != d.ContentHash {
			return nil, &api.Error{Code: "cursor_expired", Message: "search snapshot changed; restart search"}
		}
		body := r.Document.Content
		ex := searchExcerpt{ID: d.ID, Revision: d.Revision, ContentHash: d.ContentHash, TotalBytes: len(body), Complete: true}
		at := -1
		for _, term := range strings.Fields(a.Query) {
			if pos := strings.Index(body, term); pos >= 0 && (at < 0 || pos < at) {
				at = pos
			}
		}
		if at < 0 {
			ex.Reason = "no_exact_literal_match"
		} else {
			// Start on the matching logical line, or the exact match when that line
			// is too long. Never reconstruct offsets from case-folded Unicode.
			start := strings.LastIndexByte(body[:at], '\n') + 1
			if at-start >= size {
				start = at
			}
			ex.Content = bytePrefix(body[start:], size)
			ex.StartByte = start
			ex.EndByte = start + len(ex.Content)
			ex.StartLine = 1 + strings.Count(body[:start], "\n")
			ex.EndLine = lastLine(body, start, ex.EndByte)
			idx, e := markdown.Scan(body)
			if e != nil {
				return nil, store.Invalid(e.Error())
			}
			for _, h := range idx.Headings {
				if h.Start > at {
					break
				}
				copy := h
				ex.Heading = &copy
			}
			if ex.Heading != nil && len(ex.Heading.Text) > 2048 {
				ex.Heading = nil
			}
			ex.Truncated = start > 0 || ex.EndByte < len(body)
			ex.Complete = !ex.Truncated
			if ex.Truncated {
				ex.NextLines = fmt.Sprintf("%d:%d", ex.EndLine, r.TotalLines)
			}
		}
		out.Excerpts = append(out.Excerpts, ex)
	}
	if jsonSize(out) > viewBudget {
		return nil, store.Invalid("enriched results exceed 64 KiB; reduce limit or use pinned line reads")
	}
	return out, nil
}
