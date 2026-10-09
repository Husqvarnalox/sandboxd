package logbuf

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before deadline")
		}
		time.Sleep(time.Millisecond)
	}
}

func recv(t *testing.T, ch <-chan Chunk) (Chunk, bool) {
	t.Helper()
	select {
	case c, ok := <-ch:
		return c, ok
	case <-time.After(5 * time.Second):
		t.Fatal("timed out receiving chunk")
		return Chunk{}, false
	}
}

func TestStreamString(t *testing.T) {
	for s, want := range map[Stream]string{Stdout: "stdout", Stderr: "stderr", 0: "unknown"} {
		if got := s.String(); got != want {
			t.Errorf("Stream(%d) = %q, want %q", s, got, want)
		}
	}
}

func TestNewPanicsOnBadCapacity(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	New(0)
}

func TestEviction(t *testing.T) {
	b := New(10)
	b.Write(Stdout, []byte("aaaa"))
	b.Write(Stderr, []byte("bbbb"))
	if b.Size() != 8 || b.Truncated() != 0 {
		t.Fatalf("size=%d truncated=%d", b.Size(), b.Truncated())
	}
	b.Write(Stdout, []byte("cccc")) // evicts "aaaa"
	if b.Size() != 8 || b.Truncated() != 4 {
		t.Fatalf("size=%d truncated=%d", b.Size(), b.Truncated())
	}
	h := b.History()
	if len(h) != 2 || string(h[0].Data) != "bbbb" || string(h[1].Data) != "cccc" {
		t.Fatalf("history = %+v", h)
	}
	if h[0].Stream != Stderr || h[1].Stream != Stdout {
		t.Errorf("streams = %v %v", h[0].Stream, h[1].Stream)
	}
	if h[0].Dropped != 4 || h[1].Dropped != 0 {
		t.Errorf("dropped = %d %d", h[0].Dropped, h[1].Dropped)
	}
}

func TestOversizedChunkKeepsTail(t *testing.T) {
	b := New(5)
	b.Write(Stdout, []byte("old"))
	b.Write(Stdout, []byte("0123456789"))
	h := b.History()
	if len(h) != 1 || string(h[0].Data) != "56789" {
		t.Fatalf("history = %+v", h)
	}
	// 3 bytes of "old" plus 5 cut from the oversized write.
	if b.Truncated() != 8 || h[0].Dropped != 8 || b.Size() != 5 {
		t.Fatalf("truncated=%d dropped=%d size=%d", b.Truncated(), h[0].Dropped, b.Size())
	}
}

func TestWriteCopiesInput(t *testing.T) {
	b := New(100)
	p := []byte("hello")
	b.Write(Stdout, p)
	p[0] = 'X'
	if got := string(b.History()[0].Data); got != "hello" {
		t.Fatalf("history aliased caller buffer: %q", got)
	}
}

func TestEmptyWriteIgnored(t *testing.T) {
	b := New(10)
	b.Write(Stdout, nil)
	if len(b.History()) != 0 {
		t.Fatal("empty write recorded")
	}
}

func TestManyEvictionsCompact(t *testing.T) {
	b := New(3)
	for i := range 5000 {
		b.Write(Stdout, []byte{byte(i)})
	}
	h := b.History()
	if len(h) != 3 || int(h[0].Data[0]) != 4997%256 || int(h[2].Data[0]) != 4999%256 {
		t.Fatalf("history = %+v", h)
	}
	if b.Truncated() != 4997 || h[0].Dropped != 4997 {
		t.Fatalf("truncated=%d", b.Truncated())
	}
	b.mu.Lock()
	n := len(b.chunks)
	b.mu.Unlock()
	if n > 2*compactThreshold+3 {
		t.Fatalf("backing slice not compacted: %d", n)
	}
}

func TestWriterAdapter(t *testing.T) {
	b := New(100)
	n, err := io.Copy(b.Writer(Stderr), bytes.NewReader([]byte("copied")))
	if err != nil || n != 6 {
		t.Fatalf("copy = %d, %v", n, err)
	}
	h := b.History()
	if len(h) != 1 || h[0].Stream != Stderr || string(h[0].Data) != "copied" {
		t.Fatalf("history = %+v", h)
	}
}

func TestSubscribeHistoryAndLive(t *testing.T) {
	b := New(100)
	b.Write(Stdout, []byte("one"))
	sub := b.Subscribe()
	defer sub.Close()
	if len(sub.History) != 1 || string(sub.History[0].Data) != "one" {
		t.Fatalf("history = %+v", sub.History)
	}
	b.Write(Stderr, []byte("two"))
	c, ok := recv(t, sub.C)
	if !ok || string(c.Data) != "two" || c.Stream != Stderr || c.Dropped != 0 {
		t.Fatalf("chunk = %+v ok=%v", c, ok)
	}
}

