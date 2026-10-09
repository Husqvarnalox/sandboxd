// Package logbuf is a bounded in-memory log store with best-effort fan-out to
// live subscribers.
package logbuf

import (
	"io"
	"sync"
)

// Stream identifies the origin of a chunk.
type Stream uint8

const (
	Stdout Stream = 1
	Stderr Stream = 2
)

func (s Stream) String() string {
	switch s {
	case Stdout:
		return "stdout"
	case Stderr:
		return "stderr"
	}
	return "unknown"
}

const subscriberBacklog = 256

// compactThreshold bounds how many evicted slots the history slice keeps
// before the live chunks are moved back to the front.
const compactThreshold = 1024

// Chunk is one write to the buffer.
//
// Data is shared between the history and every subscriber that receives the
// chunk, so it is immutable by contract: receivers must not modify it.
type Chunk struct {
	Stream Stream
	Data   []byte
	// Dropped is the number of bytes that were discarded immediately before this
	// chunk: history evicted from the ring (first replayed chunk) or chunks skipped
	// because this subscriber was too slow.
	Dropped int64
}

// Buffer retains the most recent output of a job, shared by stdout and stderr,
// and fans new chunks out to subscribers. It never blocks writers.
type Buffer struct {
	capacity int

	mu        sync.Mutex
	chunks    []Chunk // retained chunks are chunks[head:]
	head      int
	size      int
	truncated int64
	subs      map[*Subscription]struct{}
	closed    bool
}

// New returns a Buffer retaining at most capacity bytes. It panics if
// capacity is not positive.
func New(capacity int) *Buffer {
	if capacity <= 0 {
		panic("logbuf: capacity must be positive")
	}
	return &Buffer{capacity: capacity, subs: make(map[*Subscription]struct{})}
}

// Write appends a copy of p to the history and offers it to every subscriber
// without blocking. It is a no-op after Close.
func (b *Buffer) Write(s Stream, p []byte) {
	if len(p) == 0 {
		return
	}
	data := append([]byte(nil), p...)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}

	// Subscribers see the full write; only the retained history is limited to
	// the capacity.
	kept := data
	if len(kept) > b.capacity {
		cut := len(kept) - b.capacity
		b.truncated += int64(cut)
		kept = kept[cut:]
	}
	b.chunks = append(b.chunks, Chunk{Stream: s, Data: kept})
	b.size += len(kept)
	for b.size > b.capacity {
		old := b.chunks[b.head]
		b.chunks[b.head] = Chunk{} // release the data for GC
		b.head++
		b.size -= len(old.Data)
		b.truncated += int64(len(old.Data))
	}
	if b.head >= compactThreshold && b.head*2 >= len(b.chunks) {
		n := copy(b.chunks, b.chunks[b.head:])
		clear(b.chunks[n:])
		b.chunks = b.chunks[:n]
		b.head = 0
	}

	for sub := range b.subs {
		// Drops are accounted in bytes and reported on the next chunk this
		// subscriber does receive, so a reader can tell how much it missed
		// without the writer ever waiting on it.
		c := Chunk{Stream: s, Data: data, Dropped: sub.pending}
		select {
		case sub.ch <- c:
			sub.pending = 0
		default:
			sub.pending += int64(len(data))
		}
	}
}

// Writer returns an io.Writer that writes to the buffer as stream s.
func (b *Buffer) Writer(s Stream) io.Writer { return streamWriter{b, s} }

type streamWriter struct {
	b *Buffer
	s Stream
}

func (w streamWriter) Write(p []byte) (int, error) {
	w.b.Write(w.s, p)
	return len(p), nil
}

// History returns a copy of the retained chunks in order. The first chunk
// carries Dropped=Truncated().
func (b *Buffer) History() []Chunk {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.historyLocked()
}

func (b *Buffer) historyLocked() []Chunk {
	live := b.chunks[b.head:]
	h := make([]Chunk, len(live))
	copy(h, live)
	if len(h) > 0 {
		h[0].Dropped = b.truncated
	}
	return h
}

// Truncated returns the total number of bytes evicted from the history.
func (b *Buffer) Truncated() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

// Size returns the number of bytes currently retained.
func (b *Buffer) Size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.size
}

// Close closes every subscription channel and discards later writes. Drops
// still pending for a subscriber when Close runs are never reported to it.
// Close is idempotent.
func (b *Buffer) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for sub := range b.subs {
		sub.closeLocked()
	}
	b.subs = nil
}

func (b *Buffer) Closed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// Subscription is a live view of a Buffer.
type Subscription struct {
	// History is captured at Subscribe time, consistent with C: every chunk
	// written afterwards is delivered on C (or counted in a Dropped field).
	History []Chunk
	C       <-chan Chunk

	b       *Buffer
	ch      chan Chunk
	pending int64 // guarded by b.mu
	closed  bool  // guarded by b.mu
}

// Subscribe registers a live subscriber and captures the history atomically.
// On a closed Buffer it returns the final history and an already closed
// channel.
func (b *Buffer) Subscribe() *Subscription {
	ch := make(chan Chunk, subscriberBacklog)
	sub := &Subscription{b: b, ch: ch, C: ch}

	b.mu.Lock()
	defer b.mu.Unlock()
	sub.History = b.historyLocked()
	if b.closed {
		sub.closed = true
		close(ch)
		return sub
	}
	b.subs[sub] = struct{}{}
	return sub
}

// Close unregisters the subscription and closes C. It is idempotent and safe
// to call concurrently with Write and Buffer.Close.
func (s *Subscription) Close() {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.closeLocked()
}

// closeLocked relies on Write sending under b.mu, so a channel is never
// closed while a send is in flight.
func (s *Subscription) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	delete(s.b.subs, s)
	close(s.ch)
}
