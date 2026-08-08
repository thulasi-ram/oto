package db

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EventsChannel is the single Postgres NOTIFY channel oto uses for UI events
// (SPEC §E.4). One channel, not one per org: channel names are global, cheap to
// listen on and impossible to garbage-collect, so per-tenant channels would leak.
const EventsChannel = "oto_events"

// Notify issues a NOTIFY on channel with payload, using the caller's transaction
// if there is one.
//
// Being inside the writer's transaction is binding (SPEC §E.4): a notification
// sent outside it can announce a row that never commits. Postgres queues the
// notification until COMMIT precisely so this works.
//
// The payload is deliberately tiny — `<org_id>:<seq>` — because NOTIFY has an
// 8 kB ceiling that fails the transaction when exceeded. The envelope travels
// through `ui_events`, never through the notification.
func Notify(ctx context.Context, q Querier, channel, payload string) error {
	if _, err := q.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, payload); err != nil {
		return fmt.Errorf("db: pg_notify(%s): %w", channel, err)
	}
	return nil
}

// Notification is one delivered NOTIFY.
type Notification struct {
	Channel string
	Payload string
}

// ListenerOptions tunes a Listener. The zero value is valid and uses the defaults.
type ListenerOptions struct {
	// MinBackoff and MaxBackoff bound the reconnect delay. Defaults 200ms / 30s.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// Logger receives connection-lifecycle records. Defaults to slog.Default.
	Logger *slog.Logger
	// OnConnect fires after every successful LISTEN, including reconnects.
	//
	// This is the hook that makes loss survivable: a consumer uses it to run a
	// catch-up read, because every notification issued while the socket was down
	// is gone forever. NOTIFY is not durable and has no replay.
	OnConnect func(ctx context.Context)
}

// Listener holds one dedicated connection issuing LISTEN and reconnects with
// backoff for as long as its context lives.
//
// One connection per process, per SPEC §E.4 — a listening connection cannot be
// shared with query traffic, so it is acquired from the pool and never returned
// until the listener stops.
type Listener struct {
	pool    *pgxpool.Pool
	channel string
	opts    ListenerOptions
}

// NewListener builds a Listener for one channel.
func NewListener(pool *pgxpool.Pool, channel string, opts ListenerOptions) *Listener {
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = 200 * time.Millisecond
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Listener{pool: pool, channel: channel, opts: opts}
}

// healthySession is how long a listen session must survive before we treat the
// connection as having genuinely worked and forget the accumulated backoff.
// Comfortably longer than MaxBackoff, so a flapping connection cannot reset
// itself by reconnecting briefly between failures.
const healthySession = 60 * time.Second

// Run listens until ctx is cancelled, invoking fn for every notification.
//
// fn runs on the listener goroutine and MUST NOT block: a slow handler stops the
// process hearing about anything else. Hand off to a buffered channel.
//
// Run never returns an error for a dropped connection — dropping is expected, and
// reconnecting is the job. It returns nil when ctx ends.
func (l *Listener) Run(ctx context.Context, fn func(ctx context.Context, n Notification)) error {
	if l.pool == nil {
		return errors.New("db: listener requires a pool")
	}

	backoff := l.opts.MinBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}

		started := time.Now()
		err := l.session(ctx, fn)

		// Reset the backoff after a session that actually worked. Without this
		// the delay only ever climbs: a handful of unrelated blips over a long
		// uptime pin the process at MaxBackoff for the rest of its life, and
		// every subsequent reconnect — and the catch-up read that follows it —
		// is delayed by the full 30s even though the database is healthy.
		if time.Since(started) >= healthySession {
			backoff = l.opts.MinBackoff
		}

		switch {
		case ctx.Err() != nil:
			return nil
		case err == nil:
			// A clean end without a cancelled context still means the socket is
			// gone; treat it exactly like an error and reconnect.
			err = errors.New("listen session ended")
		}

		l.opts.Logger.Warn("db: listen connection lost, reconnecting",
			"channel", l.channel, "error", err, "backoff", backoff)

		if !sleepCtx(ctx, jitter(backoff)) {
			return nil
		}
		if backoff *= 2; backoff > l.opts.MaxBackoff {
			backoff = l.opts.MaxBackoff
		}
	}
}

// session holds one connection for as long as it survives.
func (l *Listener) session(ctx context.Context, fn func(ctx context.Context, n Notification)) error {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	// pgx.Identifier quoting: the channel name is a constant in this codebase, but
	// LISTEN takes an identifier rather than a parameter, so it is quoted anyway.
	stmt := "LISTEN " + pgx.Identifier{l.channel}.Sanitize()
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	l.opts.Logger.Info("db: listening", "channel", l.channel)
	if l.opts.OnConnect != nil {
		l.opts.OnConnect(ctx)
	}

	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("wait: %w", err)
		}
		fn(ctx, Notification{Channel: n.Channel, Payload: n.Payload})
	}
}

// sleepCtx waits for d, reporting false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// jitter spreads a backoff by ±50 % so that a Postgres restart does not produce a
// synchronised reconnect stampede from every pod at the same millisecond.
//
// crypto/rand because math/rand is banned repo-wide; the cost is irrelevant next
// to the sleep it is scaling.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return d
	}
	// frac in [0,1); scale to [0.5, 1.5).
	frac := float64(binary.BigEndian.Uint64(b[:])>>11) / float64(uint64(1)<<53)
	return time.Duration(float64(d) * (0.5 + frac))
}
