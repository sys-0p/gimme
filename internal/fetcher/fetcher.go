// Package fetcher manages NNTP connections across one or more providers and
// fetches articles with priority-based failover and retries. A *Conn from
// go-newsgroups/nntp is NOT safe for concurrent use, so each worker goroutine
// holds its own set of lazily-dialed connections (one per provider).
package fetcher

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/textproto"
	"sync/atomic"

	"github.com/go-newsgroups/nntp"

	"gimme/internal/config"
	"gimme/internal/ui"
)

// ErrAuth marks an authentication failure, distinguishing it from network or
// "article missing" errors so callers can give targeted guidance.
var ErrAuth = errors.New("authentication failed")

// ProviderStats holds per-provider counters.
type ProviderStats struct {
	Name     string
	Bytes    atomic.Int64
	Segments atomic.Int64
}

// Stats aggregates download counters, safe for concurrent updates.
type Stats struct {
	Bytes       atomic.Int64
	Segments    atomic.Int64 // segments fetched successfully
	Failed      atomic.Int64 // segments that failed on all providers
	Retries     atomic.Int64
	PerProvider []*ProviderStats
}

// Pool dials connections and tracks shared stats. Create one per run.
type Pool struct {
	providers []config.Provider
	retries   int
	ui        *ui.UI
	stats     *Stats
}

// NewPool builds a pool over the given (priority-ordered) providers.
func NewPool(providers []config.Provider, retries int, u *ui.UI) *Pool {
	st := &Stats{PerProvider: make([]*ProviderStats, len(providers))}
	for i, p := range providers {
		st.PerProvider[i] = &ProviderStats{Name: p.Name}
	}
	return &Pool{providers: providers, retries: retries, ui: u, stats: st}
}

// Stats returns the shared counters.
func (p *Pool) Stats() *Stats { return p.stats }

// ProviderCount is the number of providers in priority order.
func (p *Pool) ProviderCount() int { return len(p.providers) }

// Workers returns the total number of configured connections across all
// active providers, used to size the worker pool.
func (p *Pool) Workers() int {
	total := 0
	for _, pr := range p.providers {
		total += pr.Connections
	}
	if total <= 0 {
		total = 4
	}
	return total
}

// Preflight opens ONE connection per active provider (sequentially),
// authenticating and negotiating reader mode, then closes them. It surfaces the
// first error seen so callers can report a clear diagnostic (e.g. bad
// credentials) before spawning many concurrent workers. Because it uses a single
// connection per provider, it will not trip per-account connection limits.
func (p *Pool) Preflight(ctx context.Context) error {
	w := p.NewWorker(ctx)
	defer w.Close()
	var firstErr error
	for i := range p.providers {
		if err := w.ensureDial(i); err != nil {
			p.ui.VPrintf("  preflight %s: FAILED (%v)", p.providers[i].Name, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		pr := p.providers[i]
		tls := "off"
		if pr.Encryption {
			tls = "on"
		}
		p.ui.Linef(ui.StyleInfo, "Connection", "%s@%s  Port: %d, TLS: %s",
			pr.Username, pr.Host, pr.Port, tls)
	}
	return firstErr
}

// Worker is a per-goroutine fetch handle owning one connection per provider.
// Create one Worker per goroutine and Close() it when done.
type Worker struct {
	pool   *Pool
	ctx    context.Context
	conns  []*nntp.Conn
	dialed []bool
}

// NewWorker returns a Worker with lazily-dialed connections.
func (p *Pool) NewWorker(ctx context.Context) *Worker {
	return &Worker{
		pool:   p,
		ctx:    ctx,
		conns:  make([]*nntp.Conn, len(p.providers)),
		dialed: make([]bool, len(p.providers)),
	}
}

// ensureDial establishes (and authenticates) the connection for provider i if
// not already connected. AUTHINFO is sent BEFORE MODE READER, as most providers
// require authentication before granting reader mode.
func (w *Worker) ensureDial(i int) error {
	if w.dialed[i] && w.conns[i] != nil {
		return nil
	}
	p := w.pool.providers[i]
	addr := fmt.Sprintf("%s:%d", p.Host, p.Port)

	var (
		c   *nntp.Conn
		err error
	)
	if p.Encryption {
		c, err = nntp.DialTLS(w.ctx, addr, &tls.Config{ServerName: p.Host})
	} else {
		c, err = nntp.Dial(w.ctx, addr)
	}
	if err != nil {
		return fmt.Errorf("dial %s (%s): %w", p.Name, addr, err)
	}
	if p.Username != "" {
		if err := c.Authenticate(p.Username, p.Password); err != nil {
			_ = c.Close()
			return fmt.Errorf("%w for %s (%s): %w", ErrAuth, p.Name, p.Username, err)
		}
	}
	if err := c.ModeReader(); err != nil {
		_ = c.Close()
		return fmt.Errorf("mode-reader %s: %w", p.Name, err)
	}
	w.conns[i] = c
	w.dialed[i] = true
	return nil
}

// resetConn tears down the connection for provider i so it will be redialed.
func (w *Worker) resetConn(i int) {
	if w.conns[i] != nil {
		_ = w.conns[i].Close()
	}
	w.conns[i] = nil
	w.dialed[i] = false
}

// Article fetches a message-id, trying each provider in priority order. It
// retries transient failures per provider and fails over when an article is
// reported missing (430) or a connection breaks.
func (w *Worker) Article(msgID string) (*nntp.Article, error) {
	id := msgID
	if len(id) == 0 {
		return nil, errors.New("empty message id")
	}
	if id[0] != '<' {
		id = "<" + id + ">"
	}
	var lastErr error
	for i := range w.pool.providers {
		if w.ctx.Err() != nil {
			return nil, w.ctx.Err()
		}
		if err := w.ensureDial(i); err != nil {
			lastErr = err
			continue
		}
		for attempt := 0; attempt <= w.pool.retries; attempt++ {
			art, err := w.conns[i].Article(id)
			if err == nil {
				w.pool.stats.PerProvider[i].Segments.Add(1)
				w.pool.stats.PerProvider[i].Bytes.Add(int64(len(art.Body)))
				return art, nil
			}
			lastErr = err
			if isMissing(err) {
				// Not on this server — no point retrying here; fail over.
				break
			}
			if isNetwork(err) {
				// Connection likely dead — redial on the next attempt.
				w.resetConn(i)
				break
			}
			w.pool.stats.Retries.Add(1)
		}
	}
	return nil, fmt.Errorf("all providers failed for %s: %w", id, lastErr)
}

// Close releases all connections held by this worker.
func (w *Worker) Close() {
	for i := range w.conns {
		if w.conns[i] != nil {
			_ = w.conns[i].Close()
			w.conns[i] = nil
		}
	}
}

// isMissing reports NNTP responses meaning the article is simply not present
// on this server (retrying the same server is pointless — fail over instead).
func isMissing(err error) bool {
	var te *textproto.Error
	if errors.As(err, &te) {
		switch te.Code {
		case 430, 423, 401, 400:
			return true
		}
	}
	return false
}

// isNetwork reports errors that are not a parsed NNTP status, i.e. a broken
// connection rather than a protocol-level "no".
func isNetwork(err error) bool {
	var te *textproto.Error
	return !errors.As(err, &te)
}
