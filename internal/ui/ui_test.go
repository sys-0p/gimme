package ui

import (
	"bytes"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestUI() (*UI, *bytes.Buffer) {
	var buf bytes.Buffer
	return &UI{logw: &buf, barw: &buf}, &buf
}

func TestLineAlignsLabel(t *testing.T) {
	u, buf := newTestUI()
	u.Line(StyleInfo, "Password", "Detected: secret")
	got := buf.String()
	want := "[ Password ] Detected: secret\n" // centered: pad 2 → left 1, right 1
	if got != want {
		t.Fatalf("Line = %q, want %q", got, want)
	}
}

func TestLineTruncatesLongLabel(t *testing.T) {
	u, buf := newTestUI()
	u.Line(StyleInfo, "0123456789ABCDEF", "x")
	got := buf.String()
	if !strings.HasPrefix(got, "[0123456789]") {
		t.Fatalf("long label not truncated to width 10: %q", got)
	}
}

func TestLinePlainNoMarker(t *testing.T) {
	u, buf := newTestUI() // color disabled
	u.Linef(StyleInfo, "Repair", "all files verified complete")
	got := buf.String()
	// No check-mark / "OK" prefix, and StyleInfo carries no color — just the
	// aligned tag + message.
	if strings.Contains(got, "✓") || strings.Contains(got, "OK ") || strings.Contains(got, "\x1b[") {
		t.Fatalf("StyleInfo line should be plain and unmarked, got %q", got)
	}
	if !strings.Contains(got, "all files verified complete") {
		t.Fatalf("Line missing message: %q", got)
	}
	if !strings.HasPrefix(got, "[  Repair  ]") {
		t.Fatalf("Line label not aligned: %q", got)
	}
}

// TestLineStyleColors checks that color is meaningful: only success/warn/error
// tint the text; ordinary info lines stay plain. The tag stays plain-tag text in
// this harness (label() adds bold-cyan only when color is on, which it is here).
func TestLineStyleColors(t *testing.T) {
	cases := []struct {
		name      string
		style     Style
		wantColor string // expected text color prefix, or "" for plain
	}{
		{"info", StyleInfo, ""},
		{"success", StyleSuccess, ansiGreen},
		{"warn", StyleWarn, ansiYellow},
		{"error", StyleError, ansiRed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			u := &UI{color: true, logw: &buf, barw: &buf}
			u.Line(tc.style, "Step", "msg")
			got := buf.String()
			if tc.wantColor == "" {
				// Info: text not wrapped in any fg color (tag may still be cyan).
				if strings.Contains(got, ansiGreen) || strings.Contains(got, ansiYellow) || strings.Contains(got, ansiRed) {
					t.Fatalf("StyleInfo leaked color: %q", got)
				}
				return
			}
			// The colored text is "<color>msg<reset>".
			if !strings.Contains(got, tc.wantColor+"msg"+ansiReset) {
				t.Fatalf("%s line missing %q around message: %q", tc.name, tc.wantColor, got)
			}
		})
	}
}

func TestLabelCentered(t *testing.T) {
	u, _ := newTestUI()
	cases := map[string]string{
		"Repair":     "[  Repair  ]", // 6 → left 2, right 2
		"Password":   "[ Password ]", // 8 → left 1, right 1
		"Connection": "[Connection]", // 10 → exact fit, no padding
		"Done":       "[   Done   ]", // 4 → left 3, right 3
	}
	for name, want := range cases {
		if got := u.label(name); got != want {
			t.Errorf("label(%q)=%q want %q", name, got, want)
		}
	}
}

func TestErrorAlwaysShows(t *testing.T) {
	var buf bytes.Buffer
	u := &UI{quiet: true, logw: &buf, barw: &buf} // quiet, but errors must surface
	u.Linef(StyleError, "Error", "unpack: %s", "boom")
	got := buf.String()
	if !strings.Contains(got, "[  Error   ]") {
		t.Fatalf("error line missing centered Error tag: %q", got)
	}
	if !strings.Contains(got, "unpack: boom") {
		t.Fatalf("error line missing message: %q", got)
	}
	if strings.Contains(got, "error:") {
		t.Fatalf("error line should not double-prefix with error:, got %q", got)
	}
}

