// Command gimme is a minimal Usenet NZB leecher: it downloads segments from
// NNTP providers defined in config.yaml, auto-repairs corruption with PAR2,
// and unpacks RAR archives to the output path.
package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/go-newsgroups/nzb"

	"gimme/internal/config"
	"gimme/internal/downloader"
	"gimme/internal/extract"
	"gimme/internal/fetcher"
	"gimme/internal/repair"
	"gimme/internal/ui"
)

// Exit codes.
const (
	exitOK         = 0
	exitError      = 1
	exitUsage      = 2
	exitTooCorrupt = 3
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		flg         string
		out         string
		cfgPath     string
		prov        string
		rarPw       string
		work        string
		cache       string
		conns       int
		keep        bool
		verbose     bool
		quiet       bool
		dryRun      bool
		check       bool
		noPreflight bool
	)
	flag.StringVar(&flg, "f", "", "path to .nzb file (required)")
	flag.StringVar(&out, "o", "", "output directory for the final file (named after the NZB) (required)")
	flag.StringVar(&cfgPath, "c", defaultConfigPath(), "path to config.yaml (default ~/.config/gimme/config.yaml)")
	flag.StringVar(&prov, "s", "", "override provider name from config")
	flag.StringVar(&rarPw, "p", "", "password for encrypted RAR archives")
	flag.StringVar(&work, "work", "", "persistent work directory (default: $TMPDIR/gimme/<id>)")
	flag.StringVar(&cache, "cache", "", "segment cache dir for resume (default: <work>/.segments, else OS cache dir)")
	flag.IntVar(&conns, "connections", 0, "override connections per provider (e.g. if the account limits concurrency)")
	flag.BoolVar(&keep, "keep", false, "keep the work directory after completion")
	flag.BoolVar(&verbose, "v", false, "verbose logging")
	flag.BoolVar(&quiet, "q", false, "suppress progress output")
	flag.BoolVar(&dryRun, "dry-run", false, "parse NZB + config and exit (no network)")
	flag.BoolVar(&check, "check", false, "only test provider authentication, then exit")
	flag.BoolVar(&noPreflight, "no-preflight", false, "skip the single-connection auth preflight")
	flag.Usage = usage
	flag.Parse()

	// -f is needed for anything that reads an NZB (i.e. not -check); -o is only
	// needed when we actually write output (not -dry-run / -check).
	if !check && flg == "" {
		fmt.Fprintln(os.Stderr, "error: -f is required (unless -check)")
		fmt.Fprintln(os.Stderr)
		usage()
		return exitUsage
	}
	if !check && !dryRun && out == "" {
		fmt.Fprintln(os.Stderr, "error: -o is required")
		fmt.Fprintln(os.Stderr)
		usage()
		return exitUsage
	}

	cfgExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "c" {
			cfgExplicit = true
		}
	})
	// When the config path is the default (no -c given), make sure a file exists:
	// the first run writes a commented template into ~/.config/gimme/config.yaml so
	// the user can edit it instead of hitting "no such file or directory".
	if !cfgExplicit {
		created, err := config.EnsureDefault(cfgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return exitError
		}
		if created {
			fmt.Fprintf(os.Stderr, "note: no config found — wrote a default template to %s\n", cfgPath)
			fmt.Fprintf(os.Stderr, "      edit it to set your provider credentials, then re-run.\n\n")
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitError
	}
	providers := config.ActiveProviders(cfg, prov)
	if len(providers) == 0 {
		fmt.Fprintf(os.Stderr, "error: no providers configured (checked config and -s %q)\n", prov)
		return exitError
	}
	// CLI override for concurrency (helps when an account caps connections).
	if conns > 0 {
		for i := range providers {
			providers[i].Connections = conns
		}
	}

	// Verbose precedence: -q wins, then -v, else config default.
	verb := !quiet && (verbose || cfg.Defaults.Verbose)
	u := ui.New(verb, quiet)
	u.Logo()
	// First output line: confirm which config was actually loaded (the default is
	// ~/.config/gimme/config.yaml unless -c overrides it) and its schema version.
	u.Linef(ui.StyleInfo, "Config", "Read config from '%s', version:%d", cfgPath, cfg.Version)

	// Resolve the RAR password: an explicit -p wins; otherwise use one embedded
	// in the NZB filename via the scene "…{{password}}.nzb" convention. The
	// "[ Password ]" line is printed later, after "[ NZB ]", so the header
	// reads top-down in pipeline order.
	pwSource := ""
	if rarPw != "" {
		pwSource = "source: -p flag"
	} else if pw, ok := passwordFromName(flg); ok {
		rarPw = pw
		pwSource = fmt.Sprintf("Detected: %s (from filename)", pw)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool := fetcher.NewPool(providers, cfg.Defaults.MaxRetries, u)

	// -check: only validate authentication against each active provider.
	if check {
		err := pool.Preflight(ctx)
		if err != nil {
			u.Linef(ui.StyleError, "Error", "auth check: %v", err)
			if errors.Is(err, fetcher.ErrAuth) {
				u.Printf("hint: double-check username/password; note some providers")
				u.Printf("      return 502 for wrong credentials OR too many concurrent")
				u.Printf("      connections (try a smaller -connections value).")
			}
			return exitError
		}
		u.Linef(ui.StyleSuccess, "Auth", "all providers verified")
		return exitOK
	}

	raw, err := os.ReadFile(flg)
	if err != nil {
		u.Linef(ui.StyleError, "Error", "read nzb: %v", err)
		return exitError
	}
	doc, err := nzb.Parse(raw)
	if err != nil {
		u.Linef(ui.StyleError, "Error", "parse nzb: %v", err)
		return exitError
	}

	totalSegs := countSegs(doc)
	u.Linef(ui.StyleInfo, "NZB", "Files: %d, Segments: %d", len(doc.Files), totalSegs)
	if pwSource != "" {
		u.Line(ui.StyleInfo, "Password", pwSource)
	}

	if dryRun {
		for i, f := range doc.Files {
			u.Printf("  [%d/%d] %s — %d segment(s)", i+1, len(doc.Files), firstNonEmpty(f.Subject, "(no subject)"), len(f.Segments))
		}
		u.Printf("dry-run: no downloads performed")
		return exitOK
	}

	// Work directory. An explicitly passed -work is persistent (kept across
	// runs) so re-runs resume from the segment cache; only an auto-created temp
	// dir is removed on success.
	autoWork := false
	if work == "" {
		// Deterministic temp dir (not a random name) so it is easy to find,
		// reused across re-runs of the same release, and cleaned predictably.
		// Keyed by the per-release id so different NZBs never share a dir.
		work = filepath.Join(os.TempDir(), "gimme", nzbID(flg))
		autoWork = true
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "error: work dir: %v\n", err)
		return exitError
	}
	// Default cache location. With an explicit -work the cache lives beside the
	// (kept) downloads. With the default auto temp dir the cache must live in a
	// stable place, or resume would be impossible once that throwaway dir is
	// gone — this is why re-running without -work previously never resumed.
	autoCache := false
	if cache == "" {
		if autoWork {
			cache = stableCacheDir(flg)
			autoCache = true
		} else {
			cache = filepath.Join(work, ".segments", nzbID(flg))
		}
	}
	// Cleanup. An explicit -work is always kept; an auto temp dir is removed on
	// a fully successful run. The segment cache is kept whenever a run did NOT
	// fully succeed so a re-run resumes without re-downloading; the cache we
	// auto-created in a stable location is dropped on success so it lingers.
	var ok bool
	removeWork := autoWork && !keep
	defer func() {
		if removeWork {
			os.RemoveAll(work)
		}
		if ok && autoCache && !keep {
			os.RemoveAll(cache)
			return
		}
		u.Linef(ui.StyleInfo, "Resume", "work %s · cache %s (re-run the same command to resume)", work, cache)
	}()

	// Preflight: authenticate once per provider to fail fast on bad creds.
	if !noPreflight {
		if err := pool.Preflight(ctx); err != nil {
			u.Linef(ui.StyleError, "Error", "preflight: %v", err)
			if errors.Is(err, fetcher.ErrAuth) {
				u.Printf("hint: check username/password; some providers use 502 for bad")
				u.Printf("      credentials OR too many concurrent connections (try -connections 1).")
			}
			return exitError
		}
	}

	// One long-lived renderer owns the terminal for the rest of the run: the
	// per-connection bars + per-segment matrix during download, then just the
	// matrix during repair/unpack (so the segment field stays on screen the
	// whole time and static [Stage] lines insert above it instead of smearing).
	// Leave freezes the matrix on screen when the run ends on any path.
	prog := u.NewMatrixProgress(totalSegs)
	defer prog.Leave()

	// 1. Download (gap-tolerant; resumes from the segment cache when present).
	res, err := downloader.Download(ctx, doc, work, cache, pool, u, prog)
	if err != nil {
		u.Linef(ui.StyleError, "Error", "download: %v", err)
		return exitError
	}
	st := pool.Stats()
	if st.Retries.Load() > 0 {
		u.VPrintf("total retries across providers: %d", st.Retries.Load())
	}
	if res.UnderflowBytes > 0 {
		u.Linef(ui.StyleWarn, "Warning", "NZB under-declared %d file(s) by %d byte(s); padded to correct length",
			len(res.UnderflowFiles), res.UnderflowBytes)
	}

	// 2. Repair (PAR2 auto-patch + too-corrupt detection).
	rep, err := repair.VerifyAndRepair(work, res.HasGaps(), u, prog)
	if err != nil {
		u.Linef(ui.StyleError, "Error", "repair: %v", err)
		return exitError
	}
	switch rep.Status {
	case repair.StatusTooCorrupt:
		// Keep the raw downloads + segment cache so a re-run can retry the
		// stitch/repair step without re-fetching anything.
		removeWork = false
		u.Linef(ui.StyleError, "Repair", "TOO CORRUPT to repair - %s", rep.Detail)
		if res.TotalSegs < 100 {
			u.Printf("hint: this NZB has only %d segment(s) — it looks incomplete", res.TotalSegs)
			u.Printf("      or like a fake/stub. A real release lists thousands of")
			u.Printf("      segments whose bytes sum to the release size.")
		} else {
			u.Printf("hint: all %d segments arrived, but PAR2 lacks enough parity to", res.TotalSegs)
			u.Printf("      rebuild the rest — some articles may be gone from the server.")
			u.Printf("      Re-run the same command to retry without re-downloading")
			u.Printf("      (the segment cache is kept), or try a backup provider via -s.")
		}
		return exitTooCorrupt
	case repair.StatusRepaired:
		u.Linef(ui.StyleInfo, "Repair", "repaired from parity - %s", rep.Detail)
	case repair.StatusClean:
		u.Linef(ui.StyleInfo, "Repair", "all files verified complete")
	default:
		u.Linef(ui.StyleInfo, "Repair", "%s - %s", rep.Status, rep.Detail)
	}

	// 3. Unpack to output, naming the final artifact after the release.
	finalPath, err := unpack(work, out, rarPw, releaseNameFromName(flg), u, prog)
	if err != nil {
		u.Linef(ui.StyleError, "Error", "unpack: %v", err)
		return exitError
	}
	ok = true
	u.Linef(ui.StyleSuccess, "Done", "output at %s", finalPath)
	return exitOK
}

