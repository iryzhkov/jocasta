package cli

import (
	"fmt"
	"strings"
)

// maxDiffEdits bounds the depth of the diff search. The algorithm keeps one
// integer per (edit, diagonal) pair it explores, so this bound also caps the
// working set at roughly maxDiffEdits squared integers. Two revisions that
// need more edits than this share so little that a replacement of the region
// they differ in is the honest answer, and the caller says the diff was bounded
// rather than pretending the result is minimal.
const maxDiffEdits = 1400

// diffContext is the number of unchanged lines kept on each side of a change.
// Two changes closer together than twice this share one hunk.
const diffContext = 3

// diffLine is one line of the edit script: unchanged, removed or added, with
// its 1-based line numbers in the from and to revisions. A line keeps its own
// terminator, so the marker for a missing final newline is decided per line.
type diffLine struct {
	op   byte
	text string
	a, b int
}

// script computes the edit script that turns a into b. Identical lines at the
// start and end are matched directly, which is both correct and the reason a
// one-line change in a large document costs almost nothing: only the region
// that actually differs reaches the search. The second result reports that the
// search hit its bound and the differing region was replaced wholesale.
func script(a, b []string) ([]diffLine, bool) {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	middle, bounded := myers(a[prefix:len(a)-suffix], b[prefix:len(b)-suffix])
	out := make([]diffLine, 0, prefix+len(middle)+suffix)
	for i := 0; i < prefix; i++ {
		out = append(out, diffLine{op: ' ', text: a[i]})
	}
	out = append(out, middle...)
	for i := len(a) - suffix; i < len(a); i++ {
		out = append(out, diffLine{op: ' ', text: a[i]})
	}
	ai, bi := 1, 1
	for i := range out {
		switch out[i].op {
		case ' ':
			out[i].a, out[i].b = ai, bi
			ai++
			bi++
		case '-':
			out[i].a = ai
			ai++
		case '+':
			out[i].b = bi
			bi++
		}
	}
	return out, bounded
}

// myers finds a shortest edit script by Myers' greedy algorithm: it advances a
// furthest-reaching path on each diagonal for an increasing number of edits,
// and stops at the first number of edits that reaches the end of both inputs.
// The work is proportional to the size of the inputs times that number, so it
// is cheap exactly when the two revisions are close.
func myers(a, b []string) ([]diffLine, bool) {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil, false
	}
	bound := n + m
	if bound > maxDiffEdits {
		bound = maxDiffEdits
	}
	offset := n + m
	v := make([]int, 2*(n+m)+1)
	trace := make([][]int, 0, bound+1)
	for d := 0; d <= bound; d++ {
		// Keep only the window this round can reach, so the trace costs one
		// integer per diagonal explored rather than a full copy per round.
		snapshot := make([]int, 2*d+1)
		copy(snapshot, v[offset-d:offset+d+1])
		trace = append(trace, snapshot)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return backtrack(a, b, trace, d, k, x, y), false
			}
		}
	}
	return replacement(a, b), true
}

// backtrack walks the recorded rounds in reverse, turning the path that reached
// the end into the script that produced it.
func backtrack(a, b []string, trace [][]int, d, k, x, y int) []diffLine {
	rev := make([]diffLine, 0, d*2)
	for ; d > 0; d-- {
		v := trace[d]
		prevK := k - 1
		if k == -d || (k != d && v[k-1+d] < v[k+1+d]) {
			prevK = k + 1
		}
		prevX := v[prevK+d]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			rev = append(rev, diffLine{op: ' ', text: a[x]})
		}
		if prevK == k+1 {
			y--
			rev = append(rev, diffLine{op: '+', text: b[y]})
		} else {
			x--
			rev = append(rev, diffLine{op: '-', text: a[x]})
		}
		x, y, k = prevX, prevY, prevK
	}
	for x > 0 && y > 0 {
		x--
		y--
		rev = append(rev, diffLine{op: ' ', text: a[x]})
	}
	out := make([]diffLine, len(rev))
	for i, line := range rev {
		out[len(rev)-1-i] = line
	}
	return out
}

// replacement is the answer when the search hits its bound: remove the region
// that differs and add the new one.
func replacement(a, b []string) []diffLine {
	out := make([]diffLine, 0, len(a)+len(b))
	for _, line := range a {
		out = append(out, diffLine{op: '-', text: line})
	}
	for _, line := range b {
		out = append(out, diffLine{op: '+', text: line})
	}
	return out
}

// unified renders the script as a unified diff: a header naming both revisions,
// then one hunk per changed region with its surrounding context. It returns the
// text, the number of hunks, and whether the search was bounded. Two revisions
// that do not differ produce no text and no hunks.
func unified(fromPath string, fromRevision int64, toPath string, toRevision int64, a, b []string) (string, int, bool) {
	lines, bounded := script(a, b)
	changed := false
	for _, line := range lines {
		if line.op != ' ' {
			changed = true
			break
		}
	}
	if !changed {
		return "", 0, bounded
	}
	var s strings.Builder
	fmt.Fprintf(&s, "--- %s@%d\n+++ %s@%d\n", fromPath, fromRevision, toPath, toRevision)
	hunks := 0
	for i := 0; i < len(lines); {
		if lines[i].op == ' ' {
			i++
			continue
		}
		first, last, j := i, i, i
		for j < len(lines) {
			if lines[j].op != ' ' {
				last = j
				j++
				continue
			}
			// Absorb a run of unchanged lines only when another change follows
			// close enough that two hunks would repeat the same context.
			k := j
			for k < len(lines) && lines[k].op == ' ' {
				k++
			}
			if k < len(lines) && k-j <= 2*diffContext {
				j = k
				continue
			}
			break
		}
		start := first - diffContext
		if start < 0 {
			start = 0
		}
		stop := last + 1 + diffContext
		if stop > len(lines) {
			stop = len(lines)
		}
		writeHunk(&s, lines[start:stop])
		hunks++
		i = last + 1
	}
	return s.String(), hunks, bounded
}
func writeHunk(s *strings.Builder, lines []diffLine) {
	aStart, bStart, aLen, bLen := 0, 0, 0, 0
	for _, line := range lines {
		if line.op != '+' {
			if aLen == 0 {
				aStart = line.a
			}
			aLen++
		}
		if line.op != '-' {
			if bLen == 0 {
				bStart = line.b
			}
			bLen++
		}
	}
	fmt.Fprintf(s, "@@ -%d,%d +%d,%d @@\n", aStart, aLen, bStart, bLen)
	for _, line := range lines {
		s.WriteByte(line.op)
		s.WriteString(line.text)
		if !strings.HasSuffix(line.text, "\n") {
			s.WriteString("\n\\ No newline at end of file\n")
		}
	}
}
