// Package diffutil is a small pure-Go line diff (LCS-based) producing
// unified-style hunks. It exists so the engine can render schema diffs
// without shelling out to diff(1) or adding a dependency.
package diffutil

import (
	"fmt"
	"strings"
)

// contextLines is the number of unchanged lines shown around each change.
const contextLines = 3

type op struct {
	kind byte // ' ' equal, '-' only in a, '+' only in b
	line string
}

// Unified returns a unified-style line diff of a and b ("@@ -i,n +j,m @@"
// hunks with contextLines of context, no file headers), or "" when the inputs
// are line-equal. Trailing-newline-only differences are ignored.
func Unified(a, b string) string {
	ops := diffOps(splitLines(a), splitLines(b))
	changed := false
	for _, o := range ops {
		if o.kind != ' ' {
			changed = true
			break
		}
	}
	if !changed {
		return ""
	}
	return renderHunks(ops)
}

// splitLines splits on newlines, dropping the empty tail a trailing newline
// produces (so "a\n" is the single line "a").
func splitLines(s string) []string {
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// diffOps computes the line-level edit script. Common prefix and suffix are
// trimmed first so the quadratic LCS table only covers the changed middle —
// schema dumps differ in a handful of places, not everywhere.
func diffOps(a, b []string) []op {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	ops := make([]op, 0, len(a)+len(b)-p-s)
	for _, l := range a[:p] {
		ops = append(ops, op{' ', l})
	}
	ops = append(ops, lcsOps(a[p:len(a)-s], b[p:len(b)-s])...)
	for _, l := range a[len(a)-s:] {
		ops = append(ops, op{' ', l})
	}
	return ops
}

// lcsOps is the classic DP longest-common-subsequence edit script.
func lcsOps(a, b []string) []op {
	n, m := len(a), len(b)
	// lcs[i][j] = LCS length of a[i:], b[j:]
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	ops := make([]op, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{'-', a[i]})
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j]})
	}
	return ops
}

// renderHunks groups changes into hunks: changes separated by more than
// 2*contextLines equal lines start a new hunk; each hunk carries up to
// contextLines of equal lines on either side.
func renderHunks(ops []op) string {
	// aAt[i]/bAt[i]: lines of a/b consumed before ops[i]
	aAt := make([]int, len(ops)+1)
	bAt := make([]int, len(ops)+1)
	for i, o := range ops {
		aAt[i+1], bAt[i+1] = aAt[i], bAt[i]
		if o.kind != '+' {
			aAt[i+1]++
		}
		if o.kind != '-' {
			bAt[i+1]++
		}
	}
	var changes []int
	for i, o := range ops {
		if o.kind != ' ' {
			changes = append(changes, i)
		}
	}
	var out strings.Builder
	for c := 0; c < len(changes); {
		last := c
		for last+1 < len(changes) && changes[last+1]-changes[last] <= 2*contextLines {
			last++
		}
		start := max(0, changes[c]-contextLines)
		end := min(len(ops)-1, changes[last]+contextLines)
		aCount, bCount := aAt[end+1]-aAt[start], bAt[end+1]-bAt[start]
		aStart, bStart := aAt[start]+1, bAt[start]+1
		if aCount == 0 {
			aStart--
		}
		if bCount == 0 {
			bStart--
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart, aCount, bStart, bCount)
		for i := start; i <= end; i++ {
			out.WriteByte(ops[i].kind)
			out.WriteString(ops[i].line)
			out.WriteByte('\n')
		}
		c = last + 1
	}
	return out.String()
}
