// Package queue holds messages whose first disposition attempt failed
// temporarily and retries them on a fixed interval. The queue is held in
// memory only. It does not survive a process restart.
package queue

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"smtpfilter/internal/config"
	"smtpfilter/internal/msg"
)

// Entry is one message awaiting a further attempt.
type Entry struct {
	Msg      *msg.Message
	Accepted time.Time
	NextTry  time.Time
	Attempts int32
}

// Queue wraps a dispatcher. It attempts the disposition once inline, and
// holds only what that attempt failed to deliver, so that the common path
// carries no queue overhead.
type Queue struct {
	next msg.Dispatcher
	cfg  config.Retry
	log  *slog.Logger

	mu      sync.Mutex
	entries []Entry
	bytes   int64

	stop chan struct{}
	done chan struct{}
}

// New returns a queue that wraps the given dispatcher.
func New(next msg.Dispatcher, cfg config.Retry, log *slog.Logger) *Queue {
	return &Queue{
		next: next,
		cfg:  cfg,
		log:  log,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// Dispatch performs the inline attempt and enqueues on a temporary failure.
//
// Every outcome here is reported to the session as something that produces
// a 250 reply. The sending host is never told that the message was held or
// lost.
func (q *Queue) Dispatch(ctx context.Context, m *msg.Message) (msg.Result, error) {
	res, err := q.next.Dispatch(ctx, m)
	if res != msg.ResultTempFail {
		return res, err
	}
	if !q.cfg.Enabled {
		return msg.ResultTempFail, err
	}
	if q.enqueue(m) {
		return msg.ResultQueued, err
	}
	return msg.ResultTempFail, err
}

// enqueue adds a message. It returns false when a limit refuses it, in
// which case the message is discarded, because the sending host already
// received its 250.
func (q *Queue) enqueue(m *msg.Message) bool {
	size := int64(len(m.Body))

	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.entries) >= q.cfg.MaxEntries {
		q.log.Error("message discarded, queue entry limit reached",
			"from", m.From, "to", m.To, "route", routeName(m),
			"entries", len(q.entries))
		return false
	}
	if q.bytes+size > q.cfg.MaxBytes {
		q.log.Error("message discarded, queue byte limit reached",
			"from", m.From, "to", m.To, "route", routeName(m),
			"bytes", q.bytes, "size", size)
		return false
	}

	now := time.Now()
	q.entries = append(q.entries, Entry{
		Msg:      m,
		Accepted: now,
		NextTry:  now.Add(q.cfg.Interval()),
	})
	q.bytes += size

	q.log.Warn("message queued", "from", m.From, "to", m.To,
		"route", routeName(m), "entries", len(q.entries))
	return true
}

// Start runs the worker. It returns immediately.
func (q *Queue) Start() {
	if !q.cfg.Enabled {
		close(q.done)
		return
	}
	go q.run()
}

// run is the worker loop.
func (q *Queue) run() {
	defer close(q.done)

	t := time.NewTicker(q.cfg.Interval())
	defer t.Stop()

	for {
		select {
		case <-t.C:
			q.tick()
		case <-q.stop:
			return
		}
	}
}

// tick expires stale entries, then attempts every entry that is due.
//
// The mutex is not held during a disposition attempt. A slow endpoint would
// otherwise block every session goroutine that attempts to enqueue.
func (q *Queue) tick() {
	now := time.Now()

	q.mu.Lock()
	// Expiry. Iteration proceeds from the end, because removal swaps the
	// final element into the vacated slot.
	for i := len(q.entries) - 1; i >= 0; i-- {
		if now.Sub(q.entries[i].Accepted) <= q.cfg.Expire() {
			continue
		}
		e := q.entries[i]
		q.log.Error("message discarded, queue entry expired",
			"from", e.Msg.From, "to", e.Msg.To, "route", routeName(e.Msg),
			"attempts", e.Attempts, "age", now.Sub(e.Accepted).String())
		q.remove(i)
	}

	// Copy the due entries and release the lock before attempting them.
	var due []*msg.Message
	for i := range q.entries {
		if !now.Before(q.entries[i].NextTry) {
			due = append(due, q.entries[i].Msg)
		}
	}
	q.mu.Unlock()

	for _, m := range due {
		ctx, cancel := context.WithTimeout(context.Background(),
			m.Route.Timeout())
		res, err := q.next.Dispatch(ctx, m)
		cancel()

		q.mu.Lock()
		i := q.indexOf(m)
		if i < 0 {
			// The entry expired during the attempt.
			q.mu.Unlock()
			continue
		}
		switch res {
		case msg.ResultOK:
			q.log.Info("queued message delivered", "from", m.From, "to", m.To,
				"route", routeName(m), "attempts", q.entries[i].Attempts+1)
			q.remove(i)
		case msg.ResultPermFail:
			q.log.Error("message discarded, permanent failure on retry",
				"from", m.From, "to", m.To, "route", routeName(m),
				"attempts", q.entries[i].Attempts+1, "err", err)
			q.remove(i)
		default:
			q.entries[i].Attempts++
			q.entries[i].NextTry = time.Now().Add(q.cfg.Interval())
		}
		q.mu.Unlock()
	}
}

// indexOf finds an entry by message identity. The caller holds the mutex.
func (q *Queue) indexOf(m *msg.Message) int {
	for i := range q.entries {
		if q.entries[i].Msg == m {
			return i
		}
	}
	return -1
}

// remove deletes one entry by swapping the final element into the vacated
// slot and truncating. The caller holds the mutex.
//
// Order carries no meaning here, because each entry holds its own NextTry
// value and the disposition targets are independent.
//
// The assignment of the zero value to the vacated final element is
// required. Without it the backing array retains a reference to the message
// and the garbage collector cannot reclaim the body until that slot is
// overwritten.
func (q *Queue) remove(i int) {
	q.bytes -= int64(len(q.entries[i].Msg.Body))
	last := len(q.entries) - 1
	q.entries[i] = q.entries[last]
	q.entries[last] = Entry{}
	q.entries = q.entries[:last]
}

// Len returns the current entry count.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries)
}

// Drain stops the worker and makes one final attempt on every entry, until
// the queue is empty or the deadline passes. Anything still held when it
// returns is lost, and each loss is logged.
func (q *Queue) Drain(limit time.Duration) {
	if !q.cfg.Enabled {
		return
	}
	close(q.stop)
	<-q.done

	deadline := time.Now().Add(limit)
	for q.Len() > 0 && time.Now().Before(deadline) {
		q.mu.Lock()
		for i := range q.entries {
			q.entries[i].NextTry = time.Time{}
		}
		q.mu.Unlock()
		q.tick()
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.entries {
		e := q.entries[i]
		q.log.Error("message lost, queue not drained before shutdown",
			"from", e.Msg.From, "to", e.Msg.To, "route", routeName(e.Msg),
			"attempts", e.Attempts)
	}
}

// routeName returns the recipient pattern of the route, for the log.
func routeName(m *msg.Message) string {
	if m == nil || m.Route == nil {
		return ""
	}
	return m.Route.Recipient
}

