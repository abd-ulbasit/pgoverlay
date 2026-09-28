// Package diffutil is a small pure-Go line diff producing unified-style hunks.
// It exists so the engine can render schema diffs without shelling out to
// diff(1) or adding a dependency.
//
// The edit script comes from Myers' O(ND) algorithm in its linear-space
// divide-and-conquer form (the same shape as GNU diff's diffseq): memory is
// O(N+M) whatever the inputs, and a cost cap bounds the time spent on inputs
// that differ almost everywhere (see tooExpensive).
package diffutil

import (
	"fmt"
	"math"
	"strings"
)

// contextLines is the number of unchanged lines shown around each change.
const contextLines = 3

// minTooExpensive is the floor of the per-split cost cap: a split whose
// middle-snake search reaches this many edits without meeting stops searching
// for the optimum and splits at the furthest point reached. Real schema diffs
// (a migration's worth of changes) never get near it; it only caps the work
// on pathological inputs, trading minimality for bounded time.
const minTooExpensive = 1024

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

// diffOps computes the line-level edit script: within each run of changes,
// every deletion precedes every insertion (the usual unified-diff order).
func diffOps(a, b []string) []op {
	return diffOpsCap(a, b, 0)
}

// diffOpsCap is diffOps with an explicit cost cap (0 = the default for the
// input size); tests lower it to exercise the capped path on small inputs.
func diffOpsCap(a, b []string, costCap int) []op {
	delA, insB := changes(a, b, costCap)
	ops := make([]op, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && delA[i]:
			ops = append(ops, op{'-', a[i]})
			i++
		case j < len(b) && insB[j]:
			ops = append(ops, op{'+', b[j]})
			j++
		default:
			// both unchanged: the alignment is order-preserving, so these
			// are a matched pair
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		}
	}
	return ops
}

// changes marks which lines of a are deleted and which lines of b are
// inserted by a shortest (or, past the cost cap, a short) edit script.
func changes(a, b []string, costCap int) (delA, insB []bool) {
	delA = make([]bool, len(a))
	insB = make([]bool, len(b))

	// Common prefix and suffix need no search.
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	am, bm := a[p:len(a)-s], b[p:len(b)-s]

	// Intern lines so the search compares ints, and drop lines that occur
	// only on one side: they cannot be part of any common subsequence, so
	// they are changes, and removing them first keeps the search small (a
	// migration's new tables are mostly lines the base never had).
	ids := make(map[string]int, len(am)+len(bm))
	intern := func(l string) int {
		id, ok := ids[l]
		if !ok {
			id = len(ids)
			ids[l] = id
		}
		return id
	}
	ai, bi := make([]int, len(am)), make([]int, len(bm))
	for i, l := range am {
		ai[i] = intern(l)
	}
	for j, l := range bm {
		bi[j] = intern(l)
	}
	inA, inB := make([]bool, len(ids)), make([]bool, len(ids))
	for _, id := range ai {
		inA[id] = true
	}
	for _, id := range bi {
		inB[id] = true
	}
	var xs, ys []int     // kept lines (interned)
	var xIdx, yIdx []int // their index in am / bm
	for i, id := range ai {
		if inB[id] {
			xs, xIdx = append(xs, id), append(xIdx, i)
		} else {
			delA[p+i] = true
		}
	}
	for j, id := range bi {
		if inA[id] {
			ys, yIdx = append(ys, id), append(yIdx, j)
		} else {
			insB[p+j] = true
		}
	}

	d := newDiffer(xs, ys, costCap)
	d.compare(0, len(xs), 0, len(ys), false)
	for k, del := range d.delX {
		if del {
			delA[p+xIdx[k]] = true
		}
	}
	for k, ins := range d.insY {
		if ins {
			insB[p+yIdx[k]] = true
		}
	}
	return delA, insB
}

// differ is the linear-space Myers search over two interned sequences. fd
// and bd hold, per diagonal k = x-y (offset by off), the furthest x reached
// by the forward and backward searches; both are reused across every split,
// so the whole diff allocates O(N+M).
type differ struct {
	x, y         []int
	delX, insY   []bool
	fd, bd       []int
	off          int
	tooExpensive int
}

func newDiffer(x, y []int, costCap int) *differ {
	n := len(x) + len(y) + 3
	if costCap <= 0 {
		// GNU diff's rule: about sqrt(N+M), with a floor.
		costCap = max(minTooExpensive, int(math.Sqrt(float64(n))))
	}
	return &differ{
		x: x, y: y,
		delX: make([]bool, len(x)), insY: make([]bool, len(y)),
		fd: make([]int, n), bd: make([]int, n),
		off:          len(y) + 1,
		tooExpensive: costCap,
	}
}

