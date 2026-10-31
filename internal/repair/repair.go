// Package repair runs PAR2 verify/repair over a work directory and classifies
// the outcome so the caller can decide whether to proceed to unpacking.
package repair

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-newsgroups/par2"

	"gimme/internal/ui"
)

// Status is the outcome of VerifyAndRepair.
type Status int

const (
	StatusClean      Status = iota // files complete, nothing to do
	StatusRepaired                 // files repaired from parity
	StatusNoRecovery               // no par2 present, but nothing missing either
	StatusTooCorrupt               // missing/corrupt and not enough parity
)

func (s Status) String() string {
	switch s {
	case StatusClean:
		return "complete"
	case StatusRepaired:
		return "repaired"
	case StatusNoRecovery:
		return "no-parity-needed"
	case StatusTooCorrupt:
		return "too-corrupt"
	default:
		return "unknown"
	}
}

// Report summarizes the repair pass.
type Report struct {
	Status        Status
	ParFiles      []string
	RepairedFiles []string
	Detail        string
}

// IsFailure reports whether unpacking should be aborted.
func (r Report) IsFailure() bool { return r.Status == StatusTooCorrupt }

// VerifyAndRepair scans workDir for PAR2 recovery files and, if needed,
// repairs damaged/missing siblings. hadGaps indicates the download left holes
// (failed segments). Files are read/written directly in workDir. When prog is
// non-nil its bar rows are reused for the verify/repair bar (the shared matrix
// stays on screen); otherwise a private bar renderer is created.
func VerifyAndRepair(workDir string, hadGaps bool, u *ui.UI, prog *ui.Progress) (Report, error) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return Report{}, fmt.Errorf("read work dir: %w", err)
	}
	var parBlobs [][]byte
	var parNames []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if isPar2(e.Name()) {
			data, err := os.ReadFile(filepath.Join(workDir, e.Name()))
			if err != nil {
				u.VPrintf("  could not read par2 %s: %v", e.Name(), err)
				continue
			}
			parBlobs = append(parBlobs, data)
			parNames = append(parNames, e.Name())
		}
	}

	if len(parBlobs) == 0 {
		if hadGaps {
			return Report{Status: StatusTooCorrupt, Detail: "missing segments but no .par2 recovery data available"}, nil
		}
		return Report{Status: StatusNoRecovery, Detail: "no par2 present; all segments downloaded"}, nil
	}

	rs, err := par2.Parse(parBlobs...)
	if err != nil {
		return Report{}, fmt.Errorf("parse par2: %w", err)
	}

	// Build the file map keyed exactly as the recovery set names its inputs,
	// resolving each to the flat file we wrote into workDir.
	if prog == nil {
		prog = u.NewProgress([]string{"par2: verify"}, 0)
		defer prog.Finish()
	} else {
		prog.SetRows([]string{"par2: verify"}, 0) // reuse shared renderer
		defer prog.DropRows()                     // keep the matrix, drop the bar
	}
	files := map[string][]byte{}
	for _, spec := range rs.Files {
		onDisk := filepath.Join(workDir, filepath.Base(spec.Name))
		data, err := os.ReadFile(onDisk)
		if err != nil {
			continue // genuinely missing; Verify will mark its slices missing
		}
		files[spec.Name] = data
		prog.Add(0, int64(len(data)))
	}

	vres, err := rs.Verify(files)
	if err != nil {
		return Report{}, fmt.Errorf("verify: %w", err)
	}
	if vres.Complete {
		return Report{Status: StatusClean, ParFiles: parNames, Detail: "all files verified"}, nil
	}
	if !vres.Repairable {
		return Report{Status: StatusTooCorrupt, ParFiles: parNames, Detail: describe(rs, vres, files)}, nil
	}

	// Repairable: reconstruct and write back.
	prog.SetLabel(0, "par2: repair")
	repaired, err := rs.Repair(files)
	if err != nil {
		if errors.Is(err, par2.ErrNotRepairable) {
			return Report{Status: StatusTooCorrupt, ParFiles: parNames, Detail: describe(rs, vres, files)}, nil
		}
		return Report{}, fmt.Errorf("repair: %w", err)
	}
	var written []string
	for name, data := range repaired {
		out := filepath.Join(workDir, filepath.Base(name))
		if err := os.WriteFile(out, data, 0o644); err != nil {
			return Report{}, fmt.Errorf("write repaired %s: %w", out, err)
		}
		written = append(written, filepath.Base(name))
	}
	return Report{Status: StatusRepaired, ParFiles: parNames, RepairedFiles: written, Detail: describe(rs, vres, files)}, nil
}

// isPar2 matches .par2 and the numbered PAR2 volumes (.par2, vol001.par2 etc.).
func isPar2(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".par2")
}

// describe renders a human summary of a VerifyResult that aids diagnosis: total
// damaged slices, how much parity is actually available, and up to three
// examples showing each file's on-disk size vs. the size PAR2 expects (a
// mismatch means a short/long file; equal sizes with damaged slices mean the
// bytes are wrong) plus the range of damaged slice indices (tail vs scattered).
func describe(rs *par2.RecoverySet, vres *par2.VerifyResult, files map[string][]byte) string {
	slice := rs.SliceSize
	if slice == 0 {
		slice = 1
	}
	var total int
	var base uint64
	var ex []string
	for i, st := range vres.Files {
		var want uint64
		var name string
		var n uint64
		if i < len(rs.Files) {
			want = rs.Files[i].Length
			name = rs.Files[i].Name
			n = (want + slice - 1) / slice
		}
		if len(st.MissingSlices) > 0 {
			total += len(st.MissingSlices)
			if len(ex) < 3 {
				lo := st.MissingSlices[0] - int(base)
				hi := st.MissingSlices[len(st.MissingSlices)-1] - int(base)
				ex = append(ex, fmt.Sprintf("%s[have %d want %d; slices %d-%d x%d]",
					filepath.Base(name), len(files[name]), want, lo, hi, len(st.MissingSlices)))
			}
		}
		base += n
	}
	return fmt.Sprintf("%d damaged/missing slice(s), parity for %d slice(s); %s",
		total, len(rs.Recovery), strings.Join(ex, ", "))
}
