// Package extract unpacks downloaded RAR volumes (RAR3/RAR5, multivolume,
// optional password) into a destination directory, and provides a passthrough
// for releases that are not RAR archives.
package extract

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nwaples/rardecode"

	"gimme/internal/ui"
)

var partRe = regexp.MustCompile(`(?i)\.part(\d+)\.rar$`)

// FindFirstVolume locates the first RAR volume in dir. It supports the modern
// .partNN.rar scheme (lowest number wins) and the classic .rar + .rNN scheme.
// Returns "" if no archive is present.
func FindFirstVolume(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var rarFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".rar") {
			rarFiles = append(rarFiles, e.Name())
		}
	}
	if len(rarFiles) == 0 {
		return "", nil
	}
	// Modern split volumes.
	type pn struct {
		name string
		n    int
	}
	var parts []pn
	var others []string
	for _, name := range rarFiles {
		if m := partRe.FindStringSubmatch(name); m != nil {
			n, _ := strconv.Atoi(m[1])
			parts = append(parts, pn{name, n})
		} else {
			others = append(others, name)
		}
	}
	if len(parts) > 0 {
		sort.Slice(parts, func(i, j int) bool { return parts[i].n < parts[j].n })
		return filepath.Join(dir, parts[0].name), nil
	}
	// Classic: the ".rar" (no .rNN) file is volume 1. All in others end .rar.
	sort.Strings(others)
	return filepath.Join(dir, others[0]), nil
}

// Rar decodes the archive whose first volume is firstVolume into destDir. It
// draws a single centered "[ Extract ]" line (running byte count + speed) that
// accumulates as files are written. When prog carries the pipeline's shared
// per-segment matrix, the line is drawn on that renderer (the matrix stays
// visible below it) and is dropped at the end so only the matrix remains;
// otherwise a private line is used and erased. Per-file results are printed as
// aligned sections (no stream smearing).
func Rar(firstVolume, password, destDir string, u *ui.UI, prog *ui.Progress) ([]string, error) {
	rc, err := rardecode.OpenReader(firstVolume, password)
	if err != nil {
		return nil, fmt.Errorf("open rar %s: %w", filepath.Base(firstVolume), err)
	}
	defer rc.Close()

	var extracted []string
	type done struct {
		name  string
		bytes int64
	}
	var files []done

	own := prog == nil
	if own {
		prog = u.NewProgress([]string{u.Tag("Extract")}, 0)
	} else {
		prog.SetRows([]string{u.Tag("Extract")}, 0) // reuse shared renderer
	}
	// The bar is torn down LAST (leaving the shared matrix on screen); the
	// per-file lines are emitted while it is still up so they insert above it
	// rather than smear the live block.
	defer func() {
		if own {
			prog.Finish()
		} else {
			prog.DropRows()
		}
	}()
	defer func() {
		for _, f := range files {
			u.VLine(ui.StyleInfo, "Extract", fmt.Sprintf("%s (%s)", f.name, ui.Human(f.bytes)))
		}
	}()

	for {
		hdr, err := rc.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return extracted, fmt.Errorf("read rar: %w", err)
		}
		if hdr.IsDir {
			continue
		}
		outPath, err := safeJoin(destDir, hdr.Name)
		if err != nil {
			return extracted, err
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return extracted, err
		}
		f, err := os.Create(outPath)
		if err != nil {
			return extracted, err
		}
		written, err := io.Copy(f, rc)
		cerr := f.Close()
		if err != nil {
			return extracted, fmt.Errorf("write %s: %w", outPath, err)
		}
		if cerr != nil {
			return extracted, cerr
		}
		prog.Add(0, written) // accumulate the running byte count / speed
		extracted = append(extracted, outPath)
		files = append(files, done{name: filepath.Base(outPath), bytes: written})
	}
	return extracted, nil
}

// Passthrough moves a single non-archive file to dest (a file path, or an
// existing directory into which the file is placed). It renames rather than
// copying whenever possible to avoid a second full rewrite of a multi-gigabyte
// file; when src and dest are on different filesystems (rename fails) it falls
// back to streaming the bytes.
func Passthrough(src, dest string) (string, error) {
	destPath := dest
	if st, err := os.Stat(dest); (err == nil && st.IsDir()) || strings.HasSuffix(dest, string(os.PathSeparator)) {
		destPath = filepath.Join(dest, filepath.Base(src))
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(src, destPath); err == nil {
		return destPath, nil // same filesystem: moved, no copy
	}
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.Create(destPath)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return "", err
	}
	return destPath, nil
}

// safeJoin joins name under root, keeping sub-directories but refusing to let an
// archive entry escape root (absolute paths are flattened; any ".." component
// or a cleaned result that resolves outside root is rejected). The containment
// check uses filepath.Rel so it stays correct for relative roots such as "."
// (filepath.Join drops a leading "./", which a naive prefix test misreads as an
// escape and would reject every path containing a separator).
func safeJoin(root, name string) (string, error) {
	orig := name
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimPrefix(name, "/") // never let an archive entry be absolute
	for _, comp := range strings.Split(name, "/") {
		if comp == ".." {
			return "", fmt.Errorf("illegal path in archive: %q", orig)
		}
	}
	out := filepath.Join(root, name) // Join also cleans the path
	if rel, err := filepath.Rel(filepath.Clean(root), out); err != nil ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("illegal path in archive: %q", orig)
	}
	return out, nil
}
