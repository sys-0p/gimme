package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeJoin(t *testing.T) {
	root := "/tmp/out"
	good, err := safeJoin(root, "sub/dir/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(good, filepath.Clean(root)+string(os.PathSeparator)) {
		t.Fatalf("escaped root: %q", good)
	}
	if _, err := safeJoin(root, "../../etc/passwd"); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
	// A relative root of "." must still allow nested entries: filepath.Join
	// drops the leading "./", which a naive prefix check misread as an escape
	// and wrongly rejected every path containing a "/".
	const scene = "Super.Scam.Me.2026.GERMAN.720p.WEB.h264-HAXE/" +
		"Super.Scam.Me.2026.GERMAN.720p.WEB.h264-HAXE.mkv"
	for _, root := range []string{".", "./", "out", "/abs/out"} {
		got, err := safeJoin(root, scene)
		if err != nil {
			t.Fatalf("root %q: unexpected reject of valid nested path: %v", root, err)
		}
		if filepath.Base(got) != "Super.Scam.Me.2026.GERMAN.720p.WEB.h264-HAXE.mkv" {
			t.Fatalf("root %q: wrong result %q", root, got)
		}
	}
	// ".." must still be rejected for relative roots too.
	if _, err := safeJoin(".", "../evil"); err == nil {
		t.Fatal("expected traversal under '.' to be rejected")
	}
	if _, err := safeJoin(".", "a/../../evil"); err == nil {
		t.Fatal("expected mid-path traversal to be rejected")
	}
}

func TestFindFirstVolume_Modern(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"set.part3.rar", "set.part1.rar", "set.part2.rar", "set.par2"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	first, err := FindFirstVolume(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(first) != "set.part1.rar" {
		t.Fatalf("got %q", filepath.Base(first))
	}
}

func TestFindFirstVolume_Classic(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"set.r00", "set.r01", "set.rar"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	first, err := FindFirstVolume(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(first) != "set.rar" {
		t.Fatalf("got %q", filepath.Base(first))
	}
}

func TestFindFirstVolume_None(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "readme.nfo"), []byte("x"), 0o644)
	first, err := FindFirstVolume(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != "" {
		t.Fatalf("expected empty, got %q", first)
	}
}

func TestPassthroughMovesWithinFS(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out", "My.Release.2024.mkv")

	got, err := Passthrough(src, dest)
	if err != nil {
		t.Fatal(err)
	}
	if got != dest {
		t.Fatalf("got %q want %q", got, dest)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "payload" {
		t.Fatalf("content wrong: %q err=%v", data, err)
	}
	// Same filesystem -> renamed, so the staged source is gone (no 2x disk).
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("expected source removed by rename, stat err=%v", err)
	}
}

func TestPassthroughIntoExistingDir(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Passthrough(src, out)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(got) != out || filepath.Base(got) != "movie.mkv" {
		t.Fatalf("expected file placed inside dir, got %q", got)
	}
}
