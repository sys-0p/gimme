# gimme

A minimal NZB leecher for Usenet, in Go. Downloads an `.nzb` from your NNTP
provider(s), **auto-repairs corruption with PAR2 while unpacking**, and writes
the final decompressed file(s) to disk.

```
gimme -f <path_to_nzb> -o <output>
```

## Pipeline

1. **Download** every segment across all providers, in parallel
   (`connections` per provider). A segment that fails after retries is left as a
   **gap** instead of aborting the whole download.
2. **Repair** with PAR2: verify every file against the `.par2` recovery set and
   reconstruct damaged/missing pieces automatically. If there isn't enough parity
   to fix the gaps, it reports the output as **too corrupt** and stops (exit 3).
3. **Unpack** RAR volumes (RAR3/RAR5, multi-volume, optional password) into the
   output path. Releases without RAR fall back to passing through the main file.

## Build

Requires Go (the module targets `go 1.27.1`; with `GOTOOLCHAIN=auto` the right
toolchain is fetched automatically on first build).

```sh
go build -o gimme ./cmd/gimme
go test ./...
```

## Configuration

gimme reads its config from `~/.config/gimme/config.yaml` by default (honouring
`$XDG_CONFIG_HOME` if set); point it elsewhere with `-c`. If that file does not
exist yet, the first run writes a commented default template there (and creates
the `~/.config/gimme` directory), prints where it put it, and asks you to edit
it — so you never hit a "no such file or directory" error. Edit the YAML, not the
code — the file the run actually used is echoed as the first `[ Config ]` line.

```yaml
version: 2
defaults:
  provider: main          # provider used unless -s overrides it
  max_retries: 3          # retries per segment before leaving a gap
  verbose: true
providers:
  - name: main
    host: news.example.com
    port: 563
    encryption: true      # true = implicit TLS (NNTPS), false = plaintext
    username: your-username
    password: "${GIMME_PW_MAIN}"   # ${ENV_VAR} is expanded at load time
    connections: 8
    priority: 1           # lower is tried first during failover
  - name: backup
    host: news2.example.com
    port: 563
    encryption: true
    username: your-username
    password: "${GIMME_PW_BACKUP}"
    connections: 4
    priority: 2
```

- **Failover:** if the active provider can't fetch an article (or the article
  isn't present on it), the next provider by `priority` is tried.
- **Swapping providers on the CLI:** `-s backup` runs using only the provider
  named `backup` (defined in the YAML).

## Usage

```
gimme -f <nzb> -o <output> [flags]

  -f        path to .nzb file (required)
  -o        output directory for the final file (required)
  -c        path to config.yaml          (default ~/.config/gimme/config.yaml)
  -s        provider name to use/swap    (overrides config default)
  -p        password for encrypted RAR archives
  -work     persistent work directory  (default: $TMPDIR/gimme/<id>)
  -cache    segment cache dir          (default: <work>/.segments, or OS cache dir)
  -keep     keep the (auto temp) work directory after completion
  -v        verbose logging
  -q        quiet (no progress bar or logs)
  -dry-run  parse NZB + config and exit without downloading
```

### Examples

```sh
# Fetch and unpack using the default provider
GIMME_PW_MAIN=hunter2 ./gimme -f release.nzb -o ~/Downloads/release

# Use the backup provider, RAR is password protected, keep the work dir
./gimme -f release.nzb -o out -s backup -p 'rar-password' -keep -v
```

### Output

gimme prints aligned `[Stage]` sections to stderr and a single live
`[ Download ]`/`[ Extract ]` progress line (with the segment matrix beneath it)
to stdout. The in-place line needs a terminal; when output is piped (or `-q` is
used) it is omitted and only the plain summary sections remain. Stages are
printed in pipeline order (tags are centered in a fixed-width column sized to the
longest stage name — `Connection` — so every stage lines up and none is padded):

