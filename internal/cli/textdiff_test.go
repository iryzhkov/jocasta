package cli

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// apply replays an edit script against the from revision. A script that does
// not reproduce the to revision exactly is wrong however plausible it looks,
// and this is the invariant an agent reconciling a conflict depends on.
func apply(t *testing.T, lines []diffLine) (string, string) {
	t.Helper()
	var from, to strings.Builder
	for _, line := range lines {
		if line.op != '+' {
			from.WriteString(line.text)
		}
		if line.op != '-' {
			to.WriteString(line.text)
		}
	}
	return from.String(), to.String()
}

func TestScriptReproducesBothRevisions(t *testing.T) {
	cases := [][2]string{
		{"", ""},
		{"", "a\nb\n"},
		{"a\nb\n", ""},
		{"a\n", "a\n"},
		{"a\nb\nc\n", "a\nc\n"},
		{"a\nb\nc\n", "a\nx\nb\nc\n"},
		{"a\nb\nc", "a\nb\nc changed"},
		{"one\ntwo\nthree\n", "three\ntwo\none\n"},
		{strings.Repeat("same\n", 50), strings.Repeat("same\n", 50) + "tail\n"},
		{strings.Repeat("old\n", 40), strings.Repeat("new\n", 40)},
	}
	random := rand.New(rand.NewSource(7))
	for i := 0; i < 40; i++ {
		var a, b strings.Builder
		for n := 0; n < 60; n++ {
			line := fmt.Sprintf("line %d\n", n)
			if random.Intn(4) != 0 {
				a.WriteString(line)
			}
			if random.Intn(4) != 0 {
				b.WriteString(line)
			}
			if random.Intn(8) == 0 {
				b.WriteString(fmt.Sprintf("inserted %d\n", n))
			}
		}
		cases = append(cases, [2]string{a.String(), b.String()})
	}
	for _, c := range cases {
		lines, bounded := script(splitLines(c[0]), splitLines(c[1]))
		from, to := apply(t, lines)
		if from != c[0] || to != c[1] {
			t.Fatalf("script does not reproduce its inputs\nfrom %q -> %q\nto   %q -> %q", c[0], from, c[1], to)
		}
		if bounded {
			t.Fatalf("small inputs should not reach the bound: %q %q", c[0], c[1])
		}
		for i, line := range lines {
			if line.op != ' ' && line.op != '-' && line.op != '+' {
				t.Fatalf("line %d has operation %q", i, line.op)
			}
		}
	}
}

// The unchanged parts of a document are matched directly rather than searched,
// so a one-line change in a long document costs one edit.
func TestScriptCostFollowsTheChangeNotTheDocument(t *testing.T) {
	var a strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&a, "line %d\n", i)
	}
	b := strings.Replace(a.String(), "line 10000\n", "line 10000 reconciled\n", 1)
	lines, bounded := script(splitLines(a.String()), splitLines(b))
	if bounded {
		t.Fatal("a one-line change reached the bound")
	}
	changes := 0
	for _, line := range lines {
		if line.op != ' ' {
			changes++
		}
	}
	if changes != 2 {
		t.Fatalf("one replaced line produced %d changed lines", changes)
	}
	text, hunks, _ := unified("p/a.md", 1, "p/a.md", 2, splitLines(a.String()), splitLines(b))
	if hunks != 1 || len(text) > 400 {
		t.Fatalf("%d hunks, %d bytes for a one-line change in %d bytes", hunks, len(text), a.Len())
	}
}

// Two revisions that share almost nothing exceed the search bound. The answer
// is then a replacement of the region that differs, still correct, and the
// caller is told the diff is not minimal rather than left to assume it is.
func TestBoundedDiffIsReportedAndStillCorrect(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 1200; i++ {
		fmt.Fprintf(&a, "old line %d\n", i)
		fmt.Fprintf(&b, "new line %d\n", i)
	}
	lines, bounded := script(splitLines(a.String()), splitLines(b.String()))
	if !bounded {
		t.Fatal("two wholly different revisions did not reach the bound")
	}
	from, to := apply(t, lines)
	if from != a.String() || to != b.String() {
		t.Fatal("the bounded script does not reproduce its inputs")
	}
	text, hunks, bounded := unified("p/a.md", 1, "p/a.md", 2, splitLines(a.String()), splitLines(b.String()))
	if !bounded || hunks != 1 || !strings.Contains(text, "-old line 0\n") || !strings.Contains(text, "+new line 0\n") {
		t.Fatalf("bounded=%t hunks=%d", bounded, hunks)
	}
}
