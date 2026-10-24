package markdown

import (
	"errors"
	"strings"
	"testing"
)

func TestLocatorExactRangesAndGrammar(t *testing.T) {
	body := "# Top ###\r\nintro π\r\n## Same\r\nold\r\n~~~go\r\n# fenced\r\n~~~~\r\n## Same\nother\nSet *exact*\n===\n    # indented\n> # quote\n- item\n  ## nested list\n\n#Tail\n####### too deep\n\t# tab\n# End\nno final newline"
	idx, e := Scan(body)
	if e != nil {
		t.Fatal(e)
	}
	names := []string{}
	for _, h := range idx.Headings {
		names = append(names, h.Text)
	}
	if strings.Join(names, "|") != "Top|Same|Same|Set *exact*|End" {
		t.Fatalf("%q", names)
	}
	h, e := idx.Select(Selector{Heading: "Same", HeadingLine: 3})
	if e != nil {
		t.Fatal(e)
	}
	if body[h.Start:h.End] != "## Same\r\nold\r\n~~~go\r\n# fenced\r\n~~~~\r\n" || h.EndLine != 7 {
		t.Fatalf("%+v %q", h, body[h.Start:h.End])
	}
	_, e = idx.Select(Selector{Heading: "Same"})
	var se *SelectionError
	if !errors.As(e, &se) || se.Code != "ambiguous_section" || len(se.Candidates) != 2 {
		t.Fatal(e)
	}
	for _, s := range []Selector{{Heading: "same"}, {Heading: "Same", HeadingLine: 4}, {Heading: "Set exact"}, {Heading: "Same", HeadingLine: -1}} {
		if _, e = idx.Select(s); e == nil {
			t.Fatalf("accepted %+v", s)
		}
	}
	h, e = idx.Select(Selector{Heading: "End"})
	if e != nil || body[h.Start:h.End] != "# End\nno final newline" {
		t.Fatal(h, e)
	}
}
func TestLocatorTabbedListContinuation(t *testing.T) {
	for _, body := range []string{
		"- item\n\tcontinuation\n  # Nested\n# Real",
		"- item\r\n\tcontinuation\r\n  # Nested\r\n# Real",
		"  - item\n\tcontinuation\n   # Nested\n# Real",
		"- item\n \tcontinuation\n  # Nested\n# Real",
		"- item\n\tcontinuation\n  Nested\n  ===\n# Real",
	} {
		idx, err := Scan(body)
		if err != nil {
			t.Fatal(err)
		}
		if len(idx.Headings) != 1 || idx.Headings[0].Text != "Real" || idx.Headings[0].Start != strings.Index(body, "# Real") {
			t.Errorf("tabbed list leaked a heading: %q: %+v", body, idx.Headings)
		}
		if _, err = idx.Select(Selector{Heading: "Nested"}); err == nil {
			t.Error("list continuation became selectable")
		}
	}
}
func TestLocatorFencesSetextAndBounds(t *testing.T) {
	cases := []struct {
		body  string
		names string
	}{
		{"Title\r\n---\r\nbody", "Title"},
		{"Title\n- \t\nbody", "Title"},
		{"-\n  # no\n\n# yes", "yes"},
		{"*\n  # no\n\n# yes", "yes"},
		{"1.\n   # no\n\n# yes", "yes"},
		{"\n---\n# Yes\n", "Yes"},
		{"~~~\n# no\n~~~~~\n# yes", "yes"},
		{"`" + "`" + "`" + "info\n# no\n~~turned\n# still no", ""},
		{"### no closing###\n## yes ###\n", "no closing###|yes"},
		{"#\n###### six\n   # three\n    # four\n", "|six|three"},
		{"- item\n  Title\n  ===\n\n# yes", "yes"},
	}
	for _, tt := range cases {
		idx, e := Scan(tt.body)
		if e != nil {
			t.Fatal(e)
		}
		names := []string{}
		for _, h := range idx.Headings {
			names = append(names, h.Text)
		}
		if strings.Join(names, "|") != tt.names {
			t.Fatalf("%q: %q", tt.body, names)
		}
	}
	if _, e := Scan(strings.Repeat("# x\n", MaxHeadings+1)); e == nil {
		t.Fatal("unbounded heading index")
	}
}