```
[  Config  ] Read config from '~/.config/gimme/config.yaml', version:2
[   NZB    ] Files: 14, Segments: 9280
[ Password ] Detected: HtO65TY…TJxzq (from filename)
[Connection] me@news.tweaknews.eu  Port: 563, TLS: on
[ Download ] [================>       ]  63%  3.7 GiB  19.7 MiB/s  17s
[m0001-1170] ██████████████████🬝🬎🬆🬂█🬬🬄🬂·████🬎🬆···················…
[m1171-2340] ██████🬂█🬄█🬆····················…   ← █ done · · not yet fetched
[ Download ] Segments: 9280/9280, Failed: 0, Retries: 3
[ Download ] Size: 5.9 GiB, Time: 5m12.3s, Avg: 19.7 MiB/s
[ Download ] Provider main: 9280 segs, 6.1 GiB
[  Repair  ] all files verified complete
[  Unpack  ] RAR set from Cool/release.part1.rar
[ Extract  ] [====>                   ]  2.1 GiB  240 MiB/s  9s
[m0001-1170] ████████████████████████████████████████████…
[   Done   ] output at out/…paranoid06.mkv
```

The download phase is a **single** aggregate line (`percent · bytes · rate ·
elapsed`) rather than one meter per connection — with dozens of connections the
per-connection meters just added noise, and the segment matrix below already
conveys overall activity at a glance. The `[Download]` block then summarizes
segment accounting, timing, cache-resume and per-provider usage. If a run fails
partway, a red `[ Error ]` line is printed and a `[ Resume ]` line reports the
work/segment-cache directories to re-run against.

Colour carries meaning, not decoration: the stage tag is always bold cyan, the
text of routine steps (Config, NZB, Connection, Download, Repair…) is plain, the
single terminal success (`Done`, or `Auth` under `-check`) is **green**, a
`Warning` is **yellow**, and failures (`Error`, a too-corrupt `Repair`) are
**red**. Earlier every "OK" line was painted green, which highlighted nothing —
now exactly one line goes green per successful run.

**The segment matrix stays on screen for the whole run.** Each cell is one
downloaded message (a Usenet article) — or, on very large files, a small equal
group of messages so the whole set still fits in up to 8 rows; the block flows
left-to-right and wraps. Each matrix row is prefixed with a faint `[mlo-hi]` tag
naming the 1-based message numbers it shows (e.g. `[m0001-1170]`), so a row can be
read against a range instead of just position. A message lands all at once, so there is no fractional
"percent within a segment" to fake: an unfetched cell is a faint dot (`·`), and each
message that lands adds exactly **one cube** to its cell, densifying it up to a solid
block (`█`) once every message in that cell has arrived. The exact braille shape a
cell densifies through is a per-cell **fill path** (so neighbours differ visually),
assigned at random and fixed for the run — only the *density* carries meaning, so a
denser cell always means more messages landed and a cell can never flicker back (this
replaces the old random shimmer, which made it impossible to tell progress from
noise). Partial, full and empty cells all render at the *same dim intensity*, so
filling reads purely as the shape densifying, never a sudden brighten. And because a
real landing is just an instant `0→1`, the renderer adds a little **magic**: rather
than snapping from a dot to a block, a landing densifies along the cell's path over
about a second, so each arriving message visibly fills in. This is purely cosmetic —
the stored state is still that instant transition — so the matrix keeps the crisp,
meaningful fill contract while looking alive as it downloads. When the download line
finishes it drops away and the matrix remains while PAR2 verifies and the archive
unpacks. Any message that couldn't be fetched (a gap PAR2 may repair)
lights up in light red throughout, so damage is visible at a glance instead of buried
in a counter. At the end the finished matrix is left on screen.





### Password embedded in the NZB filename

Scene releases often append the RAR password to the name as `…{{password}}.nzb`.
If present and you did **not** pass `-p`, gimme extracts it automatically and
logs the source:

```sh
./gimme -f 'CR.WEB.H264-DRiFTKiNG{{HtO65TYLYUOARuNPCUk0ALjXmTJxzq}}.nzb' -o out
# [ Password  ] Detected: HtO65TYLYUOARuNPCUk0ALjXmTJxzq (from filename)
```

