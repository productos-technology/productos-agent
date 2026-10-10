package runner

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/productos-technology/productos-agent/internal/protocol"
)

const (
	batchMax   = 50
	batchEvery = 500 * time.Millisecond
)

// ErrRunFinished: the server says the run is over (cancelled, failed, finished elsewhere). Stop the tool.
var ErrRunFinished = errors.New("the run is finished on the server")

// Reporter delivers to the server: the daemon implements it with the channel and the REST fallback.
type Reporter interface {
	// Events appends a batch and returns the server's last_seq.
	Events(ctx context.Context, runID string, events []protocol.Event) (int, error)
	// Status reports a lifecycle move or the final result.
	Status(ctx context.Context, runID string, report protocol.StatusReport) error
}

// emitter batches events (≤ 50 or every 500 ms) and keeps them in an outbox until the server acknowledged them,
// so a dropped socket loses nothing and nothing acknowledged is ever sent twice.
type emitter struct {
	runID    string
	reporter Reporter
	log      *slog.Logger

	mu      sync.Mutex
	queue   []protocol.Event
	wake    chan struct{}
	lastSeq int
	closed  bool
	dead    error

	done chan struct{}
}

func newEmitter(runID string, reporter Reporter, log *slog.Logger) *emitter {
	e := &emitter{runID: runID, reporter: reporter, log: log, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go e.loop()
	return e
}

// Emit queues an event.
func (e *emitter) Emit(ev protocol.Event) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.queue = append(e.queue, ev)
	full := len(e.queue) >= batchMax
	e.mu.Unlock()
	if full {
		e.kick()
	}
}

func (e *emitter) kick() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Flush sends everything queued now and waits until it landed (or the run is dead). Returns the last seq.
func (e *emitter) Flush(ctx context.Context) (int, error) {
	for {
		e.mu.Lock()
		pending := len(e.queue)
		seq, dead := e.lastSeq, e.dead
		e.mu.Unlock()
		if dead != nil {
			return seq, dead
		}
		if pending == 0 {
			return seq, nil
		}
		e.kick()
		select {
		case <-ctx.Done():
			return seq, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Close flushes with a deadline and stops the loop.
func (e *emitter) Close(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, _ = e.Flush(ctx)
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	e.kick()
	<-e.done
}

// Dead reports the error that ended delivery, if any.
func (e *emitter) Dead() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dead
}

func (e *emitter) loop() {
	defer close(e.done)
	ticker := time.NewTicker(batchEvery)
	defer ticker.Stop()
	backoff := time.Second
	for {
		select {
		case <-ticker.C:
		case <-e.wake:
		}
		e.mu.Lock()
		if e.dead != nil {
			e.queue = nil
		}
		if len(e.queue) == 0 {
			if e.closed {
				e.mu.Unlock()
				return
			}
			e.mu.Unlock()
			continue
		}
		n := len(e.queue)
		if n > batchMax {
			n = batchMax
		}
		batch := make([]protocol.Event, n)
		copy(batch, e.queue[:n])
		e.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		seq, err := e.reporter.Events(ctx, e.runID, batch)
		cancel()
		if err != nil {
			if errors.Is(err, ErrRunFinished) || isPermanent(err) {
				e.log.Warn("events refused; dropping the outbox", "run_id", e.runID, "error", err)
				e.mu.Lock()
				e.dead = err
				e.queue = nil
				e.mu.Unlock()
				continue
			}
			e.log.Warn("events not delivered; will retry", "run_id", e.runID, "count", len(batch), "error", err)
			time.Sleep(backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			e.kick()
			continue
		}
		backoff = time.Second
		e.mu.Lock()
		e.queue = e.queue[n:]
		if seq > e.lastSeq {
			e.lastSeq = seq
		}
		more := len(e.queue) > 0
		e.mu.Unlock()
		if more {
			e.kick()
		}
	}
}

// PermanentError marks a refusal that no retry fixes (invalid_event, not_found, validation_failed).
type PermanentError struct{ Err error }

func (p *PermanentError) Error() string { return p.Err.Error() }
func (p *PermanentError) Unwrap() error { return p.Err }

func isPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}
