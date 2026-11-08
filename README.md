# gimme

<p align="center">
  <img src="https://raw.githubusercontent.com/sys-0p/gimme/refs/heads/main/assets/gimme.jpg" />
</p>

A minimal Usenet NZB leecher. Point it at an `.nzb` (or an `nzblnk://` link): it
downloads every segment, repairs damage with PAR2, unpacks RAR, and writes the
finished file named after the release.

## Quick start

```sh
./gimme -f release.nzb -o ~/Downloads
```

`-o` is an **output directory** (created if missing); the result is named after
the `.nzb`. On first run gimme writes a config template to
`~/.config/gimme/config.yaml` — add your provider, then re-run.

## Configure

`~/.config/gimme/config.yaml` (override with `-c`):

```yaml
version: 2
defaults:
  provider: main          # used unless -s picks another
  max_retries: 3          # retries per segment before leaving a gap
providers:
  - name: main
    host: news.example.com
    port: 563
    encryption: true      # true = TLS (NNTPS)
    username: your-username
    password: "${GIMME_PW_MAIN}"   # ${ENV} / $(ENV) expanded at load time
    connections: 8
    priority: 1           # lower tried first; failed articles fail over to the next
  - name: backup
    host: news2.example.com
    port: 563
    encryption: true
    username: your-username
    password: "${GIMME_PW_BACKUP}"
    connections: 4
    priority: 2
```

## Usage

```
gimme -f <nzb> -o <dir> [flags]
gimme -n <nzblnk> -o <dir> [flags]

  -f            path to .nzb                        (required unless -n)
  -n            nzblnk:// URI, resolved via the nzbgo CLI (alternative to -f)
  -o            output directory; result named after the .nzb   (required)
  -c            config.yaml             (default ~/.config/gimme/config.yaml)
  -s            provider name to use    (overrides the config default)
  -p            password for encrypted RAR archives
  -connections  override connections per provider
  -work         persistent work dir     (default $TMPDIR/gimme/<id>)
  -cache        segment cache dir       (default OS cache dir)
  -keep-cache   keep the cache after the run, to reprocess without re-downloading
  -keep         keep the work dir after the run
  -check        test provider auth and exit
  -no-preflight skip the single-connection auth preflight
  -dry-run      parse NZB + config, no download
  -v / -q       verbose / quiet
  -nzbgo        path to the nzbgo binary                          (with -n)
  -nzbgo-config base nzbgo config to copy + force target=EXECUTE  (with -n)
```

```sh
./gimme -f release.nzb -o ~/Downloads                  # default provider
./gimme -f release.nzb -o out -s backup -p 'rar-pw'    # backup provider, locked RAR
./gimme -n "nzblnk:?t=Movie.2024&h=AbCdEf&p=hunter2&g=alt.binaries.warez&d=01.01.2000" -o ~/Downloads
```

If the `.nzb` is named `Release{{password}}.nzb`, gimme reads the RAR password
from the name when you don't pass `-p`.

## nzblnk:// links

With `-n`, gimme runs the [nzbgo](https://github.com/Tensai75/nzb-monkey-go) CLI
for you, grabs the `.nzb` it saves, and leeches it. nzbgo only saves the file
when its `target = "EXECUTE"`; gimme makes that happen by copying your nzbgo
config into a throwaway with `target` forced to `EXECUTE` and passing it as
`--config`, so your real config is never touched. By default it uses nzbgo's own
config (`~/.config/nzb-monkey-go.conf`, or `config.txt` beside the binary on
macOS); override with `-nzbgo-config`.

## Resume & cleanup

By default gimme leaves **nothing** behind: when a run ends the segment cache is
deleted, so you never end up with gigabytes of half-used `.rar` segments sitting
in `~/Library/Caches/gimme`.

To resume or reprocess **without re-downloading**, ask for it explicitly:

- `-keep-cache` retains the segment cache after the run. Re-run the same command
  and only missing segments are fetched; the rest are stitched from cache.
- `-work DIR` keeps the whole work dir (downloads + cache) beside `DIR`.
- An explicit `-cache DIR` is also kept — gimme never deletes a directory you
  named yourself.

An interrupted run (Ctrl-C, failure) keeps its cache only under those flags;
otherwise everything is cleaned up. The startup line
`[ Cache ] Current cache <path> size <x>` shows how much is already on disk.

## Exit codes

| code | meaning |
|-----:|---------|
| 0    | success |
| 1    | error (config / network / unpack) |
| 2    | usage error |
| 3    | too corrupt to repair — not enough PAR2 to rebuild the gaps |

Any non-zero exit also prints a `[ Error ]` or `[ Repair ]` line explaining what
went wrong (for exit 3: how many segments are unrecoverable and how to retry).

## Build

Needs Go (module targets `go 1.27.1`; `GOTOOLCHAIN=auto` fetches it on first build).

```sh
go build -o gimme ./cmd/gimme
go test ./...
```