Precedence: explicit `-p` > filename-embedded `{{…}}`. Plain `{password}` and
backslash-escaped forms are also handled; the last brace group in the name wins.

### Output naming

`-o` is treated as an **output directory** (created if missing). The final file
is named after the NZB file — everything before the `.nzb` extension and the
`{{password}}` suffix — keeping the payload's real extension:

```sh
./gimme -f 'Reborn.…CR.WEB.H264-DRiFTKiNG{{HtO65TY…}}.nzb' -o Reborn
# → Reborn/Reborn.…CR.WEB.H264-DRiFTKiNG.mkv
```

A single extracted/passthrough file is renamed to that release name; multi-file
archives are left inside the output directory as-is.

gimme also strips the trailing ` yEnc` token that many obfuscated posts append
to article names (`…part1.rar yEnc`), so PAR2 `.par2` parity and RAR `.rar`
volumes are detected correctly.

### Resume (no re-download)

Every successfully fetched article is cached (decoded, keyed by message-ID). On a
re-run, cached segments are restored from disk and **only the missing/failed
ones are re-fetched**, so an interrupted or `TOO CORRUPT` run can be retried
without pulling everything again — just run the **same command**.

Where the cache lives (override with `-cache`):

| Invocation                        | Cache location                          |
|-----------------------------------|-----------------------------------------|
| with `-work DIR`                  | `DIR/.segments/<nzb-id>` (kept with DIR)|
| without `-work` (auto temp)       | OS cache dir, e.g. `~/Library/Caches/gimme/<nzb-id>` |

The OS-cache default is what makes resume work **without** `-work`: the temp work
dir (`$TMPDIR/gimme/<id>`) is removed after a successful run, but the cache is
not. A cache gimme created for you is removed automatically after a run that
**fully succeeds** (nothing left to resume); on any failure it is kept. An
explicit `-cache` is never auto-removed.

The final single file is **moved** (renamed) from the work dir into `-o` when
they share a filesystem, so a multi-gigabyte passthrough is not copied twice.

```sh
# Plain run: cache goes to the OS cache dir; a failure leaves it for resume.
./gimme -f release.nzb -o out
./gimme -f release.nzb -o out          # resumes; only failed segments refetched

# With an explicit work dir, the cache lives beside the (kept) downloads.
./gimme -f release.nzb -o out -work ~/nzbwork
./gimme -f release.nzb -o out -work ~/nzbwork -no-preflight   # offline re-stitch
```

Delete the cache directory (or pass a fresh `-cache`) to force a clean re-download.

### Exit codes

| code | meaning                                     |
|-----:|---------------------------------------------|
| 0    | success                                     |
| 1    | error (config / network / unpack)           |
| 2    | usage error                                 |
| 3    | output too corrupt to repair (not enough PAR2) |

## Layout

```
cmd/gimme/         CLI + pipeline orchestration
internal/config/   YAML config (versioned), env expansion, provider selection
internal/fetcher/  NNTP connection pool, per-goroutine conns, failover, stats
internal/downloader/  concurrent segment download + gap-tolerant reassembly
internal/repair/   PAR2 verify/repair + too-corrupt classification
internal/extract/  RAR multi-volume extraction + passthrough
internal/ui/       plain progress bar + leveled logging
```

## Notes & limitations

- Built on the pure-Go, CGO-free `go-newsgroups/{nntp,nzb,yenc,par2}` libraries
  and `nwaples/rardecode`.
- **PAR2 repair interop:** the Go `par2` library's `Repair` is validated by
  self-consistent round-trip, not yet byte-for-byte against `par2cmdline`. Verify
  is hash-based and reliable; if a real-world repair ever misbehaves, the
  `internal/repair` layer is intentionally thin so a `par2cmdline` fallback can
  be dropped in.
- `-o` is treated as a directory when a RAR set extracts multiple files; a
  single result is written directly to the `-o` path.
