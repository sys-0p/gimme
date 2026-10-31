package downloader

import (
	"bytes"
	"crypto/sha1"
	"encoding/gob"
	"encoding/hex"
	"os"
	"path/filepath"
)

// segEntry is the cached, decoded form of one article: the yEnc byte offset the
// payload begins at (1-based; 0 means "use the cumulative segment offset"), the
// chunk's end offset, the authoritative TOTAL file size from the yEnc size=
// header (0 if absent), the file name, and the raw decoded bytes. Size/End let a
// resumed run reproduce the exact file length (NZB "bytes" is the larger,
// yEnc-encoded size and must not be used for the final length).
type segEntry struct {
	Begin int64
	End   int64
	Size  int64
	Name  string
	Data  []byte
}

// segmentCache persists decoded segments to disk, keyed by message-ID, so a
// re-run with the same cache dir skips all networking and goes straight to
// re-stitching and PAR2 repair. A nil *segmentCache is a valid no-op cache
// (caching disabled).
type segmentCache struct{ dir string }

func newSegmentCache(dir string) *segmentCache {
	if dir == "" {
		return nil
	}
	_ = os.MkdirAll(dir, 0o755)
	return &segmentCache{dir: dir}
}

func (c *segmentCache) path(msgID string) string {
	if c == nil {
		return ""
	}
	sum := sha1.Sum([]byte(msgID))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".seg")
}

// load returns a cached segment. A corrupt or partial file is treated as a miss
// (self-healing: the next successful fetch rewrites it).
func (c *segmentCache) load(msgID string) (segEntry, bool) {
	if c == nil {
		return segEntry{}, false
	}
	raw, err := os.ReadFile(c.path(msgID))
	if err != nil {
		return segEntry{}, false
	}
	var e segEntry
	if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&e); err != nil {
		return segEntry{}, false
	}
	return e, true
}

// save atomically writes a segment (tmp file then rename) so an interrupted run
// can never leave a half-written cache entry.
func (c *segmentCache) save(msgID string, e segEntry) {
	if c == nil {
		return
	}
	p := c.path(msgID)
	tmp := p + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	if err := gob.NewEncoder(f).Encode(&e); err != nil {
		f.Close()
		os.Remove(tmp)
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return
	}
	_ = os.Rename(tmp, p)
}
