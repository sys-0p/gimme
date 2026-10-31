package repair

import (
	"os"
	"path/filepath"
	"testing"

	"gimme/internal/ui"
)

func TestVerifyAndRepair_NoParityNeeded(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte("data"), 0o644)
	rep, err := VerifyAndRepair(dir, false, ui.New(false, true), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != StatusNoRecovery {
		t.Fatalf("got status %v", rep.Status)
	}
}

func TestVerifyAndRepair_GapsWithoutParity(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte("partial"), 0o644)
	rep, err := VerifyAndRepair(dir, true, ui.New(false, true), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.IsFailure() || rep.Status != StatusTooCorrupt {
		t.Fatalf("expected too-corrupt, got %v", rep.Status)
	}
}