func TestSlowSubscriberDropAccounting(t *testing.T) {
	b := New(1 << 20)
	slow := b.Subscribe()
	defer slow.Close()
	fast := b.Subscribe()
	defer fast.Close()

	const extra = 10
	for range subscriberBacklog + extra {
		b.Write(Stdout, []byte("abc"))
		if _, ok := recv(t, fast.C); !ok {
			t.Fatal("fast channel closed")
		}
	}

	for i := range subscriberBacklog {
		c, _ := recv(t, slow.C)
		if c.Dropped != 0 {
			t.Fatalf("chunk %d: dropped=%d before any gap was visible", i, c.Dropped)
		}
	}
	b.Write(Stdout, []byte("zz"))
	c, _ := recv(t, slow.C)
	if c.Dropped != extra*3 || string(c.Data) != "zz" {
		t.Fatalf("chunk = %+v, want dropped=%d", c, extra*3)
	}
	b.Write(Stdout, []byte("y"))
	if c, _ := recv(t, slow.C); c.Dropped != 0 {
		t.Fatalf("drops reported twice: %d", c.Dropped)
	}
	// The slow reader never held up the writer or the fast subscriber.
	if len(b.History()) != subscriberBacklog+extra+2 {
		t.Fatalf("history len = %d", len(b.History()))
	}
}

func TestPendingDropsNotDeliveredOnClose(t *testing.T) {
	b := New(1 << 20)
	sub := b.Subscribe()
	for range subscriberBacklog + 3 {
		b.Write(Stdout, []byte("x"))
	}
	b.Close()
	n := 0
	for c := range sub.C {
		if c.Dropped != 0 {
			t.Fatalf("unexpected dropped=%d", c.Dropped)
		}
		n++
	}
	if n != subscriberBacklog {
		t.Fatalf("received %d chunks, want %d", n, subscriberBacklog)
	}
}

func TestBufferClose(t *testing.T) {
	b := New(100)
	b.Write(Stdout, []byte("x"))
	sub := b.Subscribe()
	if b.Closed() {
		t.Fatal("closed too early")
	}
	b.Close()
	b.Close()
	if !b.Closed() {
		t.Fatal("not closed")
	}
	if _, ok := recv(t, sub.C); ok {
		t.Fatal("channel not closed")
	}
	b.Write(Stdout, []byte("late"))
	if len(b.History()) != 1 || b.Size() != 1 {
		t.Fatalf("write after close recorded: %+v", b.History())
	}
	sub.Close() // after Buffer.Close
	sub.Close()

	late := b.Subscribe()
	if len(late.History) != 1 || string(late.History[0].Data) != "x" {
		t.Fatalf("history = %+v", late.History)
	}
	if _, ok := recv(t, late.C); ok {
		t.Fatal("subscription on closed buffer is open")
	}
	late.Close()
}

func TestSubscriptionClose(t *testing.T) {
	b := New(100)
	sub := b.Subscribe()
	other := b.Subscribe()
	defer other.Close()

	released := make(chan bool)
	go func() {
		_, ok := <-sub.C
		released <- ok
	}()
	sub.Close()
	sub.Close()
	if ok := <-released; ok {
		t.Fatal("blocked reader received data instead of close")
	}

	b.Write(Stdout, []byte("after"))
	if c, ok := recv(t, other.C); !ok || string(c.Data) != "after" {
		t.Fatalf("other subscriber affected: %+v ok=%v", c, ok)
	}
	b.Close()
}

func seqChunk(n uint64) []byte {
	return binary.BigEndian.AppendUint64(nil, n)
}

// A subscriber must see history followed by live chunks with neither a gap nor
// a duplicate, even when subscribing in the middle of concurrent writes. A gap
// is only allowed where it is declared through Dropped.
func TestSubscribeConsistentUnderConcurrentWrites(t *testing.T) {
	const writes = 5000
	const subscribers = 8
	b := New(1 << 20)

	start := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		<-start
		for i := uint64(1); i <= writes; i++ {
			b.Write(Stdout, seqChunk(i))
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, subscribers)
	for range subscribers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			sub := b.Subscribe()
			defer sub.Close()
			next := uint64(1)
			for _, c := range sub.History {
				if got := binary.BigEndian.Uint64(c.Data); got != next {
					errs <- fmt.Errorf("history: got %d want %d", got, next)
					return
				}
				next++
			}
			for c := range sub.C {
				got := binary.BigEndian.Uint64(c.Data)
				next += uint64(c.Dropped) / 8
				if got != next {
					errs <- fmt.Errorf("live: got %d want %d (dropped %d)", got, next, c.Dropped)
					return
				}
				next++
			}
		}()
	}
	close(start)
	writer.Wait()
	b.Close()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestConcurrentWritersAndSubscribers(t *testing.T) {
	b := New(512)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream := Stdout
			if w%2 == 1 {
				stream = Stderr
			}
			for range 2000 {
				b.Write(stream, []byte("0123456789"))
			}
		}()
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				sub := b.Subscribe()
				select {
				case <-sub.C:
				default:
				}
				b.History()
				b.Size()
				b.Truncated()
				sub.Close()
			}
		}()
	}
	wg.Wait()
	if b.Size() > 512 {
		t.Fatalf("size %d exceeds capacity", b.Size())
	}
	b.Close()
}

func BenchmarkWrite(bm *testing.B) {
	b := New(1 << 20)
	p := bytes.Repeat([]byte("x"), 128)
	bm.SetBytes(int64(len(p)))
	bm.ReportAllocs()
	for bm.Loop() {
		b.Write(Stdout, p)
	}
}

func BenchmarkWriteFanout(bm *testing.B) {
	b := New(1 << 20)
	for range 8 {
		sub := b.Subscribe()
		defer sub.Close()
	}
	p := bytes.Repeat([]byte("x"), 128)
	bm.SetBytes(int64(len(p)))
	bm.ReportAllocs()
	for bm.Loop() {
		b.Write(Stdout, p)
	}
}
