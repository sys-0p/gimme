package downloader

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/go-newsgroups/nzb"
)

func TestPlace_MultipartAndCumulative(t *testing.T) {
	buf := make([]byte, 10)
	// multipart: Begin=4 (1-based) → offset 3
	if n, end := place(buf, 4, 0, []byte("AB")); n != 2 || end != 5 || buf[3] != 'A' || buf[4] != 'B' {
		t.Fatalf("multipart placement wrong: n=%d end=%d buf=%q", n, end, buf)
	}
	// single-part: Begin=0 → cumulative offset
	buf2 := make([]byte, 10)
	if n, _ := place(buf2, 0, 5, []byte("XY")); n != 2 || buf2[5] != 'X' || buf2[6] != 'Y' {
		t.Fatalf("cumulative placement wrong: n=%d buf=%q", n, buf2)
	}
	// out-of-range offset yields zero bytes placed (a gap stays a gap)
	if n, _ := place(buf2, 0, 100, []byte("ZZ")); n != 0 {
		t.Fatalf("expected 0 bytes for out-of-range, got %d", n)
	}
}

func TestSegmentCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := newSegmentCache(dir)
	msg := "abc123@news.example"

	if _, ok := c.load(msg); ok {
		t.Fatal("expected cold miss")
	}
	want := segEntry{Begin: 4097, End: 8192, Size: 524288000, Name: "rel.part01.rar", Data: []byte("hello-payload")}
	c.save(msg, want)

	got, ok := c.load(msg)
	if !ok {
		t.Fatal("expected hit after save")
	}
	if got.Begin != want.Begin || got.End != want.End || got.Size != want.Size ||
		got.Name != want.Name || string(got.Data) != string(want.Data) {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, want)
	}
}

func TestSegmentCacheNilIsNoop(t *testing.T) {
	var c *segmentCache // nil = disabled
	if _, ok := c.load("x"); ok {
		t.Fatal("nil cache must always miss")
	}
	c.save("x", segEntry{Data: []byte("y")}) // must not panic
}

// TestFetchAndPlaceUsesCache proves resume: when a segment is already cached,
// fetchAndPlace returns it without touching the network. The worker is nil, so
// any attempt to dial would panic — reaching the returned values at all proves
// the cache short-circuited the fetch.
func TestFetchAndPlaceUsesCache(t *testing.T) {
	c := newSegmentCache(t.TempDir())
	// Size (total file length from size=) is 10 while this chunk only fills 5
	// bytes — the authoritative length must follow size=, not the bytes placed.
	c.save("m1", segEntry{Begin: 1, Size: 10, Name: "movie.mkv", Data: []byte("HELLO")})

	task := task{fileIdx: 0, seg: nzb.Segment{MessageID: "m1", Number: 1, Bytes: 5}}
	buf := make([]byte, 5)
	name := "fallback.bin"
	named := false
	ends := make([]int64, 1)
	var mu sync.Mutex

	n, hit, err := fetchAndPlace(context.Background(), nil, task, buf, &name, &named, &mu, c, ends)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hit {
		t.Fatal("expected a cache hit")
	}
	if n != 5 || string(buf) != "HELLO" {
		t.Fatalf("bad payload: n=%d buf=%q", n, buf)
	}
	if name != "movie.mkv" {
		t.Fatalf("name not locked from cache: %q", name)
	}
	if ends[0] != 10 {
		t.Fatalf("authoritative length should follow size= (10), got %d", ends[0])
	}
}

// TestCollectTrimsAndPadsToAuthoritativeLength covers the over-allocation fix:
// the pre-sized buffer uses the (larger) encoded size, so collect must trim the
// tail down to the yEnc length, and pad when the NZB under-declared.
func TestCollectTrimsAndPadsToAuthoritativeLength(t *testing.T) {
	dir := t.TempDir()
	doc := &nzb.NZB{Files: []nzb.File{
		{Segments: []nzb.Segment{{}}},
		{Segments: []nzb.Segment{{}}},
	}}
	buffers := [][]byte{
		make([]byte, 10), // pre-sized too big (encoded size)
		make([]byte, 4),  // pre-sized too small (under-declared)
	}
	copy(buffers[0], []byte("ABCDEFGHIJ"))
	copy(buffers[1], []byte("abcd"))
	names := []string{"a.rar", "b.rar"}
	ends := []int64{6, 8} // authoritative lengths from yEnc size=/end=
	failed := []int64{0, 0}

	res, err := collect(doc, buffers, names, ends, failed, dir, 14, 2)
	if err != nil {
		t.Fatal(err)
	}
	got0, _ := os.ReadFile(filepath.Join(dir, "a.rar"))
	if len(got0) != 6 || string(got0) != "ABCDEF" {
		t.Fatalf("trim failed: len=%d content=%q", len(got0), got0)
	}
	got1, _ := os.ReadFile(filepath.Join(dir, "b.rar"))
	if len(got1) != 8 || string(got1) != "abcd\x00\x00\x00\x00" {
		t.Fatalf("pad failed: len=%d content=%q", len(got1), got1)
	}
	if res.UnderflowBytes != 4 || len(res.UnderflowFiles) != 1 || res.UnderflowFiles[0] != "b.rar" {
		t.Fatalf("underflow accounting wrong: %+v", res)
	}
	if res.Files[0].ExpectedBytes != 6 || res.Files[1].ExpectedBytes != 8 {
		t.Fatalf("expected-bytes wrong: %+v", res.Files)
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"foo.part1.rar": "foo.part1.rar",
		"a/b/c.bin":     "c.bin",
		"..\\evil.exe":  "evil.exe",
		"../..":         "unnamed.bin",
		"sub/dir/file":  "file",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q)=%q want %q", in, got, want)
		}
	}
}

func TestUniqueName(t *testing.T) {
	used := map[string]bool{"a.bin": true}
	if got := uniqueName(used, "a.bin"); got != "a_2.bin" {
		t.Fatalf("got %q", got)
	}
	used["a_2.bin"] = true
	if got := uniqueName(used, "a.bin"); got != "a_3.bin" {
		t.Fatalf("got %q", got)
	}
}

func TestFallbackName(t *testing.T) {
	f := nzb.File{Subject: `"[01/10] - \"release.part1.rar\" (1/3)"`}
	if got := fallbackName(f, 0); got != "release.part1.rar" {
		t.Fatalf("fallback got %q", got)
	}
	empty := nzb.File{}
	if got := fallbackName(empty, 4); got != "file005.bin" {
		t.Fatalf("fallback got %q", got)
	}
	// A trailing " yEnc" token (common in obfuscated posts) must be stripped so
	// extension-based .rar/.par2 detection still works.
	yenc := nzb.File{Subject: `"VzNi2PVxQ8OUQrZelsvTio0jyZJzYJ.rar yEnc" (1/1)`}
	if got := fallbackName(yenc, 0); got != "VzNi2PVxQ8OUQrZelsvTio0jyZJzYJ.rar" {
		t.Fatalf("fallback yenc got %q", got)
	}
}

func TestCleanName(t *testing.T) {
	cases := map[string]string{
		"movie.part1.rar yEnc":  "movie.part1.rar",
		"movie.part1.rar yenc ": "movie.part1.rar",
		"movie.par2  yEnc":      "movie.par2",
		"no.token.here":         "no.token.here",
		"yyEnc":                 "yyEnc", // only a leading-space token counts
		" yEnc":                 "yEnc",  // trims to bare token (no extension to protect)
		"  spaced.mkv  yEnc":    "spaced.mkv",
	}
	for in, want := range cases {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q)=%q want %q", in, got, want)
		}
	}
}
