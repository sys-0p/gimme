// Package ui provides terminal output: leveled logging to stderr, a
// structured "[Label]  key: value" section printer, and a TTY-aware progress
// renderer that draws one status line per row (e.g. a download/extract speed
// line) with a per-line throughput indicator, plus an optional per-segment
// matrix. All decoration is suppressed when not attached to a terminal or when
// quiet, so
// piped/CI output stays plain and log-safe.
package ui

import (
	crand "crypto/rand"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// ANSI style helpers (only emitted when color is enabled).
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiCyan   = "\x1b[36m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
	ansiBright = "\x1b[91m" // light red — damaged-segment cue in the matrix
	ansiDim    = "\x1b[2m"
)

// Segment-matrix tuning: each cell covers one or more NZB segments and shows a
// braille block beneath the download line. A cell's fill level (segments placed
// in it) walks its own fixed path in segPaths from sparse to full, so a cell can
// only ever gain cubes — the shape advances by exactly one sub-block per step
// and never flickers to a random glyph. To avoid a jarring "fills then
// brightens" jump, BOTH partial and full cells render in the same dim intensity;
// only the shape changes. Damaged segments are the one highlight (light red), so
// gaps stay visible for the whole run; the field persists on screen.
const (
	matrixMaxRows   = 8
	segDoneGlyph    = "█" // all segments placed (dim): full block
	segEmptyGlyph   = "·" // no message placed yet (dim): faint placeholder
	segDamagedGlyph = "█" // same block, light red = segment gap
	segDamagedPlain = "x" // non-color fallback for a damaged segment
)

// segAnimDuration is how long a cell spends "filling in" (its path densifying from
// the previous fill count up to the new one) after a message lands. This is purely
// cosmetic: the underlying state is an instant 0->1 transition, but densifying the
// braille over ~a second makes each landed message read as a bit of magic rather
// than a hard pop from a dot to a solid block.
const segAnimDuration = time.Second

// segPath is one monotonic braille "fill path": a chain of segPathLen glyphs where
// each step is the previous one plus exactly one sub-block (one cube), ending at the
// full block (U+2588). A matrix cell draws one path and its fill level picks a
// position along it, so a partially-filled cell only ever densifies (+1 cube).
const segPathLen = 6

type segPath [segPathLen]rune

// fullSegPath is the canonical 6-cube ladder (BLOCK SEXTANT-1 .. FULL BLOCK); it is
// the fallback used when segPaths is empty and documents the required path shape.
var fullSegPath = segPath{'\U0001FB00', '\U0001FB02', '\U0001FB06', '\U0001FB0E', '\U0001FB1D', '\u2588'}

