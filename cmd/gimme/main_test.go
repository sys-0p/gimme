package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gimme/internal/ui"
)

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
			name: "double-brace from user example (escaped form)",
			in:   `CR.WEB.H264-DRiFTKiNG\{\{HtO65TYLYUOARuNPCUk0ALjXmTJxzq\}\}.nzb`,
			want: "HtO65TYLYUOARuNPCUk0ALjXmTJxzq", ok: true,
		},
		{
			name: "double-brace plain",
			in:   "Some.Release.1080p-GRP{{HtO65TYLYUOARuNPCUk0ALjXmTJxzq}}.nzb",
			want: "HtO65TYLYUOARuNPCUk0ALjXmTJxzq", ok: true,
		},
		{
			name: "single brace",
			in:   "Release.{secret123}.nzb",
			want: "secret123", ok: true,
		},
		{
			name: "last group wins",
			in:   "Release.{first}extra{{thelast}}.nzb",
			want: "thelast", ok: true,
		},
		{
			name: "with directories",
			in:   "/home/dl/nzbs/Movie.2024{{abcXYZ}}.nzb",
			want: "abcXYZ", ok: true,
		},
		{
			name: "no braces",
			in:   "Ordinary.Release.1080p.nzb",
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
		`Reborn.S01E01.German.DL.1080p.CR.WEB.H264-DRiFTKiNG\{\{HtO65TY\}\}.nzb`: "Reborn.S01E01.German.DL.1080p.CR.WEB.H264-DRiFTKiNG",
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
	p := filepath.Join(dir, "VzNi2PVxQ8OUQrZelsvTio0jyZJzYJ.mkv")
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
