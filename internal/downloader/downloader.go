// Package downloader reassembles NZB files from Usenet segments without
// aborting on individual failures. Unlike nzb.DownloadFile (which stops at the
// first bad segment), a failed segment leaves a zero-filled gap that PAR2 can
// later repair. Placement uses the yEnc Begin/End offset for multipart posts, or
// the segment's cumulative byte offset for single-part-per-article posts. Each
// file's final length comes from the yEnc size=/end= headers, NOT the sum of the
// NZB's segment bytes (which is the larger, yEnc-encoded size and would leave a
// zero tail that PAR2 reads as damage).
package downloader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-newsgroups/nzb"
	"github.com/go-newsgroups/yenc"

	"gimme/internal/fetcher"
	"gimme/internal/ui"
)

var (
	bracketRe = regexp.MustCompile(`\[[^\]]*\]`)  // [01/10]
	tailRe    = regexp.MustCompile(`\(\d+/\d+\)`) // (1/3)
)

// FileResult is the per-file outcome of a download.
type FileResult struct {
	Name          string
	Path          string // where written in the work dir
	ExpectedBytes int64
	FailedSegs    int
	TotalSegs     int
}

// Result aggregates a whole-NZB download.
type Result struct {
	Files          []FileResult
	TotalBytes     int64
	FailedSegs     int
	TotalSegs      int
	UnderflowBytes int64    // bytes that fell outside a pre-sized buffer (NZB under-declared)
	UnderflowFiles []string // files affected by underflow
}

// HasGaps reports whether any segment failed to fetch (PAR2 may be able to fix).
func (r *Result) HasGaps() bool { return r.FailedSegs > 0 }

// task is one unit of work: fetch a segment and place it in a file buffer.
type task struct {
	fileIdx int
	segIdx  int // global flat segment index (matrix cell), 0..totalSegs-1
	seg     nzb.Segment
	offset  int64 // cumulative byte offset for single-part placement
}

// Download fetches every segment of doc's files concurrently, writes the
// reassembled (possibly gap-having) files into workDir, and returns per-file
// accounting. progress is advanced by bytes successfully placed. When cacheDir
// is non-empty, decoded segments are persisted there keyed by message-ID, so a
// re-run reuses them (no re-download) and only re-stitches + repairs.
//
// prog is a long-lived renderer shared across the whole run: Download installs
// the single "[ Download ]" status row on it, draws the per-segment matrix, then
// drops the row (leaving the matrix) on completion. When prog is nil a private
// renderer is created and disposed of here, preserving standalone use.
func Download(ctx context.Context, doc *nzb.NZB, workDir, cacheDir string, pool *fetcher.Pool, u *ui.UI, prog *ui.Progress) (*Result, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("work dir: %w", err)
	}

	// Pre-size each file buffer and compute cumulative segment offsets.
	buffers := make([][]byte, len(doc.Files))
	names := make([]string, len(doc.Files))
	offsets := make([][]int64, len(doc.Files))
	var totalBytes int64
	var totalSegs int
	for fi, f := range doc.Files {
		segs := append([]nzb.Segment(nil), f.Segments...)
		sort.SliceStable(segs, func(i, j int) bool { return segs[i].Number < segs[j].Number })

		var cum int64
		offs := make([]int64, len(segs))
		for si := range segs {
			offs[si] = cum
			cum += int64(segs[si].Bytes)
		}
		buffers[fi] = make([]byte, cum)
		offsets[fi] = offs
		names[fi] = fallbackName(f, fi)
		totalBytes += cum
		totalSegs += len(segs)
		doc.Files[fi].Segments = segs // persist sorted order for stable indexing
	}

	// Build the task queue.
	tasks := make(chan task, 1024)
	go func() {
		defer close(tasks)
		segIdx := 0
		for fi := range doc.Files {
			for si, seg := range doc.Files[fi].Segments {
				select {
				case <-ctx.Done():
					return
				case tasks <- task{fileIdx: fi, segIdx: segIdx, seg: seg, offset: offsets[fi][si]}:
				}
				segIdx++
			}
		}
	}()

	workers := workerCount(pool)
	failed := make([]int64, len(doc.Files)) // per-file failed-segment counters
	named := make([]bool, len(doc.Files))   // per-file: name locked from a yEnc header
	ends := make([]int64, len(doc.Files))   // per-file: highest byte written (true length)

	var mu sync.Mutex // guards names[]/named[]/ends[]
	cache := newSegmentCache(cacheDir)
	var resumed int64 // segments restored from cache (no network)
	var placed int64  // total bytes written to buffers (cache + network)
	start := time.Now()

	own := prog == nil
	if own {
		prog = u.NewMatrixProgress(totalSegs)
		defer prog.Leave() // standalone: freeze the matrix when this call returns
	}
	// A single aggregate "[ Download ]" line (running bytes + speed) above the
	// segment matrix — the per-connection meters added noise the matrix already
	// conveys, so every worker feeds the one row (index 0).
	prog.SetRows([]string{u.Tag("Download")}, totalBytes)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := pool.NewWorker(ctx)
			defer w.Close()
			for t := range tasks {
				if ctx.Err() != nil {
					return
				}
				n, hit, perr := fetchAndPlace(ctx, w, t, buffers[t.fileIdx], &names[t.fileIdx], &named[t.fileIdx], &mu, cache, ends)
				if perr != nil {
					failed[t.fileIdx]++
					pool.Stats().Failed.Add(1)
					prog.MarkSeg(t.segIdx, false) // gap: flagged red in the matrix
					u.VPrintf("  gap in %s seg %d: %v", names[t.fileIdx], t.seg.Number, perr)
					continue
				}
				if hit {
					atomic.AddInt64(&resumed, 1)
				}
				atomic.AddInt64(&placed, int64(n))
				prog.Add(0, int64(n)) // single aggregate download row
				prog.MarkSeg(t.segIdx, true)
				pool.Stats().Segments.Add(1)
			}
		}()
	}
	wg.Wait()
	prog.DropRows() // keep the matrix on screen; drop the download line

	res, err := collect(doc, buffers, names, ends, failed, workDir, totalBytes, totalSegs)
	if err != nil {
		return nil, err
	}
	printDownloadSummary(u, pool, res, atomic.LoadInt64(&placed), atomic.LoadInt64(&resumed), time.Since(start))
	return res, nil
}