// compare marks the edits turning x[xoff:xlim] into y[yoff:ylim]. minimal
// forbids the cost cap for this range (set on the side of a capped split
// whose edit distance is already known to be small).
func (d *differ) compare(xoff, xlim, yoff, ylim int, minimal bool) {
	for xoff < xlim && yoff < ylim && d.x[xoff] == d.y[yoff] {
		xoff++
		yoff++
	}
	for xoff < xlim && yoff < ylim && d.x[xlim-1] == d.y[ylim-1] {
		xlim--
		ylim--
	}
	switch {
	case xoff == xlim:
		for ; yoff < ylim; yoff++ {
			d.insY[yoff] = true
		}
	case yoff == ylim:
		for ; xoff < xlim; xoff++ {
			d.delX[xoff] = true
		}
	default:
		xmid, ymid, loMin, hiMin := d.split(xoff, xlim, yoff, ylim, minimal)
		d.compare(xoff, xmid, yoff, ymid, loMin)
		d.compare(xmid, xlim, ymid, ylim, hiMin)
	}
}

// split finds a point (xmid, ymid) on a shortest edit path through
// x[xoff:xlim] × y[yoff:ylim] by running the forward and backward searches
// until they overlap (Myers' "middle snake"). Unless minimal is set, once the
// search cost reaches tooExpensive it gives up on the optimum and returns the
// furthest point either search reached; loMin/hiMin then say which half is
// known to be cheap enough to solve exactly.
func (d *differ) split(xoff, xlim, yoff, ylim int, minimal bool) (xmid, ymid int, loMin, hiMin bool) {
	fd, bd, off := d.fd, d.bd, d.off
	dmin, dmax := xoff-ylim, xlim-yoff // valid diagonals
	fmid, bmid := xoff-yoff, xlim-ylim // centre diagonals of each search
	fmin, fmax := fmid, fmid
	bmin, bmax := bmid, bmid
	odd := (fmid-bmid)&1 != 0
	fd[off+fmid] = xoff
	bd[off+bmid] = xlim

	for c := 1; ; c++ {
		// forward search: one more edit on every active diagonal
		if fmin > dmin {
			fmin--
			fd[off+fmin-1] = -1
		} else {
			fmin++
		}
		if fmax < dmax {
			fmax++
			fd[off+fmax+1] = -1
		} else {
			fmax--
		}
		for k := fmax; k >= fmin; k -= 2 {
			tlo, thi := fd[off+k-1], fd[off+k+1]
			x := tlo + 1
			if tlo < thi {
				x = thi
			}
			y := x - k
			for x < xlim && y < ylim && d.x[x] == d.y[y] {
				x++
				y++
			}
			fd[off+k] = x
			if odd && bmin <= k && k <= bmax && bd[off+k] <= x {
				return x, y, true, true
			}
		}

		// backward search, symmetrically
		if bmin > dmin {
			bmin--
			bd[off+bmin-1] = math.MaxInt
		} else {
			bmin++
		}
		if bmax < dmax {
			bmax++
			bd[off+bmax+1] = math.MaxInt
		} else {
			bmax--
		}
		for k := bmax; k >= bmin; k -= 2 {
			tlo, thi := bd[off+k-1], bd[off+k+1]
			x := thi - 1
			if tlo < thi {
				x = tlo
			}
			y := x - k
			for x > xoff && y > yoff && d.x[x-1] == d.y[y-1] {
				x--
				y--
			}
			bd[off+k] = x
			if !odd && fmin <= k && k <= fmax && x <= fd[off+k] {
				return x, y, true, true
			}
		}

		if minimal || c < d.tooExpensive {
			continue
		}

		// Too expensive: split at whichever search got further.
		fxyBest, fxBest := -1, 0
		for k := fmax; k >= fmin; k -= 2 {
			x := min(fd[off+k], xlim)
			y := x - k
			if ylim < y {
				x, y = ylim+k, ylim
			}
			if fxyBest < x+y {
				fxyBest, fxBest = x+y, x
			}
		}
		bxyBest, bxBest := math.MaxInt, 0
		for k := bmax; k >= bmin; k -= 2 {
			x := max(xoff, bd[off+k])
			y := x - k
			if y < yoff {
				x, y = yoff+k, yoff
			}
			if x+y < bxyBest {
				bxyBest, bxBest = x+y, x
			}
		}
		if (xlim+ylim)-bxyBest < fxyBest-(xoff+yoff) {
			return fxBest, fxyBest - fxBest, true, false
		}
		return bxBest, bxyBest - bxBest, false, true
	}
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