// unpack extracts RAR volumes if present, otherwise copies the single largest
// non-par2 data file (passthrough) into outDir. The final artifact is named
// after releaseName (the NZB filename minus ".nzb" and the "{{password}}"
// suffix) while preserving the payload's real file extension. outDir is treated
// as a directory and created if missing.
func unpack(work, outDir, rarPw, releaseName string, u *ui.UI, prog *ui.Progress) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	firstVol, err := extract.FindFirstVolume(work)
	if err != nil {
		return "", err
	}
	if firstVol != "" {
		u.Line(ui.StyleInfo, "Unpack", "RAR set from "+filepath.Base(firstVol))
		files, err := extract.Rar(firstVol, rarPw, outDir, u, prog)
		if err != nil {
			return "", err
		}
		if len(files) == 1 {
			return renameToRelease(files[0], releaseName)
		}
		return outDir, nil
	}
	// No RAR — passthrough the largest data file, named after the release.
	src, err := largestDataFile(work)
	if err != nil {
		return "", err
	}
	destName := filepath.Base(src)
	if releaseName != "" {
		destName = releaseName + filepath.Ext(src)
	}
	u.Line(ui.StyleInfo, "Unpack", "no RAR volumes; passing through "+filepath.Base(src))
	return extract.Passthrough(src, filepath.Join(outDir, destName))
}

