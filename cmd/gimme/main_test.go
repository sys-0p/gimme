package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gimme/internal/ui"
)

func TestParseSavedNZB(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{
			name: "typical scene path (double-brace)",
			in: `Searching on NZBKing ...
   Found:    [01/19] - "x.par2" yEnc (1/1) (5.2 GB)
   Saving the NZB file ...
   SUCCESS:  The NZB file was saved as '/tmp/nzb/Sir.Squeaks-a-Lot.2019.1080p-QUACKS{{hunter2}}.nzb'
   Ending program in 0 seconds .`,
			want: "/tmp/nzb/Sir.Squeaks-a-Lot.2019.1080p-QUACKS{{hunter2}}.nzb", ok: true,
		},
		{
			name: "no results",
			in:   "Searching on NZBIndex ...\nWARNING:  No results found\n",
			want: "", ok: false,
		},
		{
			name: "not an nzb path ignored",
			in:   "saved as '/tmp/foo.txt'\n",
			want: "", ok: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSavedNZB(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("parseSavedNZB()=(%q,%v) want (%q,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestForceExecuteTarget(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "replaces active target, keeps comment and other keys",
			in: "[GENERAL]\n" +
				"# target = \"NZBGET\"\n" +
				"target = \"SABNZBD\"\n" +
				"cleanup = true\n" +
				"\n" +
				"[SABNZBD]\n" +
				"api = secret-key-123\n",
			want: "[GENERAL]\n" +
				"# target = \"NZBGET\"\n" +
				"target = \"EXECUTE\"\n" +
				"cleanup = true\n" +
				"\n" +
				"[SABNZBD]\n" +
				"api = secret-key-123\n",
		},
		{
			name: "inserts after header when only commented target exists",
			in:   "[GENERAL]\n# target = \"NZBGET\"\n\n[EXECUTE]\npath = /tmp/nzb\n",
			want: "[GENERAL]\ntarget = \"EXECUTE\"\n# target = \"NZBGET\"\n\n[EXECUTE]\npath = /tmp/nzb\n",
		},
		{
			name: "case-insensitive section and preserves indentation",
			in:   "[General]\n    target = \"NZBGET\"\n",
			want: "[General]\n    target = \"EXECUTE\"\n",
		},
		{
			name: "appends [GENERAL] when missing",
			in:   "[SABNZBD]\napi = x\n",
			want: "[SABNZBD]\napi = x\n[GENERAL]\ntarget = \"EXECUTE\"\n",
		},
		{
			name: "preserves CRLF line endings",
			in:   "[GENERAL]\r\ntarget = \"SABNZBD\"\r\n",
			want: "[GENERAL]\r\ntarget = \"EXECUTE\"\r\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(forceExecuteTarget([]byte(tc.in)))
			if got != tc.want {
				t.Fatalf("forceExecuteTarget()\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b"), make([]byte, 250), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := dirSize(dir); got != 350 {
		t.Fatalf("dirSize = %d want 350", got)
	}
	// A missing directory must yield 0 rather than crash the startup line.
	if got := dirSize(filepath.Join(dir, "nope")); got != 0 {
		t.Fatalf("dirSize(missing) = %d want 0", got)
	}
}

func TestStableCacheDir(t *testing.T) {
	a := stableCacheDir("/x/Release.One.nzb")
	b := stableCacheDir("/y/Release.Two.nzb")
	if !filepath.IsAbs(a) {
		t.Fatalf("expected absolute path, got %q", a)
	}
	if !strings.HasSuffix(a, nzbID("/x/Release.One.nzb")) {
		t.Errorf("path %q should end with the per-release id", a)
	}
	if !strings.Contains(a, "gimme") {
		t.Errorf("path %q should be namespaced under gimme", a)
	}
	if a == b {
		t.Errorf("different releases must map to different cache dirs: %q", a)
	}
	// Same base name -> same id (resume keys off the NZB filename).
	if stableCacheDir("/other/place/Release.One.nzb") != a {
		t.Errorf("same release name should map to the same cache dir")
	}
}

func TestPasswordFromName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{
			name: "double-brace escaped form (braces escaped by the shell)",
			in:   `Sir.Squeaks.H264-QUACKS\{\{FurryLegsFourInches\}\}.nzb`,
			want: "FurryLegsFourInches", ok: true,
		},
		{
			name: "double-brace plain",
			in:   "Grumpy.Cat.1080p-HONK{{correctHorse}}.nzb",
			want: "correctHorse", ok: true,
		},
		{
			name: "single brace",
			in:   "Goose.Get.Gravy.{lunchbox}.nzb",
			want: "lunchbox", ok: true,
		},
		{
			name: "last group wins",
			in:   "Wibble.{wibble}wobble{{wubble}}.nzb",
			want: "wubble", ok: true,
		},
		{
			name: "with directories",
			in:   "/home/dl/nzbs/Quackers.1987{{DoubleQuacks}}.nzb",
			want: "DoubleQuacks", ok: true,
		},
		{
			name: "no braces",
			in:   "Plain.Ol.Sandwich.1080p.nzb",
			want: "", ok: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := passwordFromName(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("passwordFromName(%q)=(%q,%v) want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestLargestDataFileSkipsPar2AndNzb(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "small.nfo"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, 2048), 0o644)
	os.WriteFile(filepath.Join(dir, "recovery.vol1+2.par2"), make([]byte, 8192), 0o644) // bigger but ignored
	got, err := largestDataFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "big.bin" {
		t.Fatalf("expected big.bin, got %q", filepath.Base(got))
	}
}

func TestUnpackPassthroughWhenNoRar(t *testing.T) {
	work := t.TempDir()
	out := t.TempDir()
	os.WriteFile(filepath.Join(work, "movie.mkv"), []byte("payload"), 0o644)
	os.WriteFile(filepath.Join(work, "recovery.par2"), []byte("parity"), 0o644)

	finalPath, err := unpack(work, out, "", "My.Release.2024", ui.New(false, true), nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(finalPath) != "My.Release.2024.mkv" {
		t.Fatalf("expected release-named output, got %q", filepath.Base(finalPath))
	}
	data, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("payload mismatch: %q", string(data))
	}
}

func TestReleaseNameFromName(t *testing.T) {
	cases := map[string]string{
		`Sir.Squeaks.S01E01.German.DL.1080p.CR.WEB.H264-QUACKS\{\{hunter2\}\}.nzb`: "Sir.Squeaks.S01E01.German.DL.1080p.CR.WEB.H264-QUACKS",
		`/dir/Movie.2024{{pw}}.nzb`: "Movie.2024",
		`Movie.2024.nzb`:            "Movie.2024",
		`Movie.{single}.nzb`:        "Movie",
		`Movie-{{pw}}.nzb`:          "Movie",
		`/path/to/Some.Release.nzb`: "Some.Release",
		`weird.name.with{{pw}}.nzb`: "weird.name.with",
	}
	for in, want := range cases {
		if got := releaseNameFromName(in); got != want {
			t.Errorf("releaseNameFromName(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRenameToReleasePreservesExtension(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "DuckyLandsAtDawn.mkv")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := renameToRelease(p, "My.Release")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "My.Release.mkv" {
		t.Fatalf("got %q", filepath.Base(got))
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("renamed file missing: %v", err)
	}
}
