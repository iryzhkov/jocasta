// Package markdown locates a deliberately bounded top-level Markdown grammar.
// It preserves byte offsets and never renders, normalizes or rewrites source.
package markdown

import (
	"fmt"
	"strings"
)

const MaxHeadings = 10000

type Selector struct {
	Heading     string `json:"heading"`
	HeadingLine int    `json:"heading_line,omitempty"`
}
type Heading struct {
	Text    string `json:"heading"`
	Level   int    `json:"level"`
	Line    int    `json:"heading_line"`
	EndLine int    `json:"end_line"`
	Start   int    `json:"-"`
	End     int    `json:"-"`
}
type Index struct {
	Headings   []Heading
	TotalLines int
}
type SelectionError struct {
	Code       string
	Candidates []Heading
}

func (e *SelectionError) Error() string { return e.Code + ": exact section selector refused" }

// Scan recognizes ATX, eligible single-line Setext and backtick/tilde fences.
// Containers, four-space/tab indents and HTML/rendered headings are excluded.
// At most MaxHeadings are indexed; excess refuses rather than guessing a section.
func Scan(body string) (Index, error) {
	var idx Index
	fence := byte(0)
	fenceLen := 0
	previous := ""
	previousStart, previousLine := 0, 0
	listIndent := -1
	for pos, line := 0, 1; pos < len(body); line++ {
		end := strings.IndexByte(body[pos:], '\n')
		next := len(body)
		if end >= 0 {
			next = pos + end + 1
		}
		text := strings.TrimSuffix(strings.TrimSuffix(body[pos:next], "\n"), "\r")
		idx.TotalLines = line
		indent := 0
		for indent < len(text) && text[indent] == ' ' {
			indent++
		}
		top := indent <= 3 && (indent == len(text) || text[indent] != '\t')
		t := ""
		if top {
			t = text[indent:]
		}
		if fence == 0 {
			if listIndent >= 0 {
				if strings.TrimSpace(text) == "" || indent > listIndent {
					previous = ""
					pos = next
					continue
				}
				listIndent = -1
			}
			if top && listMarker(t) && !(previous != "" && setext(t) != 0) {
				listIndent = indent
				previous = ""
				pos = next
				continue
			}
		}
		if fence != 0 {
			n := run(t, fence)
			if n >= fenceLen && strings.TrimSpace(t[n:]) == "" {
				fence = 0
			}
			previous = ""
			pos = next
			continue
		}
		if top && len(t) > 0 && (t[0] == '`' || t[0] == '~') {
			n := run(t, t[0])
			if n >= 3 && (t[0] != '`' || !strings.ContainsRune(t[n:], '`')) {
				fence = t[0]
				fenceLen = n
				previous = ""
				pos = next
				continue
			}
		}
		level := 0
		label := ""
		start, headingLine := pos, line
		if top {
			n := run(t, '#')
			if n >= 1 && n <= 6 && (n == len(t) || t[n] == ' ' || t[n] == '\t') {
				level = n
				label = strings.TrimSpace(t[n:])
				// Optional closing hashes require preceding whitespace.
				cut := len(label)
				for cut > 0 && label[cut-1] == '#' {
					cut--
				}
				if cut < len(label) && (cut == 0 || label[cut-1] == ' ' || label[cut-1] == '\t') {
					label = strings.TrimSpace(label[:cut])
				}
			} else if previous != "" && setext(t) != 0 {
				level = setext(t)
				label = strings.TrimSpace(previous)
				start = previousStart
				headingLine = previousLine
			}
		}
		if level > 0 {
			if len(idx.Headings) >= MaxHeadings {
				return Index{}, fmt.Errorf("markdown heading limit exceeded")
			}
			idx.Headings = append(idx.Headings, Heading{Text: label, Level: level, Line: headingLine, Start: start})
			previous = ""
		} else if top && eligible(t) {
			previous = t
			previousStart = pos
			previousLine = line
		} else {
			previous = ""
		}
		pos = next
	}
	nextHeading := [7]int{}
	for i := len(idx.Headings) - 1; i >= 0; i-- {
		h := &idx.Headings[i]
		h.End = len(body)
		h.EndLine = idx.TotalLines
		for level := 1; level <= h.Level; level++ {
			if n := nextHeading[level]; n > 0 && idx.Headings[n-1].Start < h.End {
				h.End = idx.Headings[n-1].Start
				h.EndLine = idx.Headings[n-1].Line - 1
			}
		}
		nextHeading[h.Level] = i + 1
	}
	return idx, nil
}
func run(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}
func setext(s string) int {
	t := strings.TrimRight(s, " \t")
	if t == "" {
		return 0
	}
	if run(t, '=') == len(t) {
		return 1
	}
	if run(t, '-') == len(t) {
		return 2
	}
	return 0
}
func listMarker(s string) bool {
	if s == "-" || s == "+" || s == "*" {
		return true
	}
	if s == "" {
		return false
	}
	for _, p := range []string{"- ", "+ ", "* ", "-\t", "+\t", "*\t"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n > 0 && n < len(s) && (s[n] == '.' || s[n] == ')') && (n+1 == len(s) || s[n+1] == ' ' || s[n+1] == '\t')
}
func eligible(s string) bool {
	if strings.TrimSpace(s) == "" || s[0] == '>' || s[0] == '#' || s[0] == '<' || s[0] == '\t' || setext(s) != 0 {
		return false
	}
	for _, p := range []string{"- ", "+ ", "* "} {
		if strings.HasPrefix(s, p) {
			return false
		}
	}
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	if n > 0 && n < len(s) && (s[n] == '.' || s[n] == ')') && n+1 < len(s) && (s[n+1] == ' ' || s[n+1] == '\t') {
		return false
	}
	return true
}
func (idx Index) Select(s Selector) (Heading, error) {
	if s.HeadingLine < 0 {
		return Heading{}, &SelectionError{Code: "invalid_section"}
	}
	matches := []Heading{}
	count := 0
	for _, h := range idx.Headings {
		if h.Text == s.Heading && (s.HeadingLine == 0 || h.Line == s.HeadingLine) {
			count++
			if len(matches) < 20 {
				matches = append(matches, h)
			}
		}
	}
	if count == 0 {
		return Heading{}, &SelectionError{Code: "section_not_found"}
	}
	if count > 1 {
		return Heading{}, &SelectionError{Code: "ambiguous_section", Candidates: matches}
	}
	return matches[0], nil
}