// renameToRelease renames a single extracted artifact to "<releaseName><ext>",
// keeping its original extension. A no-op when releaseName is empty or the name
// already matches.
func renameToRelease(path, releaseName string) (string, error) {
	if releaseName == "" {
		return path, nil
	}
	dest := filepath.Join(filepath.Dir(path), releaseName+filepath.Ext(path))
	if filepath.Clean(dest) == filepath.Clean(path) {
		return path, nil
	}
	if err := os.Rename(path, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// releaseNameFromName derives a human-friendly release name from the NZB path:
// the base name minus ".nzb" and minus the trailing "{{password}}" scene
// suffix (everything from the first '{' onward), with dangling separators gone.
//
//	Reborn....DRiFTKiNG{{pw}}.nzb -> Reborn....DRiFTKiNG
func releaseNameFromName(path string) string {
	base := filepath.Base(path)
	if len(base) >= 4 && strings.EqualFold(base[len(base)-4:], ".nzb") {
		base = base[:len(base)-4]
	}
	base = strings.ReplaceAll(base, `\`, "") // tolerate shell/markdown escaping
	if i := strings.IndexByte(base, '{'); i >= 0 {
		base = base[:i]
	}
	return strings.TrimRight(base, " ._-+")
}

// largestDataFile returns the biggest non-par2/nzb file in dir (the payload).
func largestDataFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var best string
	var bestSize int64 = -1
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		lower := strings.ToLower(e.Name())
		if strings.HasSuffix(lower, ".par2") || strings.HasSuffix(lower, ".nzb") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Size() > bestSize {
			bestSize = info.Size()
			best = filepath.Join(dir, e.Name())
		}
	}
	if best == "" {
		return "", fmt.Errorf("no data files to output")
	}
	return best, nil
}

func countSegs(doc *nzb.NZB) int {
	n := 0
	for _, f := range doc.Files {
		n += len(f.Segments)
	}
	return n
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// nzbID derives a short stable id from an NZB path so several releases can share
// one -work directory without their segment caches (or assembled files) mixing.
func nzbID(path string) string {
	sum := sha1.Sum([]byte(filepath.Base(path)))
	return hex.EncodeToString(sum[:])[:12]
}

// defaultConfigPath returns the config file used when -c is not given:
// $XDG_CONFIG_HOME/gimme/config.yaml, else ~/.config/gimme/config.yaml. We build
// it from $HOME rather than os.UserConfigDir so the default is the same readable
// "~/.config/gimme/config.yaml" on macOS and Linux alike (UserConfigDir would put
// it under ~/Library/Application Support on macOS).
func defaultConfigPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "gimme", "config.yaml")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "gimme", "config.yaml")
	}
	return filepath.Join(".config", "gimme", "config.yaml")
}

// stableCacheDir returns a per-release cache directory in the OS user cache dir
// (e.g. ~/Library/Caches/gimme/<id> on macOS, ~/.cache/gimme/<id> on Linux). It
// is used when no -work is given so the segment cache survives the throwaway
// temp work dir and a re-run can resume; falls back to a dir beside the NZB.
func stableCacheDir(nzbPath string) string {
	id := nzbID(nzbPath)
	if base, err := os.UserCacheDir(); err == nil && base != "" {
		return filepath.Join(base, "gimme", id)
	}
	return filepath.Join(filepath.Dir(nzbPath), ".gimme-cache", id)
}

// namePwRe matches a brace group: one-or-more '{', inner content (no braces),
// then one-or-more '}'. It matches both the scene {{password}} form and a plain
// {password}.
var namePwRe = regexp.MustCompile(`\{+([^{}]+)\}+`)

// passwordFromName extracts an embedded RAR password from an NZB filename that
// follows the scene "…{{password}}.nzb" convention (also tolerating stray
// backslash-escaping and a single-brace form). The last brace group in the base
// name wins. Returns ok=false when no group is present.
func passwordFromName(path string) (string, bool) {
	base := filepath.Base(path)
	if len(base) >= 4 && strings.EqualFold(base[len(base)-4:], ".nzb") {
		base = base[:len(base)-4]
	}
	base = strings.ReplaceAll(base, `\`, "") // tolerate shell/markdown escaping
	ms := namePwRe.FindAllStringSubmatch(base, -1)
	if len(ms) == 0 {
		return "", false
	}
	pw := strings.TrimSpace(ms[len(ms)-1][1])
	if pw == "" {
		return "", false
	}
	return pw, true
}

func usage() {
	fmt.Fprintln(os.Stderr, `gimme - minimal Usenet NZB leecher

Usage:
  gimme -f <nzb> -o <output> [flags]

Required:
  -f  path to .nzb file
  -o  output directory (final file is named after the .nzb filename)

Options:
  -c            path to config.yaml (default ~/.config/gimme/config.yaml)
  -s            provider name to use/swap (overrides config default)
  -p            password for encrypted RAR archives
  -connections  override connections per provider (lower if account limits concurrency)
  -work         persistent work directory (default: $TMPDIR/gimme/<id>)
  -cache        segment cache dir (default: <work>/.segments, or the OS cache
                dir when no -work is given; cached segments are reused on a
                re-run so an interrupted/failed download resumes without refetch)
  -keep         keep the work directory after completion
  -check        only test provider authentication, then exit
  -no-preflight skip the single-connection auth preflight
  -v            verbose logging
  -q            quiet (no progress bar or logs)
  -dry-run      parse NZB + config and exit without downloading

Exit codes: 0 ok, 1 error, 2 usage, 3 output too corrupt to repair`)
}
