// Package scheduler runs queued tasks on a fixed pool of workers in strict
// submission order. It knows nothing about what a task does.
package scheduler

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
)

// Task is a unit of work.
type Task interface {
	ID() string
	// Run is called with a context that is cancelled when Shutdown begins; it
	// must return promptly after that.
	Run(ctx context.Context)
	// Discard is called instead of Run when the task will never run because it
	// was still queued at shutdown.
	Discard()
}

type Config struct {
	MaxConcurrent int
	MaxQueue      int
}

var (
	ErrQueueFull = errors.New("job queue is full")
	ErrStopped   = errors.New("scheduler is stopped")
	ErrDuplicate = errors.New("duplicate task id")
)

type Stats struct {
	Queued, Running, MaxConcurrent, MaxQueue int
}

// Scheduler is a bounded FIFO served by MaxConcurrent workers.
type Scheduler struct {
	cfg Config

	// mu guards everything below. Workers wait on cond for the queue to become
	// non-empty or for shutdown; they never hold mu while running a task.
	mu      sync.Mutex
	cond    *sync.Cond
	queue   *list.List               // of Task, in submission order
	ids     map[string]*list.Element // queued: its element; running: nil
	running int
	started bool
	stopped bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	doneOnce sync.Once
	done     chan struct{} // closed once every worker has exited
}

// New returns an unstarted Scheduler. It panics if either limit is not
// positive.
func New(cfg Config) *Scheduler {
	if cfg.MaxConcurrent <= 0 || cfg.MaxQueue <= 0 {
		panic(fmt.Sprintf("scheduler: invalid config %+v", cfg))
	}
	s := &Scheduler{
		cfg:   cfg,
		queue: list.New(),
		ids:   make(map[string]*list.Element),
		done:  make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

// Start spawns the workers. Tasks submitted earlier start running in order.
// It is idempotent and does nothing after Shutdown.
func (s *Scheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true
	s.wg.Add(s.cfg.MaxConcurrent)
	for range s.cfg.MaxConcurrent {
		go s.worker()
	}
}

func (s *Scheduler) worker() {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		for s.queue.Len() == 0 && !s.stopped {
			s.cond.Wait()
		}
		if s.stopped {
			// Shutdown has already emptied the queue.
			s.mu.Unlock()
			return
		}
		t := s.queue.Remove(s.queue.Front()).(Task)
		s.ids[t.ID()] = nil
		s.running++
		s.mu.Unlock()

		t.Run(s.ctx)

		s.mu.Lock()
		s.running--
		delete(s.ids, t.ID())
		s.mu.Unlock()
	}
}

// Submit queues t. It fails with ErrStopped after Shutdown began,
// ErrDuplicate if a queued or running task has the same ID, and ErrQueueFull
// when MaxQueue tasks are already waiting.
func (s *Scheduler) Submit(t Task) error {
	id := t.ID()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return ErrStopped
	}
	if _, ok := s.ids[id]; ok {
		return fmt.Errorf("submit task %s: %w", id, ErrDuplicate)
	}
	if s.queue.Len() >= s.cfg.MaxQueue {
		return ErrQueueFull
	}
	s.ids[id] = s.queue.PushBack(t)
	s.cond.Signal()
	return nil
}

// Cancel removes a task that is still queued and reports whether it did. A
// removed task is neither run nor discarded; the caller owns its fate. A task
// a worker has already dequeued cannot be cancelled here.
func (s *Scheduler) Cancel(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.ids[id]
	if e == nil {
		return false
	}
	s.queue.Remove(e)
	delete(s.ids, id)
	return true
}

// Shutdown refuses new submissions, discards every queued task, cancels the
// context of running tasks and waits for the workers. It returns ctx.Err() if
// workers are still busy when ctx is done. It is idempotent; later calls just
// wait again.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	var discard []Task
	if !s.stopped {
		s.stopped = true
		for e := s.queue.Front(); e != nil; e = e.Next() {
			t := e.Value.(Task)
			discard = append(discard, t)
			delete(s.ids, t.ID())
		}
		s.queue.Init()
		s.cond.Broadcast()
	}
	s.mu.Unlock()

	// Discard runs outside the lock so a task may call back into the
	// scheduler. Only the first caller gets a non-empty list, so each task is
	// discarded exactly once; stopped is already set, so none can be Run.
	for _, t := range discard {
		t.Discard()
	}
	s.cancel()

	s.doneOnce.Do(func() {
		go func() {
			s.wg.Wait()
			close(s.done)
		}()
	})
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scheduler) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Queued:        s.queue.Len(),
		Running:       s.running,
		MaxConcurrent: s.cfg.MaxConcurrent,
		MaxQueue:      s.cfg.MaxQueue,
	}
}