// segPaths is the pool of fill paths cells draw from. To extend the look, append
// more rows below; each row MUST be a monotonic chain of BLOCK SEXTANT glyphs
// (U+1FB00..U+1FB3B) ending at the full block and adding exactly one cube per step.
// TestSegPathsMonotonic enforces that, and cubeBits decodes any such glyph to its
// filled-cube mask. A cell is assigned one path at random (fixed for the whole run,
// so it never flickers) via segmentGrid.pathIndex.
var segPaths = []segPath{
	{'\U0001fb00', '\U0001fb02', '\U0001fb06', '\U0001fb0e', '\U0001fb1d', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb06', '\U0001fb0e', '\U0001fb2c', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb06', '\U0001fb15', '\U0001fb1d', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb06', '\U0001fb15', '\U0001fb34', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb06', '\U0001fb25', '\U0001fb2c', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb06', '\U0001fb25', '\U0001fb34', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb0a', '\U0001fb0e', '\U0001fb1d', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb0a', '\U0001fb0e', '\U0001fb2c', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb0a', '\U0001fb19', '\U0001fb1d', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb0a', '\U0001fb19', '\U0001fb38', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb0a', '\U0001fb28', '\U0001fb2c', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb0a', '\U0001fb28', '\U0001fb38', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb12', '\U0001fb15', '\U0001fb1d', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb12', '\U0001fb15', '\U0001fb34', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb12', '\U0001fb19', '\U0001fb1d', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb12', '\U0001fb19', '\U0001fb38', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb12', '\U0001fb30', '\U0001fb34', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb12', '\U0001fb30', '\U0001fb38', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb21', '\U0001fb25', '\U0001fb2c', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb21', '\U0001fb25', '\U0001fb34', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb21', '\U0001fb28', '\U0001fb2c', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb21', '\U0001fb28', '\U0001fb38', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb21', '\U0001fb30', '\U0001fb34', '\u2588'},
	{'\U0001fb00', '\U0001fb02', '\U0001fb21', '\U0001fb30', '\U0001fb38', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb06', '\U0001fb0e', '\U0001fb1d', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb06', '\U0001fb0e', '\U0001fb2c', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb06', '\U0001fb15', '\U0001fb1d', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb06', '\U0001fb15', '\U0001fb34', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb06', '\U0001fb25', '\U0001fb2c', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb06', '\U0001fb25', '\U0001fb34', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb0c', '\U0001fb0e', '\U0001fb1d', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb0c', '\U0001fb0e', '\U0001fb2c', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb0c', '\U0001fb1b', '\U0001fb1d', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb0c', '\U0001fb1b', '\U0001fb3a', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb0c', '\U0001fb2a', '\U0001fb2c', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb0c', '\U0001fb2a', '\U0001fb3a', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb23', '\U0001fb25', '\U0001fb2c', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb23', '\U0001fb25', '\U0001fb34', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb23', '\U0001fb2a', '\U0001fb2c', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb23', '\U0001fb2a', '\U0001fb3a', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb23', '\U0001fb32', '\U0001fb34', '\u2588'},
	{'\U0001fb00', '\U0001fb04', '\U0001fb23', '\U0001fb32', '\U0001fb3a', '\u2588'},
	{'\U0001fb00', '\U0001fb08', '\U0001fb0a', '\U0001fb0e', '\U0001fb1d', '\u2588'},
	{'\U0001fb00', '\U0001fb08', '\U0001fb0a', '\U0001fb0e', '\U0001fb2c', '\u2588'},
	{'\U0001fb00', '\U0001fb08', '\U0001fb0a', '\U0001fb19', '\U0001fb1d', '\u2588'},
	{'\U0001fb00', '\U0001fb08', '\U0001fb0a', '\U0001fb19', '\U0001fb38', '\u2588'},
	{'\U0001fb00', '\U0001fb08', '\U0001fb0a', '\U0001fb28', '\U0001fb2c', '\u2588'},
	{'\U0001fb00', '\U0001fb08', '\U0001fb0a', '\U0001fb28', '\U0001fb38', '\u2588'},
	{'\U0001fb00', '\U0001fb08', '\U0001fb0c', '\U0001fb0e', '\U0001fb1d', '\u2588'},
	{'\U0001fb00', '\U0001fb08', '\U0001fb0c', '\U0001fb0e', '\U0001fb2c', '\u2588'},
}

// segCubeBits maps every BLOCK SEXTANT glyph (U+1FB00..U+1FB3B) and the FULL BLOCK
// (U+2588) to a 6-bit mask of its filled sub-blocks (bit i = cube i+1). Derived from
// each glyph's Unicode name (BLOCK SEXTANT-<cubes>) — note the codepoints are NOT
// enumerated in mask order, so this is a lookup rather than a codepoint formula. It
// covers the whole sextant alphabet, so a newly appended segPaths row needs no new
// entry here.
var segCubeBits = map[rune]uint8{
	'\u2588':     0b111111, // FULL BLOCK
	'\U0001fb00': 0b000001,
	'\U0001fb01': 0b000010,
	'\U0001fb02': 0b000011,
	'\U0001fb03': 0b000100,
	'\U0001fb04': 0b000101,
	'\U0001fb05': 0b000110,
	'\U0001fb06': 0b000111,
	'\U0001fb07': 0b001000,
	'\U0001fb08': 0b001001,
	'\U0001fb09': 0b001010,
	'\U0001fb0a': 0b001011,
	'\U0001fb0b': 0b001100,
	'\U0001fb0c': 0b001101,
	'\U0001fb0d': 0b001110,
	'\U0001fb0e': 0b001111,
	'\U0001fb0f': 0b010000,
	'\U0001fb10': 0b010001,
	'\U0001fb11': 0b010010,
	'\U0001fb12': 0b010011,
	'\U0001fb13': 0b010100,
	'\U0001fb14': 0b010110,
	'\U0001fb15': 0b010111,
	'\U0001fb16': 0b011000,
	'\U0001fb17': 0b011001,
	'\U0001fb18': 0b011010,
	'\U0001fb19': 0b011011,
	'\U0001fb1a': 0b011100,
	'\U0001fb1b': 0b011101,
	'\U0001fb1c': 0b011110,
	'\U0001fb1d': 0b011111,
	'\U0001fb1e': 0b100000,
	'\U0001fb1f': 0b100001,
	'\U0001fb20': 0b100010,
	'\U0001fb21': 0b100011,
	'\U0001fb22': 0b100100,
	'\U0001fb23': 0b100101,
	'\U0001fb24': 0b100110,
	'\U0001fb25': 0b100111,
	'\U0001fb26': 0b101000,
	'\U0001fb27': 0b101001,
	'\U0001fb28': 0b101011,
	'\U0001fb29': 0b101100,
	'\U0001fb2a': 0b101101,
	'\U0001fb2b': 0b101110,
	'\U0001fb2c': 0b101111,
	'\U0001fb2d': 0b110000,
	'\U0001fb2e': 0b110001,
	'\U0001fb2f': 0b110010,
	'\U0001fb30': 0b110011,
	'\U0001fb31': 0b110100,
	'\U0001fb32': 0b110101,
	'\U0001fb33': 0b110110,
	'\U0001fb34': 0b110111,
	'\U0001fb35': 0b111000,
	'\U0001fb36': 0b111001,
	'\U0001fb37': 0b111010,
	'\U0001fb38': 0b111011,
	'\U0001fb39': 0b111100,
	'\U0001fb3a': 0b111101,
	'\U0001fb3b': 0b111110,
}

// cubeBits returns the filled-cube mask for a braille glyph (ok=false for any rune
// outside the sextant alphabet). It powers TestSegPathsMonotonic's single-cube-per-
// step contract without hand-maintaining per-path data.
func cubeBits(r rune) (uint8, bool) {
	m, ok := segCubeBits[r]
	return m, ok
}

// segmentGrid tracks the terminal state of every NZB segment so the matrix can
// be redrawn each frame. Statuses are written concurrently (one atomic per
// segment) and read only by the single render goroutine.
type segmentGrid struct {
	st      []atomic.Int32 // 0 pending, 1 present, 2 damaged
	n       int
	done    atomic.Int64
	damaged atomic.Int64
	color   bool
	seed    uint32 // per-run salt for pathIndex's random path assignment

	// Per-cell fill-in animation. Only ever touched by the render goroutine
	// (matrixLines runs while p.mu is held), so plain fields — no atomics needed.
	// Indexed by layout cell index. -1 fill means "not rendered yet this layout".
	cellFill   []int8  // resting fill count (cubes) reported last frame
	animFrom   []int8  // fill count the in-progress animation began from
	animStartN []int64 // monotonic ns the animation began (0 = settled)
	layout     int     // cellCount the anim slices were sized for (width*rows)
}

// pathIndex assigns cell a segPaths entry pseudo-randomly from g.seed. It is a pure
// function of (cell, seed), so the choice is identical on every frame of a run (a
// cell never flickers) yet scatters neighbours and varies between runs (newGridSeed
// reseeds per run). Returns 0 when the pool is empty (fullSegPath fallback).
func (g *segmentGrid) pathIndex(cell int) uint32 {
	if len(segPaths) == 0 {
		return 0
	}
	h := uint64(cell)*0x9E3779B97F4A7C15 + uint64(g.seed)*0xC2B2AE3D27D4EB4F
	h ^= h >> 29
	h *= 0xBF58476D1CE4E5B9
	h ^= h >> 32
	return uint32(h % uint64(len(segPaths)))
}

// path returns the fill path assigned to a cell, falling back to fullSegPath when
// the pool is empty.
func (g *segmentGrid) path(cell int) segPath {
	if len(segPaths) == 0 {
		return fullSegPath
	}
	return segPaths[g.pathIndex(cell)]
}

// newGridSeed returns a random seed for scrambling path selection across runs, or 0
// if the system entropy source is unavailable. Test grids are built with a zero
// seed for deterministic rendering.
func newGridSeed() uint32 {
	var b [4]byte
	if _, err := crand.Read(b[:]); err != nil {
		return 0
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// set records a segment's outcome (first result wins).
func (g *segmentGrid) set(i int, ok bool) {
	if i < 0 || i >= g.n || !g.st[i].CompareAndSwap(0, boolCode(ok)) {
		return
	}
	if ok {
		g.done.Add(1)
	} else {
		g.damaged.Add(1)
	}
}

func boolCode(ok bool) int32 {
	if ok {
		return 1
	}
	return 2
}

// Layout tuning for the progress renderer. When there are many rows they are
// packed into a compact multi-column grid sized to the terminal width;
// otherwise a single rich line per row is used.
const (
	richBarW     = 26                                                 // bar width in single-column (rich) mode
	compactLblW  = 4                                                  // connection label width in grid cells ("c12")
	compactBarW  = 10                                                 // bar width inside a grid cell
	compactRateW = 7                                                  // "18.2M/s"-style throughput width in a grid cell
	compactCellW = compactLblW + (compactBarW + 2) + 1 + compactRateW // = 24
	cellGap      = 1                                                  // spaces between grid columns
	pitch        = compactCellW + cellGap
	maxColumns   = 3 // never exceed this many side-by-side columns
	gridMinRows  = 6 // only go multi-column at/above this many rows
)

// UI carries output preferences and destinations. Logging is safe for
// concurrent use. When a Progress renderer is active (a live in-place block on
// a terminal), static log lines are routed through it (emit) so they insert
// above the block instead of smearing it; otherwise they go straight to logw.
type UI struct {
	Verbose bool
	quiet   bool
	color   bool
	logw    io.Writer
	barw    io.Writer

	mu     sync.Mutex // guards active
	active *Progress  // live renderer that static lines route through, if any
}

// New builds a UI. When quiet is true, all output (logs and bars) is
// suppressed. Color is enabled only when the output is a terminal and NO_COLOR
// is unset.
func New(verbose, quiet bool) *UI {
	return &UI{
		Verbose: verbose,
		quiet:   quiet,
		color:   isTTY(os.Stderr) && os.Getenv("NO_COLOR") == "",
		logw:    os.Stderr,
		barw:    os.Stdout,
	}
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// emit writes a single static log line. If a live Progress is active on a
// terminal, it inserts the line above the in-place block (so it cannot smear
// the redraw); otherwise it writes straight to the log stream.
func (u *UI) emit(s string) {
	u.mu.Lock()
	p := u.active
	u.mu.Unlock()
	if p != nil {
		p.Line(s)
		return
	}
	fmt.Fprintln(u.logw, s)
}

// attach/detach register the live renderer that static lines route through.
// detach only clears the pointer if it still points at p (a later renderer may
// have replaced it).
func (u *UI) attach(p *Progress) {
	u.mu.Lock()
	u.active = p
	u.mu.Unlock()
}

func (u *UI) detach(p *Progress) {
	u.mu.Lock()
	if u.active == p {
		u.active = nil
	}
	u.mu.Unlock()
}

// Printf always prints (unless quiet). Use for important milestones.
func (u *UI) Printf(format string, a ...any) {
	if u.quiet {
		return
	}
	u.emit(fmt.Sprintf(format, a...))
}

// VPrintf prints only in verbose mode (unless quiet).
func (u *UI) VPrintf(format string, a ...any) {
	if u.quiet || !u.Verbose {
		return
	}
	u.emit(fmt.Sprintf(format, a...))
}

// label renders a centered, fixed-width, colored "[Name]" tag so columns line
// up. Text is centered within the bracket (extra space, if odd, goes right). The
// width is the longest stage tag ("Connection"), so that one fits with no padding.
func (u *UI) label(name string) string {
	const w = 10
	if len(name) > w {
		name = name[:w]
	}
	pad := w - len(name)
	left := pad / 2
	tag := "[" + strings.Repeat(" ", left) + name + strings.Repeat(" ", pad-left) + "]"
	if u.color {
		return ansiBold + ansiCyan + tag + ansiReset
	}
	return tag
}

// Style selects the color of a tagged output line's text so that color carries
// meaning instead of decorating every "successful" line. Every tagged line goes
// through Line below, so they all share one column/spacing/tag treatment; only the
// text hue varies. The tag itself is always the bold-cyan stage tag.
type Style int

const (
	StyleInfo    Style = iota // plain text, cyan tag — a routine milestone
	StyleSuccess              // green text — a run/step that finished successfully
	StyleWarn                 // yellow text — a non-fatal problem worth noticing
	StyleError                // red text — a failure (surfaces even under -quiet)
)

// lineColor returns the ANSI prefix for a style's text (empty when color is off or
// for StyleInfo, which stays plain). Kept separate from emit so the quiet/verbose
// gating in Line stays obvious.
func (u *UI) lineColor(s Style) string {
	if !u.color {
		return ""
	}
	switch s {
	case StyleSuccess:
		return ansiGreen
	case StyleWarn:
		return ansiYellow
	case StyleError:
		return ansiRed
	}
	return "" // StyleInfo: no color, just the cyan tag
}

// Line is the single funnel for every tagged "[ Stage ] ..." output line: an
// aligned, colored stage tag followed by fields. One code path guarantees the tag
// column and spacing stay consistent across the whole run, and Style keeps color
// meaningful (plain normal steps, green success, yellow warn, red error). Hidden
// under -quiet except StyleError, which always surfaces because failures matter.
func (u *UI) Line(s Style, name, fields string) {
	if u.quiet && s != StyleError {
		return
	}
	if c := u.lineColor(s); c != "" {
		fields = c + fields + ansiReset
	}
	u.emit(u.label(name) + " " + fields)
}

// Linef is Line with printf-style field formatting.
func (u *UI) Linef(s Style, name, format string, a ...any) {
	u.Line(s, name, fmt.Sprintf(format, a...))
}

// VLine is Line gated on verbose mode (in addition to quiet) — for per-item
// detail lines that would otherwise clutter normal runs.
func (u *UI) VLine(s Style, name, fields string) {
	if u.quiet || !u.Verbose {
		return
	}
	u.Line(s, name, fields)
}

// logo is the fixed-width ASCII wordmark, drawn from quadrant/block glyphs so the
// shapes line up on any monospaced terminal. Shown once at the very top of a run.
const logo = `               ▂▂▂▂▂▂▂  ▂▂  ▂▂▂▂▂▂▂▂   ▂▂▂▂▂▂▂▂   ▂▂▂▂▂▂ .
              ▐█▊  ▂▂▂  ▄▄ ██▊ █▊ ██▊ ██▊ █▊ ██▊ ██  ▃▂█▎
              ▐██▇▇▇██▊ ██ ██▊ █▊ ██▊ ██▊ █▊ ██▊ ██▆▆▆▆▆▆
                    ██▊                               -m|d`

// Logo prints the wordmark banner once, as the very first output of a run (before
// any stage line). Hidden under -quiet. Rendered dim so it reads as a soft
// watermark above the crisp, colored stage lines that follow.
func (u *UI) Logo() {
	if u.quiet {
		return
	}
	for _, ln := range strings.Split(logo, "\n") {
		if u.color {
			ln = ansiDim + ln + ansiReset
		}
		u.emit(ln)
	}
}

// Tag returns the rendered centered "[ Stage ]" tag string (ANSI-colored when
// enabled). Useful for callers that want a progress-row label to read as a stage.
func (u *UI) Tag(name string) string { return u.label(name) }

// Progress draws one status line per row, updating in place on a terminal with
// a per-line speed indicator. On a non-terminal (or quiet) it is inert: Add
// still records counters but nothing is drawn, keeping piped output clean.
type Progress struct {
	u      *UI
	rows   []*prow
	width  int
	labelW int
	active bool // render goroutine is running
	freeze bool // when stopping, leave the last frame on screen (don't erase)
	frame  int64
	grid   *segmentGrid // optional per-segment matrix (nil when not requested)
	stop   chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	drawn  int
	start  time.Time
}

type prow struct {
	mu    sync.Mutex
	label string
	total int64 // estimated full-size bytes (0 => count/spinner mode)
	cur   atomic.Int64
	// speed sampling, touched only by the redraw goroutine:
	last  int64
	lastT time.Time
	rate  float64
}

// NewProgress starts a renderer with one line per label. perRowTotal is the
// approximate byte total each row is expected to reach (0 for unknown/count
// mode). The render loop only starts once there is something to draw (rows or a
// matrix) via ensureRunning, so it is safe to build one before initializing a
// matrix.
func (u *UI) NewProgress(labels []string, perRowTotal int64) *Progress {
	p := &Progress{u: u, width: 26, start: time.Now()}
	for _, l := range labels {
		if len(l) > p.labelW {
			p.labelW = len(l)
		}
		p.rows = append(p.rows, &prow{label: l, total: perRowTotal, lastT: time.Now()})
	}
	p.ensureRunning()
	return p
}

// NewMatrixProgress starts a long-lived renderer whose only content is the
// per-segment matrix for totalSegs segments. A caller adds/replaces bar rows per
// phase with SetRows (and DropRows between phases) without ever erasing the
// matrix, and calls Leave at the end of the run to freeze it on screen. On a
// non-terminal or quiet UI it stays inert (Add/MarkSeg/Sweep still record).
func (u *UI) NewMatrixProgress(totalSegs int) *Progress {
	p := u.NewProgress(nil, 0)
	p.InitMatrix(totalSegs)
	p.ensureRunning()
	return p
}

// ensureRunning starts the render loop the first time there is content to show
// on a terminal, and registers p as the renderer that static lines route
// through. It is idempotent and a no-op when inert (quiet / non-TTY / empty).
func (p *Progress) ensureRunning() {
	if p == nil || p.active || p.u.quiet || !isTTY(p.u.barw) {
		return
	}
	if len(p.rows) == 0 && p.grid == nil {
		return
	}
	p.active = true
	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	p.u.attach(p)
	go p.loop()
}

// SetRows replaces the bar rows (used when a phase begins, e.g. the download
// or extract line). The matrix, if any, is preserved and keeps drawing below.
func (p *Progress) SetRows(labels []string, perRowTotal int64) {
	if p == nil {
		return
	}
	rows := make([]*prow, 0, len(labels))
	for _, l := range labels {
		if len(l) > p.labelW {
			p.labelW = len(l)
		}
		rows = append(rows, &prow{label: l, total: perRowTotal, lastT: time.Now()})
	}
	p.mu.Lock()
	p.rows = rows
	p.mu.Unlock()
	p.ensureRunning()
	p.repaint()
}

// DropRows removes all bar rows while keeping the matrix and the render loop
// alive (used between phases so only the matrix remains on screen).
func (p *Progress) DropRows() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.rows = nil
	p.mu.Unlock()
	p.repaint()
}

// Line prints a static log line safely around the live block: it clears the
// block, writes the line, then redraws the block below it. When the renderer is
// not active it just writes to the log stream.
func (p *Progress) Line(s string) {
	if p == nil {
		return
	}
	if !p.active {
		fmt.Fprintln(p.u.logw, s)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	fmt.Fprintln(p.u.barw, s)
	p.redrawLocked()
}

// Add advances row i by n bytes. Safe for concurrent use.
func (p *Progress) Add(i int, n int64) {
	if p == nil || i < 0 || i >= len(p.rows) {
		return
	}
	p.rows[i].cur.Add(n)
}

// SetLabel relabels row i (e.g. a stage change within one bar).
func (p *Progress) SetLabel(i int, label string) {
	if p == nil || i < 0 || i >= len(p.rows) {
		return
	}
	r := p.rows[i]
	r.mu.Lock()
	r.label = label
	r.mu.Unlock()
	if len(label) > p.labelW {
		p.labelW = len(label)
	}
}

// InitMatrix enables the per-message matrix for a download of total segments.
// The matrix is drawn (one glyph per cell — a single message, or a small equal
// group of messages on very large files) beneath the status rows and densified as
// messages are marked resolved. Safe to skip when there is no terminal (the
// renderer stays inert; MarkSeg simply records).
func (p *Progress) InitMatrix(total int) {
	if p == nil || total <= 0 {
		return
	}
	g := &segmentGrid{
		st:    make([]atomic.Int32, total),
		n:     total,
		color: p.u.color,
		seed:  newGridSeed(),
	}
	p.mu.Lock()
	p.grid = g
	p.mu.Unlock()
}

// MarkSeg records the outcome of segment i (ok = fully placed, otherwise it is
// left as a repairable gap and flagged red in the matrix).
func (p *Progress) MarkSeg(i int, ok bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	g := p.grid
	p.mu.Unlock()
	if g != nil {
		g.set(i, ok)
	}
}

// cellFillCubes maps a cell's landed-message count to a fill level in cubes:
// 0 (empty) .. segPathLen (full). A fully-present cell is segPathLen (the solid
// block); an empty cell is 0 (the dot); a partial cell is 1..segPathLen-1 so it
// never fakes completion. A cell holding up to segPathLen messages uses one cube
// per message (cubes == present); a larger grouped cell (huge files) scales the
// count so one cube stands for a group of messages.
func cellFillCubes(present, size int) int {
	switch {
	case present <= 0:
		return 0
	case present >= size:
		return segPathLen
	case size <= segPathLen:
		return present
	default:
		return present*(segPathLen-1)/size + 1
	}
}

// glyphCubes renders a fill level (0..segPathLen) as a dim glyph: a faint dot for
// 0, the full block for segPathLen, otherwise the cell's assigned path so the exact
// braille shape varies per cell while the *density* still reads as the fill count.
// Empty, partial and full cells share the same dim intensity so filling reads as
// densification, never a brightness change.
func (g *segmentGrid) glyphCubes(cubes, cellIdx int) string {
	var gl string
	switch {
	case cubes <= 0:
		gl = segEmptyGlyph
	case cubes >= segPathLen:
		gl = segDoneGlyph
	default:
		gl = string(g.path(cellIdx)[cubes-1])
	}
	if g.color {
		return ansiDim + gl + ansiReset
	}
	return gl
}

// cellStatus reports how many of segments [lo,hi) have landed and whether any is a
// (repairable) gap.
func (g *segmentGrid) cellStatus(lo, hi int) (present int, damaged bool) {
	for i := lo; i < hi; i++ {
		switch g.st[i].Load() {
		case 2:
			damaged = true
		case 1:
			present++
		}
	}
	return present, damaged
}

// cellGlyph returns the resting glyph for matrix cell cellIdx spanning segments
// [lo,hi) — no animation. A gap anywhere in the cell wins (light red); otherwise the
// cell renders its resting fill level via glyphCubes. This is the settled state a
// cell shows once its fill-in animation has finished; see animGlyph for the rendered
// (possibly mid-animation) glyph used by the frame loop.
func (g *segmentGrid) cellGlyph(lo, hi, cellIdx int) string {
	size := hi - lo
	if size <= 0 {
		return " "
	}
	present, damaged := g.cellStatus(lo, hi)
	if damaged {
		if g.color {
			return ansiBright + segDamagedGlyph + ansiReset
		}
		return segDamagedPlain
	}
	return g.glyphCubes(cellFillCubes(present, size), cellIdx)
}

// animGlyph is the glyph the frame loop draws for a cell. It behaves like cellGlyph
// except that when a cell's fill count *increases* (a message just landed) it does
// not pop straight to the denser glyph: instead it densifies along the cell's path
// from its previous fill count up to the new one over segAnimDuration, so each
// landed message reads as a little "filling in" animation — the magic that hides the
// instant 0->1 transition. The very first frame a cell is seen snaps to its resting
// state (no animation), and any decrease (e.g. a resize regrouping the layout) snaps
// too. nowN is a monotonic nanosecond timestamp (time.Now().UnixNano()).
//
// It is called only from matrixLines while p.mu is held, so the per-cell animation
// bookkeeping needs no locking.
func (g *segmentGrid) animGlyph(nowN int64, lo, hi, cellIdx int) string {
	size := hi - lo
	if size <= 0 {
		return " "
	}
	present, damaged := g.cellStatus(lo, hi)
	if damaged {
		g.animStartN[cellIdx] = 0 // cancel any in-flight fill; damage is terminal
		if g.color {
			return ansiBright + segDamagedGlyph + ansiReset
		}
		return segDamagedPlain
	}
	resting := cellFillCubes(present, size)
	prev := int(g.cellFill[cellIdx])
	switch {
	case prev < 0: // first sight this layout: snap, do not animate
		g.cellFill[cellIdx] = int8(resting)
		g.animStartN[cellIdx] = 0
		return g.glyphCubes(resting, cellIdx)
	case resting > prev: // a message landed: begin the fill-in
		g.animFrom[cellIdx] = int8(prev)
		g.animStartN[cellIdx] = nowN
		g.cellFill[cellIdx] = int8(resting)
	case resting < prev: // state thinned (resize/regroup): snap
		g.animStartN[cellIdx] = 0
		g.cellFill[cellIdx] = int8(resting)
	}
	if s := g.animStartN[cellIdx]; s != 0 {
		elapsed := nowN - s
		if elapsed >= int64(segAnimDuration) {
			g.animStartN[cellIdx] = 0
			return g.glyphCubes(resting, cellIdx)
		}
		span := resting - int(g.animFrom[cellIdx])
		if span < 1 {
			span = 1
		}
		cur := int(g.animFrom[cellIdx]) + int(elapsed*int64(span)/int64(segAnimDuration))
		if cur > resting {
			cur = resting
		}
		return g.glyphCubes(cur, cellIdx)
	}
	return g.glyphCubes(resting, cellIdx)
}

// resetAnim (re)sizes the per-cell animation bookkeeping for a cellCount-wide layout,
// marking every cell unseen so the next frame snaps rather than animating. Called
// whenever the matrix layout (width*rows) changes.
func (g *segmentGrid) resetAnim(cellCount int) {
	g.cellFill = make([]int8, cellCount)
	for i := range g.cellFill {
		g.cellFill[i] = -1
	}
	g.animFrom = make([]int8, cellCount)
	g.animStartN = make([]int64, cellCount)
	g.layout = cellCount
}

func (p *Progress) loop() {
	defer close(p.done)
	tick := time.NewTicker(120 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-p.stop:
			p.redraw()
			if !p.freeze {
				p.clear()
			} else {
				fmt.Fprintln(p.u.barw) // move cursor below the frozen frame
			}
			return
		case <-tick.C:
			p.redraw()
		}
	}
}

// repaint redraws only when the loop is active (used by state-changing calls so
// they are inert until the renderer is running).
func (p *Progress) repaint() {
	if p.active {
		p.redraw()
	}
}

func (p *Progress) redraw() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.redrawLocked()
}

// redrawLocked renders one frame. The caller must hold p.mu.
func (p *Progress) redrawLocked() {
	p.clearLocked()
	p.frame++
	now := time.Now()
	n := len(p.rows)

	var lines []string
	cols := layoutCols(termWidth(p.u.barw), n)
	if cols <= 1 {
		// Single-column rich layout: full detail per line.
		for _, r := range p.rows {
			lines = append(lines, p.renderRow(r, now, p.frame))
		}
	} else {
		// Multi-column compact grid, filled column-major (top-to-bottom, then
		// the next column) so connection order reads naturally down each column.
		rowsPerCol := (n + cols - 1) / cols
		for rr := 0; rr < rowsPerCol; rr++ {
			var b strings.Builder
			for c := 0; c < cols; c++ {
				if c > 0 {
					b.WriteString(strings.Repeat(" ", cellGap))
				}
				idx := c*rowsPerCol + rr
				if idx < n {
					b.WriteString(p.renderCell(p.rows[idx], now, p.frame))
				} else {
					b.WriteString(strings.Repeat(" ", compactCellW))
				}
			}
			lines = append(lines, b.String())
		}
	}

	if ml := p.matrixLines(); len(ml) > 0 {
		lines = append(lines, ml...)
	}

	fmt.Fprint(p.u.barw, strings.Join(lines, "\n"))
	p.drawn = len(lines)
}

// matrixLines renders the per-segment matrix: one braille glyph per segment,
// flowing left-to-right across the terminal width and wrapping to at most
// matrixMaxRows lines. When there are more segments than fit in that grid,
// several segments collapse into one cell (a cell reads as damaged if any of its
// segments is a gap, done if all resolved). Each row is prefixed with a dim
// "[mlo-hi]" tag naming the 1-based segment range it covers (e.g.
// "[m00001-00080]"), and its width is reserved so the row still fits the terminal.
// Returns nil when no matrix is active.
func (p *Progress) matrixLines() []string {
	g := p.grid
	if g == nil || g.n == 0 {
		return nil
	}
	term := termWidth(p.u.barw)
	if term < 1 {
		return nil
	}
	// Reserve room for the per-row "[m00001-00080] " range tag (padded to the
	// width of the largest segment index). Skip the tag on very narrow terminals.
	digits := len(fmt.Sprintf("%d", g.n))
	if digits < 4 {
		digits = 4
	}
	labelW := 2*digits + 6 // "[m" + digits + "-" + digits + "]" + trailing space
	width := term
	showLabel := term-labelW >= 16
	if showLabel {
		width = term - labelW
	}
	if width < 1 {
		return nil
	}
	// Size the grid: as many rows as needed up to matrixMaxRows; if the segment
	// count overflows that, widen each cell to a multi-segment bucket.
	rows := (g.n + width - 1) / width
	bucket := 1
	if rows > matrixMaxRows {
		rows = matrixMaxRows
		bucket = (g.n + width*matrixMaxRows - 1) / (width * matrixMaxRows)
	}
	if g.layout != width*rows {
		g.resetAnim(width * rows)
	}
	perRow := width * bucket // segments represented by one row (for its range tag)
	nowN := time.Now().UnixNano()
	var out []string
	for r := 0; r < rows; r++ {
		var b strings.Builder
		if showLabel {
			lo := r*perRow + 1
			hi := lo + perRow - 1
			if hi > g.n {
				hi = g.n
			}
			tag := fmt.Sprintf("[m%0*d-%0*d]", digits, lo, digits, hi)
			if p.u.color {
				tag = ansiCyan + tag + ansiReset
			}
			b.WriteString(tag)
			b.WriteByte(' ')
		}
		for c := 0; c < width; c++ {
			idx := r*width + c
			segLo := idx * bucket
			if segLo >= g.n {
				break
			}
			segHi := segLo + bucket
			if segHi > g.n {
				segHi = g.n
			}
			b.WriteString(g.animGlyph(nowN, segLo, segHi, idx))
		}
		out = append(out, b.String())
	}
	return out
}

// layoutCols chooses how many side-by-side columns to use for n rows given a
// terminal width. It stays single-column (rich) for small row counts or narrow
// terminals where a second column would not fit.
func layoutCols(width, n int) int {
	if n < gridMinRows {
		return 1
	}
	cols := width / pitch
	if cols > maxColumns {
		cols = maxColumns
	}
	if cols > n {
		cols = n
	}
	if cols < 2 {
		return 1
	}
	return cols
}

// sample updates and returns row r's smoothed instantaneous byte rate. Called
// once per redraw from the single render goroutine.
func (p *Progress) sample(r *prow, now time.Time) float64 {
	cur := r.cur.Load()
	if dt := now.Sub(r.lastT).Seconds(); dt > 0 {
		inst := float64(cur-r.last) / dt
		if r.rate == 0 {
			r.rate = inst
		} else {
			r.rate = 0.6*r.rate + 0.4*inst
		}
		if r.rate < 0 {
			r.rate = 0
		}
		r.last = cur
		r.lastT = now
	}
	return r.rate
}

func (p *Progress) renderRow(r *prow, now time.Time, frame int64) string {
	cur := r.cur.Load()
	rate := p.sample(r, now)

	r.mu.Lock()
	label, total := r.label, r.total
	r.mu.Unlock()
	lbl := label + strings.Repeat(" ", p.labelW-len(label))

	right := fmt.Sprintf("%9s  %8s/s  %s", human(cur), human(int64(rate)),
		time.Since(p.start).Truncate(time.Second))
	if total > 0 {
		pct := cur * 100 / total
		if pct > 100 {
			pct = 100
		}
		right = fmt.Sprintf("%3d%% %s", pct, right)
	}
	barStr := r.bar(cur, frame, richBarW)
	if p.u.color {
		return ansiDim + lbl + ansiReset + " " + barStr + " " + right
	}
	return lbl + " " + barStr + " " + right
}

// renderCell draws a row for the compact multi-column grid. The visible width
// is exactly compactCellW (ANSI codes wrap only the label, which keeps its
// on-screen width, so grid columns stay aligned).
func (p *Progress) renderCell(r *prow, now time.Time, frame int64) string {
	cur := r.cur.Load()
	rate := p.sample(r, now)

	r.mu.Lock()
	label := r.label
	r.mu.Unlock()
	lbl := shortLabel(label)
	if len(lbl) > compactLblW {
		lbl = lbl[:compactLblW]
	}
	lbl = pad(lbl, compactLblW)
	barStr := r.bar(cur, frame, compactBarW)
	rateStr := humanRate(int64(rate)) + "/s"
	if len(rateStr) > compactRateW {
		rateStr = rateStr[:compactRateW]
	}
	rateStr = pad(rateStr, compactRateW)
	if p.u.color {
		return ansiDim + lbl + ansiReset + barStr + " " + rateStr
	}
	return lbl + barStr + " " + rateStr
}

// bar renders the bracketed saucer of width w: proportional fill when total is
// known, or a bouncing "activity" saucer (driven by frame) when the total is
// unknown, so a line never looks frozen while a black-box step (e.g. PAR2
// verify) runs.
func (r *prow) bar(cur, frame, w int64) string {
	if r.total > 0 {
		fill := int(cur * w / r.total)
		if fill > int(w) {
			fill = int(w)
		}
		if fill < 0 {
			fill = 0
		}
		return "[" + strings.Repeat("=", fill) + strings.Repeat(" ", int(w)-fill) + "]"
	}
	phase := int(frame) % (2 * (int(w) - 1))
	if phase >= int(w)-1 {
		phase = 2*(int(w)-1) - phase
	}
	return "[" + strings.Repeat(" ", phase) + ">" + strings.Repeat(" ", int(w)-1-phase) + "]"
}

// clear moves the cursor back over the previously drawn block.
func (p *Progress) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
}

func (p *Progress) clearLocked() {
	if p.drawn == 0 {
		return
	}
	var b strings.Builder
	for i := 0; i < p.drawn; i++ {
		b.WriteString("\r\x1b[2K")
		if i < p.drawn-1 {
			b.WriteString("\x1b[1A")
		}
	}
	fmt.Fprint(p.u.barw, b.String())
	p.drawn = 0
}

// Finish stops the renderer and erases its lines so a summary can follow.
func (p *Progress) Finish() {
	if p == nil || !p.active {
		return
	}
	p.u.detach(p)
	close(p.stop)
	<-p.done
	p.active = false
}

// Leave stops the renderer but leaves its last frame (typically the matrix) on
// screen, moving the cursor just below it. Use at the end of a run to freeze the
// matrix instead of erasing it.
func (p *Progress) Leave() {
	if p == nil || !p.active {
		return
	}
	p.mu.Lock()
	p.freeze = true
	p.mu.Unlock()
	p.u.detach(p)
	close(p.stop)
	<-p.done
	p.active = false
}

// human renders a byte count with binary units.
func human(n int64) string {
	if n < 0 {
		n = 0
	}
	const k = 1024
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f := float64(n)
	for i, un := range units {
		if f < k || i == len(units)-1 {
			if i == 0 {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, un)
		}
		f /= k
	}
	return ""
}

// Human is the exported form of human, for callers formatting byte totals.
func Human(n int64) string { return human(n) }

// humanRate is a compact, space-free byte-rate formatter for narrow grid cells
// (e.g. "18.2M", "512K", "1.0G").
func humanRate(n int64) string {
	if n < 0 {
		n = 0
	}
	const k = 1024
	units := []string{"B", "K", "M", "G", "T"}
	f := float64(n)
	for i, un := range units {
		if f < k || i == len(units)-1 {
			if i == 0 {
				return fmt.Sprintf("%dB", n)
			}
			return fmt.Sprintf("%.1f%s", f, un)
		}
		f /= k
	}
	return ""
}

// shortLabel shortens a row label for grid cells: "conn 12" -> "c12". Labels
// without a space are returned unchanged.
func shortLabel(s string) string {
	if i := strings.IndexByte(s, ' '); i >= 0 {
		if i+1 < len(s) {
			return s[:1] + s[i+1:]
		}
		return s[:1]
	}
	return s
}

// pad right-pads s with spaces to width w (never truncates).
func pad(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

// termWidth returns the character width of the output, falling back to 80 when
// it cannot be determined (e.g. a pipe or non-terminal writer).
func termWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 80
	}
	if cols, _, err := term.GetSize(int(f.Fd())); err == nil && cols > 0 {
		return cols
	}
	return 80
}