func TestQuietSuppresses(t *testing.T) {
	var buf bytes.Buffer
	u := &UI{quiet: true, logw: &buf, barw: &buf}
	u.Logo()
	u.Line(StyleInfo, "X", "y")
	u.Line(StyleSuccess, "X", "y")
	u.Line(StyleWarn, "X", "y")
	u.VLine(StyleInfo, "X", "y")
	u.Printf("z")
	if buf.Len() != 0 {
		t.Fatalf("quiet UI produced output: %q", buf.String())
	}
}

// TestLogo checks the wordmark prints on a normal run, stays plain when color is
// off, is dimmed when color is on, and is fully suppressed when quiet.
func TestLogo(t *testing.T) {
	u, buf := newTestUI() // color off
	u.Logo()
	got := buf.String()
	if !strings.Contains(got, "-m|d") || !strings.Contains(got, "█") {
		t.Fatalf("Logo missing wordmark content: %q", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("Logo leaked color with color disabled: %q", got)
	}

	var cbuf bytes.Buffer
	(&UI{color: true, logw: &cbuf, barw: &cbuf}).Logo()
	if !strings.Contains(cbuf.String(), ansiDim) {
		t.Fatalf("color Logo missing dim: %q", cbuf.String())
	}

	var qbuf bytes.Buffer
	(&UI{quiet: true, logw: &qbuf, barw: &qbuf}).Logo()
	if qbuf.Len() != 0 {
		t.Fatalf("quiet Logo produced output: %q", qbuf.String())
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:        "0 B",
		512:      "512 B",
		1024:     "1.0 KiB",
		5 * 1024: "5.0 KiB",
		5 << 20:  "5.0 MiB",
		3 << 30:  "3.0 GiB",
		-1:       "0 B",
	}
	for in, want := range cases {
		if got := human(in); got != want {
			t.Errorf("human(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestProgressBarProportional(t *testing.T) {
	u, _ := newTestUI() // non-tty: no redraw goroutine
	p := u.NewProgress([]string{"conn 1"}, 100)
	if p.active {
		t.Fatal("progress should be inert on a non-terminal")
	}
	p.Add(0, 50)
	line := p.renderRow(p.rows[0], time.Now(), 3)
	if !strings.Contains(line, "50%") {
		t.Fatalf("proportional bar missing percent: %q", line)
	}
	if !strings.Contains(line, "conn 1") || !strings.Contains(line, "[") {
		t.Fatalf("bar missing label/saucer: %q", line)
	}
	// Every connection line must end with a speed indicator.
	if !strings.Contains(line, "/s") {
		t.Fatalf("bar missing speed indicator: %q", line)
	}
	// Clamp above 100%.
	p.Add(0, 1000)
	if l := p.renderRow(p.rows[0], time.Now(), 4); !strings.Contains(l, "100%") {
		t.Fatalf("bar did not clamp at 100%%: %q", l)
	}
}

func TestProgressCountModeBounces(t *testing.T) {
	u, _ := newTestUI()
	p := u.NewProgress([]string{"par2"}, 0)
	// Count-mode saucer must animate across frames and stay within bounds.
	var prev []string
	for f := int64(1); f <= 6; f++ {
		line := p.renderRow(p.rows[0], time.Now(), f)
		if !strings.Contains(line, ">") {
			t.Fatalf("count-mode bar missing marker on frame %d: %q", f, line)
		}
		if strings.Contains(line, "%") {
			t.Fatalf("count-mode bar should not show a percentage: %q", line)
		}
		prev = append(prev, line)
	}
	if prev[0] == prev[3] {
		t.Fatal("count-mode bar did not animate between frames")
	}
}

func TestProgressOutOfRangeSafe(t *testing.T) {
	u, _ := newTestUI()
	p := u.NewProgress([]string{"a"}, 10)
	p.Add(-1, 5) // no panic
	p.Add(99, 5) // no panic
	p.SetLabel(99, "x")
	p.Finish() // inert when not active
}

func TestLayoutCols(t *testing.T) {
	// pitch is the per-column width; width/pitch drives the column count,
	// capped at maxColumns, and never below the single-column fallback.
	cases := []struct {
		width, n, want int
	}{
		{200, 60, maxColumns}, // very wide: capped at maxColumns
		{80, 60, 3},           // pitch=25 → 80/25=3
		{60, 60, 2},           // 60/25=2
		{40, 60, 1},           // too narrow for a 2nd column → rich
		{1000, 8, maxColumns}, // many cols available but capped
		{200, 4, 1},           // fewer rows than gridMinRows → rich
		{200, 2, 1},           // two connections stay rich/detail
	}
	for _, c := range cases {
		if got := layoutCols(c.width, c.n); got != c.want {
			t.Errorf("layoutCols(%d,%d)=%d, want %d", c.width, c.n, got, c.want)
		}
	}
}

func TestShortLabel(t *testing.T) {
	if got := shortLabel("conn 12"); got != "c12" {
		t.Errorf("shortLabel(conn 12)=%q want c12", got)
	}
	if got := shortLabel("extract"); got != "extract" {
		t.Errorf("shortLabel(extract)=%q want extract", got)
	}
}

func TestRenderCellFixedWidth(t *testing.T) {
	u, _ := newTestUI() // color off
	p := u.NewProgress([]string{"conn 12"}, 1000)
	p.Add(0, 500)
	cell := p.renderCell(p.rows[0], time.Now(), 3)
	if len(cell) != compactCellW {
		t.Fatalf("compact cell width = %d, want %d (%q)", len(cell), compactCellW, cell)
	}
	// Count-mode cells keep the same fixed width (grid stays rectangular).
	q := u.NewProgress([]string{"r1"}, 0)
	q.Add(0, 65536)
	if c := q.renderCell(q.rows[0], time.Now(), 4); len(c) != compactCellW {
		t.Fatalf("count cell width = %d, want %d (%q)", len(c), compactCellW, c)
	}
}

func TestRedrawMultiColumnGrid(t *testing.T) {
	u, buf := newTestUI() // barw is buf (not a *os.File) → termWidth falls back to 80
	labels := make([]string, 60)
	for i := range labels {
		labels[i] = fmt.Sprintf("conn %d", i+1)
	}
	p := u.NewProgress(labels, 1000) // active stays false (no terminal)
	p.redraw()                       // draw one frame directly

	lines := strings.Split(buf.String(), "\n")
	rowsPerCol := (60 + maxColumns - 1) / maxColumns // 20
	if len(lines) != rowsPerCol {
		t.Fatalf("grid drew %d lines, want %d", len(lines), rowsPerCol)
	}
	wantLineLen := maxColumns*compactCellW + (maxColumns-1)*cellGap
	for i, ln := range lines {
		if len(ln) != wantLineLen {
			t.Fatalf("line %d width = %d, want %d (%q)", i, len(ln), wantLineLen, ln)
		}
	}
	// Column-major fill: row 0 holds the top of each column — c1, c21, c41.
	top := lines[0]
	for _, want := range []string{"c1", "c21", "c41"} {
		if !strings.Contains(top, want) {
			t.Errorf("top line %q missing %q", top, want)
		}
	}
	// A connection must appear on a later row (c2 sits at row 1, col 0).
	if !strings.Contains(lines[1], "c2") {
		t.Errorf("row 1 %q missing c2", lines[1])
	}
	// The widest grid must fit within the 80-column fallback (no wrap).
	if wantLineLen > 80 {
		t.Fatalf("grid width %d exceeds 80-col fallback", wantLineLen)
	}
}

// TestSegPathsMonotonic asserts every fill path is a strict chain where each step
// adds exactly one cube, starting from the single-cube glyph and ending at the
// full block. This is what guarantees a matrix cell only ever densifies.
func TestSegPathsMonotonic(t *testing.T) {
	const sparse = '\U0001FB00' // BLOCK SEXTANT-1: a single cube
	const full = '█'            // FULL BLOCK
	for i, p := range segPaths {
		if p[0] != sparse {
			t.Fatalf("path %d starts %U want %U", i, p[0], sparse)
		}
		if p[len(p)-1] != full {
			t.Fatalf("path %d ends %U want %U", i, p[len(p)-1], full)
		}
		for k := 0; k+1 < len(p); k++ {
			a, okA := cubeBits(p[k])
			b, okB := cubeBits(p[k+1])
			if !okA || !okB {
				t.Fatalf("path %d step %d: glyph %U is not a BLOCK SEXTANT/full glyph", i, k, p[k])
			}
			if a&b != a {
				t.Fatalf("path %d step %d: %U (%06b) not a subset of %U (%06b)", i, k, p[k], a, p[k+1], b)
			}
			if added := b &^ a; added&(added-1) != 0 {
				t.Fatalf("path %d step %d: must add exactly one cube, added %06b", i, k, added)
			}
		}
	}
}

// cubeCount returns the number of filled sub-blocks a braille glyph represents, or
// -1 for a rune cubeBits cannot decode.
func cubeCount(r rune) int {
	m, ok := cubeBits(r)
	if !ok {
		return -1
	}
	n := 0
	for ; m != 0; m >>= 1 {
		n += int(m & 1)
	}
	return n
}

// TestMatrixFillAdvancesMonotonically verifies a cell maps messages to cubes 1:1:
// an empty cell is the faint dot (no faked progress), each landed message adds
// exactly one cube along the cell's assigned path, a partial cell never masquerades
// as the full block, and only a fully-present cell renders the solid block. Done
// counters stay idempotent.
// TestMatrixFillAnimation drives animGlyph with explicit monotonic timestamps to
// verify the cosmetic fill-in: a landing does not pop to the solid block, it
// densifies one cube at a time over segAnimDuration and settles exactly on the block.
// The first sight of a cell snaps (cold frame of dots), not animates.
func TestMatrixFillAnimation(t *testing.T) {
	const t0 = int64(1_000_000_000)
	g := newTestGrid(1, false) // single-message cell: 0 -> full(6)
	g.resetAnim(1)
	path := g.path(0)

	if got := g.animGlyph(t0, 0, 1, 0); got != segEmptyGlyph {
		t.Fatalf("cold first frame = %q want dot %q", got, segEmptyGlyph)
	}
	g.set(0, true) // the real 0 -> 1 transition

	// Frame right at the landing instant: still the dot (elapsed 0), not the block.
	if got := g.animGlyph(t0, 0, 1, 0); got != segEmptyGlyph {
		t.Fatalf("frame at landing instant = %q want dot %q", got, segEmptyGlyph)
	}
	// Halfway through: densifying — a partial cube glyph, never the block, never blank.
	mid := g.animGlyph(t0+int64(segAnimDuration)/2, 0, 1, 0)
	if mid == segDoneGlyph || mid == segEmptyGlyph {
		t.Fatalf("mid-animation glyph %q must be a partial cube", mid)
	}
	midCubes := cubeCount([]rune(mid)[0])
	if midCubes < 1 || midCubes >= segPathLen {
		t.Fatalf("mid-animation cubes = %d want 1..%d", midCubes, segPathLen-1)
	}
	// Once the duration has elapsed: settled on the solid block, and stays there.
	if got := g.animGlyph(t0+int64(segAnimDuration), 0, 1, 0); got != segDoneGlyph {
		t.Fatalf("settled glyph %q want block %q", got, segDoneGlyph)
	}
	if got := g.animGlyph(t0+int64(3*segAnimDuration), 0, 1, 0); got != segDoneGlyph {
		t.Fatalf("post-settle glyph %q want block %q", got, segDoneGlyph)
	}
	_ = path
}

// TestMatrixFillAnimationMonotonic checks that across sampled frames a filling cell's
// cube count never decreases and only reaches the solid block at the very end.
func TestMatrixFillAnimationMonotonic(t *testing.T) {
	const t0 = int64(5_000_000_000)
	g := newTestGrid(6, false) // 6-message cell; land them one per sampled frame
	g.resetAnim(6)
	g.set(0, true) // start: 1 cube target, then add more mid-flight
	prev := -1
	for step := 0; step <= 12; step++ {
		now := t0 + int64(step)*int64(segAnimDuration)/4 // quarter-second frames
		if step == 4 {
			g.set(1, true)
		} // raise target mid animation
		cell := g.animGlyph(now, 0, 6, 0)
		if cell == segDoneGlyph {
			if step < 8 {
				t.Fatalf("block shown too early at step %d", step)
			}
			prev = segPathLen
			continue
		}
		cubes := cubeCount([]rune(cell)[0])
		if cell == segEmptyGlyph {
			cubes = 0
		}
		if cubes < prev {
			t.Fatalf("fill regressed at step %d: %d < %d", step, cubes, prev)
		}
		prev = cubes
	}
}

// TestMatrixFillAdvancesMonotonically verifies a cell maps messages to cubes 1:1:
// an empty cell is the faint dot (no faked progress), each landed message adds
// exactly one cube along the cell's assigned path, a partial cell never masquerades
// as the full block, and only a fully-present cell renders the solid block. Done
// counters stay idempotent.
func TestMatrixFillAdvancesMonotonically(t *testing.T) {
	g := newTestGrid(6, false) // one cell of 6 messages -> exactly one cube each
	path := g.path(0)          // cell 0's assigned path (fixed for the run)
	if got := g.cellGlyph(0, 6, 0); got != segEmptyGlyph {
		t.Fatalf("empty cell=%q want empty dot %q", got, segEmptyGlyph)
	}
	prevCubes := 0
	for filled := 1; filled <= 6; filled++ {
		g.set(filled-1, true)
		cell := g.cellGlyph(0, 6, 0)
		if filled == 6 {
			if cell != segDoneGlyph {
				t.Fatalf("full cell=%q want %q", cell, segDoneGlyph)
			}
			continue
		}
		if cell == segDoneGlyph || cell == segDamagedPlain || cell == segEmptyGlyph {
			t.Fatalf("partial cell %q must not read as done/damaged/empty", cell)
		}
		r := []rune(cell)[0]
		if want := path[filled-1]; r != want {
			t.Fatalf("cell glyph %U want path[%d]=%U", r, filled-1, want)
		}
		cubes := cubeCount(r)
		if cubes != filled {
			t.Fatalf("glyph %U cubes=%d want %d (one cube per landed message)", r, cubes, filled)
		}
		if cubes <= prevCubes {
			t.Fatalf("fill did not advance at %d/6 (cubes %d <= prev %d)", filled, cubes, prevCubes)
		}
		prevCubes = cubes
	}
	before := g.done.Load()
	g.set(0, true)
	if g.done.Load() != before {
		t.Fatalf("re-set changed done to %d want %d", g.done.Load(), before)
	}
}

// TestMatrixGroupedCellScales verifies a cell holding more than segPathLen messages
// (a huge file bucketed down to fit the grid) still densifies monotonically, starts
// non-empty after the first landing, and only becomes the solid block when complete.
func TestMatrixGroupedCellScales(t *testing.T) {
	const size = 40 // > segPathLen (6): each cube stands for a group
	g := newTestGrid(size, false)
	if got := g.cellGlyph(0, size, 0); got != segEmptyGlyph {
		t.Fatalf("empty grouped cell=%q want %q", got, segEmptyGlyph)
	}
	prevCubes := -1
	prev := " "
	for filled := 1; filled <= size; filled++ {
		g.set(filled-1, true)
		cell := g.cellGlyph(0, size, 0)
		r := []rune(cell)[0]
		if filled == size {
			if cell != segDoneGlyph {
				t.Fatalf("complete grouped cell=%q want %q", cell, segDoneGlyph)
			}
			continue
		}
		cubes := cubeCount(r)
		if cubes < 1 {
			t.Fatalf("grouped cell went blank at %d/%d: %q", filled, size, cell)
		}
		if cubes < prevCubes {
			t.Fatalf("grouped fill regressed at %d/%d (cubes %d < prev %d)", filled, size, cubes, prevCubes)
		}
		if cell == prev && filled > 1 { // density must eventually rise, never stall at full
			if cubes == segPathLen {
				t.Fatalf("grouped cell faked completion at %d/%d", filled, size)
			}
		}
		prevCubes, prev = cubes, cell
	}
}

// TestMatrixCellPathAssignment verifies each cell draws the head glyph of its own
// randomly-assigned path, that the assignment is stable across calls (a cell never
// flickers), and that the pool scatters neighbours onto more than one path.
func TestMatrixCellPathAssignment(t *testing.T) {
	g := newTestGrid(8, false)
	g.set(0, true) // cell 0: 1 of 4 messages -> head glyph path[0]
	g.set(4, true) // cell 1: 1 of 4 messages -> head glyph path[0]
	for _, c := range []int{0, 1} {
		lo, hi := c*4, c*4+4
		want := g.path(c)[0]
		for i := 0; i < 5; i++ {
			if got := []rune(g.cellGlyph(lo, hi, c))[0]; got != want {
				t.Fatalf("cell %d glyph %U unstable/wrong, want %U", c, got, want)
			}
		}
	}
	// Selection must actually vary: a spread of cells hits several distinct paths.
	seen := map[uint32]bool{}
	for c := 0; c < 64; c++ {
		seen[g.pathIndex(c)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("path selection did not scatter: only %d distinct path(s)", len(seen))
	}
}

func TestMatrixCellStates(t *testing.T) {
	u, _ := newTestUI() // color off
	p := u.NewProgress([]string{"c1"}, 0)
	p.InitMatrix(4)
	g := p.grid
	g.set(0, true)
	g.set(1, true)
	g.set(2, false) // damaged
	// Cell [0,2): both done → solid block.
	if got := g.cellGlyph(0, 2, 0); got != segDoneGlyph {
		t.Fatalf("done cell=%q want %q", got, segDoneGlyph)
	}
	// Cell [2,3): damaged → plain 'x' marker when color off.
	if got := g.cellGlyph(2, 3, 0); got != segDamagedPlain {
		t.Fatalf("damaged cell=%q want %q", got, segDamagedPlain)
	}
	// Cell [3,4): still pending (no message landed) → faint empty dot, not a block/x.
	if got := g.cellGlyph(3, 4, 0); got != segEmptyGlyph {
		t.Fatalf("pending cell=%q want empty dot %q", got, segEmptyGlyph)
	}
	// Damaged wins within a mixed bucket.
	if got := g.cellGlyph(0, 3, 0); got != segDamagedPlain {
		t.Fatalf("mixed cell should read damaged, got %q", got)
	}
}

func TestMatrixRedrawDrawsBlock(t *testing.T) {
	u, buf := newTestUI() // barw=buf → termWidth falls back to 80
	p := u.NewProgress([]string{"conn 1"}, 0)
	// 80 cols minus a "[m0001-0066] "-style range tag (2*4+6 = 14) = 66 glyph
	// columns; 200 segs / 66 = 4 rows.
	p.InitMatrix(200)
	p.MarkSeg(0, true)
	p.MarkSeg(1, false)
	p.redraw()
	lines := strings.Split(buf.String(), "\n")
	// 1 rich bar line + 4 matrix rows.
	if want := 1 + 4; len(lines) != want {
		t.Fatalf("redraw drew %d lines, want %d (%q)", len(lines), want, buf.String())
	}
	joined := strings.Join(lines[1:], "")
	if !strings.Contains(joined, segDoneGlyph) {
		t.Fatalf("matrix missing done block: %q", joined)
	}
	if !strings.Contains(joined, segDamagedPlain) {
		t.Fatalf("matrix missing damaged marker: %q", joined)
	}
	// Every matrix row is prefixed with its 1-based segment-range tag.
	if !strings.HasPrefix(lines[1], "[m0001-0066] ") {
		t.Fatalf("first matrix row missing range tag: %q", lines[1])
	}
	if !strings.HasPrefix(lines[len(lines)-1], "[m0199-0200] ") {
		t.Fatalf("last matrix row has wrong range tag: %q", lines[len(lines)-1])
	}
	for _, ln := range lines[1:] {
		if r := []rune(ln); len(r) > 80 {
			t.Fatalf("matrix row exceeds 80 cols with tag: %d (%q)", len(r), ln)
		}
	}
}

func TestMatrixBucketingForManySegments(t *testing.T) {
	u, buf := newTestUI() // width fallback 80
	p := u.NewProgress([]string{"conn 1"}, 0)
	p.InitMatrix(100000) // far more than width*matrixMaxRows
	p.redraw()
	lines := strings.Split(buf.String(), "\n")
	wantRows := 1 + matrixMaxRows // bar + capped matrix rows
	if len(lines) != wantRows {
		t.Fatalf("drew %d lines, want %d", len(lines), wantRows)
	}
	for i, ln := range lines[1:] {
		// Range tag + glyphs must together stay within the 80-col fallback.
		if r := []rune(ln); len(r) > 80 {
			t.Fatalf("matrix row %d too wide: %d runes", i, len(r))
		}
		if !strings.HasPrefix(ln, "[m") {
			t.Fatalf("matrix row %d missing range tag: %q", i, ln)
		}
	}
	// Last row's tag must be clamped to the true segment count, never overshoot.
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "[m") || !strings.Contains(last, "-100000]") {
		t.Fatalf("last row tag not clamped to 100000: %q", last)
	}
}

func TestRedrawSingleColumnForFewRows(t *testing.T) {
	u, buf := newTestUI()
	p := u.NewProgress([]string{"conn 1", "conn 2"}, 1000)
	p.redraw()
	lines := strings.Split(buf.String(), "\n")
	if len(lines) != 2 {
		t.Fatalf("2 connections should draw 2 rich lines, got %d (%q)", len(lines), buf.String())
	}
	// Rich lines carry the elapsed/byte detail that compact grid cells omit.
	if !strings.Contains(lines[0], "conn 1") {
		t.Errorf("rich line missing full label: %q", lines[0])
	}
}

func newTestGrid(n int, color bool) *segmentGrid {
	return &segmentGrid{st: make([]atomic.Int32, n), n: n, color: color}
}

// TestMatrixPresentCellNotBrighten is the regression guard for the "full then
// bright" bug: a present cell must render in the SAME dim intensity as a pending
// cell (only the glyph shape densifies), and there is no green/extracted
// highlight at all any more.
func TestMatrixPresentCellNotBrighten(t *testing.T) {
	g := newTestGrid(2, true)
	g.set(0, true)
	g.set(1, true)
	cell := g.cellGlyph(0, 2, 0)
	if cell != ansiDim+segDoneGlyph+ansiReset {
		t.Fatalf("present cell should be a dim block, got %q", cell)
	}
	if strings.Contains(cell, ansiGreen) {
		t.Fatalf("present cell must not use a green highlight: %q", cell)
	}
	// A present cell and a pending cell carry the same dim intensity.
	if !strings.HasPrefix(cell, ansiDim) {
		t.Fatalf("present cell must be dim: %q", cell)
	}
	if pending := g.cellGlyph(0, 1, 0); !strings.HasPrefix(pending, ansiDim) {
		t.Fatalf("pending cell must be dim too: %q", pending)
	}
}

// TestMatrixDamagedStaysHighlighted confirms gaps keep their light-red cue (the
// one intentional brightness highlight) while every other cell stays dim.
func TestMatrixDamagedStaysHighlighted(t *testing.T) {
	g := newTestGrid(3, true)
	g.set(0, true)  // present → dim
	g.set(1, false) // damaged → bright red
	if c := g.cellGlyph(0, 1, 0); !strings.HasPrefix(c, ansiDim) || strings.Contains(c, ansiBright) {
		t.Fatalf("present cell should be dim, not bright: %q", c)
	}
	if c := g.cellGlyph(1, 2, 0); !strings.Contains(c, ansiBright) {
		t.Fatalf("damaged cell should keep the light-red highlight: %q", c)
	}
}

// TestMatrixNoExtractedState verifies the extracted state was removed: MarkSeg
// only ever records present(1)/damaged(2), never a third state.
func TestMatrixNoExtractedState(t *testing.T) {
	u, _ := newTestUI()
	p := u.NewProgress([]string{"c1"}, 0)
	p.InitMatrix(4)
	p.MarkSeg(0, true)
	if got := p.grid.st[0].Load(); got != 1 {
		t.Fatalf("present seg=%d want 1 (no extracted state)", got)
	}
	p.MarkSeg(0, true) // idempotent
	if got := p.grid.done.Load(); got != 1 {
		t.Fatalf("re-mark changed done to %d want 1", got)
	}
}

func TestLineWritesToLogwWhenInactive(t *testing.T) {
	u, buf := newTestUI()
	p := u.NewProgress([]string{"c1"}, 0) // non-TTY barw → never active
	p.Line("hello")
	if !strings.Contains(buf.String(), "hello") {
		t.Fatalf("inactive Line should write to logw, got %q", buf.String())
	}
}

// TestLineInsertsAboveWhenActive exercises the anti-smear path: with the loop
// "active", Line must erase the live block, print the static line, then redraw
// the block BELOW it — so the block reappears after the static text.
func TestLineInsertsAboveWhenActive(t *testing.T) {
	u, buf := newTestUI()
	p := u.NewProgress([]string{"conn 1"}, 0) // not active on a non-TTY buffer
	p.active = true                           // exercise the active code path
	p.redraw()                                // establish a drawn block (sets p.drawn >= 1)
	if p.drawn == 0 {
		t.Fatalf("expected a drawn block before Line")
	}
	buf.Reset()
	p.Line("STATIC-LINE")
	out := buf.String()
	// Must erase the previous block first.
	if !strings.Contains(out, "\x1b[2K") {
		t.Fatalf("active Line did not erase the block: %q", out)
	}
	iStatic := strings.Index(out, "STATIC-LINE")
	iBar := strings.Index(out, "conn 1") // redrawn block
	if iStatic < 0 || iBar < 0 {
		t.Fatalf("missing static line or redrawn block in %q", out)
	}
	if iBar < iStatic {
		t.Fatalf("block must be redrawn BELOW the static line (bar@%d < static@%d)", iBar, iStatic)
	}
}