// printDownloadSummary renders the post-download stats block: throughput,
// segment accounting, any cache resume, and per-provider totals.
func printDownloadSummary(u *ui.UI, pool *fetcher.Pool, res *Result, placed, resumed int64, dur time.Duration) {
	st := pool.Stats()
	var avg int64
	if secs := dur.Seconds(); secs > 0 {
		avg = int64(float64(placed) / secs)
	}
	u.Linef(ui.StyleInfo, "Download", "Segments: %d/%d, Failed: %d, Retries: %d",
		st.Segments.Load(), res.TotalSegs, res.FailedSegs, st.Retries.Load())
	u.Linef(ui.StyleInfo, "Download", "Size: %s, Time: %s, Avg: %s/s",
		ui.Human(placed), dur.Round(time.Millisecond), ui.Human(avg))
	if resumed > 0 {
		u.Linef(ui.StyleInfo, "Download", "Resumed: %d segment(s) from cache (no re-download)", resumed)
	}
	for _, ps := range st.PerProvider {
		segs, b := ps.Segments.Load(), ps.Bytes.Load()
		if segs == 0 && b == 0 {
			continue
		}
		u.Linef(ui.StyleInfo, "Download", "Provider %s: %d segs, %s", ps.Name, segs, ui.Human(b))
	}
}

// collect writes buffers to disk under unique names and builds the Result. Each
// file is written to its authoritative length ends[fi] (the yEnc size=/end=),
// not the pre-sized buffer length. The buffer is pre-sized from the NZB's
// segment "bytes", which is the yEnc-ENCODED size and over-counts the decoded
// payload by the escape expansion (~2-3%); left in place that zero tail makes
// the file longer than PAR2 expects, which flags *every* slice as damaged. So we
// trim the tail; if the NZB instead under-declared a file we pad with zeros so
// the length still matches and PAR2 can repair the gap.
func collect(doc *nzb.NZB, buffers [][]byte, names []string, ends []int64, failed []int64, workDir string, totalBytes int64, totalSegs int) (*Result, error) {
	res := &Result{TotalBytes: totalBytes, TotalSegs: totalSegs}
	used := map[string]bool{}
	for fi := range doc.Files {
		name := uniqueName(used, names[fi])
		used[name] = true
		path := filepath.Join(workDir, name)
		buf := buffers[fi]
		switch target := ends[fi]; {
		case target > int64(len(buf)):
			// NZB under-declared this file's size: extend with zeros so PAR2
			// sees the correct length (the missing bytes stay a repairable gap).
			pad := int(target) - len(buf)
			buf = append(buf, make([]byte, pad)...)
			res.UnderflowBytes += int64(pad)
			res.UnderflowFiles = append(res.UnderflowFiles, name)
		case target > 0 && target < int64(len(buf)):
			// Trim the over-allocated (encoded-size) tail down to the real file.
			buf = buf[:target]
		}
		if err := os.WriteFile(path, buf, 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", name, err)
		}
		res.FailedSegs += int(failed[fi])
		res.Files = append(res.Files, FileResult{
			Name:          name,
			Path:          path,
			ExpectedBytes: int64(len(buf)),
			FailedSegs:    int(failed[fi]),
			TotalSegs:     len(doc.Files[fi].Segments),
		})
	}
	return res, nil
}

// fetchAndPlace places one segment's payload into the file buffer, using the
// on-disk cache when present. It returns the number of bytes placed, whether the
// data came from cache, and an error (fetch/decode/CRC/empty) that makes the
// caller leave a gap. The yEnc header name is locked in on first sight so a
// subject-derived fallback (which can carry a trailing " yEnc" word) is fixed.
func fetchAndPlace(ctx context.Context, w *fetcher.Worker, t task, buf []byte, name *string, named *bool, mu *sync.Mutex, cache *segmentCache, ends []int64) (int, bool, error) {
	if e, ok := cache.load(t.seg.MessageID); ok {
		lockName(name, named, mu, e.Name)
		n, end := place(buf, e.Begin, t.offset, e.Data)
		setLen(ends, t.fileIdx, end, e.Size, mu)
		return n, true, nil
	}
	art, err := w.Article(t.seg.MessageID)
	if err != nil {
		return 0, false, err
	}
	part, derr := yenc.Decode([]byte(art.Body))
	if derr != nil {
		// Includes ErrCRCMismatch (corrupt post) and malformed yEnc.
		return 0, false, derr
	}
	if len(part.Data) == 0 {
		// A zero-length payload is not a real segment; treat it as a gap so the
		// byte range stays zero-filled for PAR2 and is never cached.
		return 0, false, errors.New("empty yEnc payload")
	}
	cn := lockName(name, named, mu, part.Name)
	n, end := place(buf, part.Begin, t.offset, part.Data)
	setLen(ends, t.fileIdx, end, part.Size, mu)
	cache.save(t.seg.MessageID, segEntry{Begin: part.Begin, End: part.End, Size: part.Size, Name: cn, Data: part.Data})
	return n, false, nil
}

// lockName sets *name from a yEnc/subject-derived partName the first time one
// appears (all segments of a file share it) and returns the current name.
func lockName(name *string, named *bool, mu *sync.Mutex, partName string) string {
	mu.Lock()
	defer mu.Unlock()
	if !*named && partName != "" {
		*name = cleanName(partName)
		*named = true
	}
	return *name
}

// setLen records a file's authoritative length: the largest of the byte offset
// just written (`end`) and the yEnc total file size (`size`, from size=). The
// size= header wins when known, so a file's length is correct even if the final
// segment (and therefore the largest `end`) is still missing.
func setLen(ends []int64, idx int, end, size int64, mu *sync.Mutex) {
	cand := end
	if size > cand {
		cand = size
	}
	if cand <= 0 {
		return
	}
	mu.Lock()
	if cand > ends[idx] {
		ends[idx] = cand
	}
	mu.Unlock()
}

// place copies data into buf either at the yEnc Begin offset (multipart,
// 1-based) or at the cumulative segment offset. It returns the number of bytes
// actually written (clamped to buf) and the exclusive end offset the segment
// WOULD occupy — an end past len(buf) signals the buffer was under-sized.
func place(buf []byte, begin int64, cumOffset int64, data []byte) (int, int64) {
	at := cumOffset
	if begin > 0 {
		at = begin - 1
	}
	if at < 0 || at >= int64(len(buf)) {
		return 0, at
	}
	n := copy(buf[at:], data)
	return n, at + int64(len(data))
}

func workerCount(pool *fetcher.Pool) int {
	n := pool.Workers()
	if n < 1 {
		return 1
	}
	return n
}

// fallbackName derives a usable filename from NZB metadata before any yEnc
// header is seen. Real subjects look like:  "[01/10] - release.part1.rar" (1/3)
// so we strip the wrapping quotes, the [N/M] group, and the trailing (N/M).
func fallbackName(f nzb.File, idx int) string {
	s := f.Subject
	if s != "" {
		s = strings.ReplaceAll(s, "\"", "")
		s = bracketRe.ReplaceAllString(s, "") // [01/10]
		s = tailRe.ReplaceAllString(s, "")    // (1/3)
		s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "-"))
		if s != "" {
			return cleanName(s)
		}
	}
	return fmt.Sprintf("file%03d.bin", idx+1)
}

// cleanName trims the trailing " yEnc" token that many posters append to the
// subject/header name (e.g. "movie.part1.rar yEnc"), which otherwise breaks
// extension-based detection of .rar volumes and .par2 parity files. The result
// is then sanitized for safe filesystem use.
func cleanName(name string) string {
	name = strings.TrimSpace(name)
	for len(name) >= 5 && strings.EqualFold(name[len(name)-5:], " yEnc") {
		name = strings.TrimSpace(name[:len(name)-5])
	}
	return sanitize(name)
}

// sanitize strips any path components and disallows traversal.
func sanitize(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == ".." || name == "" {
		return "unnamed.bin"
	}
	return name
}

// uniqueName appends a counter suffix if name is already taken.
func uniqueName(used map[string]bool, name string) string {
	if !used[name] {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s_%d%s", base, i, ext)
		if !used[cand] {
			return cand
		}
	}
}
