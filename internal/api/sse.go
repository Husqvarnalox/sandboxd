package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/Husqvarnalox/sandboxd/internal/job"
	"github.com/Husqvarnalox/sandboxd/internal/logbuf"
)

// sseWriteTimeout bounds each event write. The server has no global
// WriteTimeout (streams are long-lived), so this is what drops dead clients
// and releases their log subscription.
const sseWriteTimeout = 30 * time.Second

type sseStream struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	last job.State
}

func (s *sseStream) event(name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", name, err)
	}
	// Not every writer supports deadlines (e.g. in tests); that is fine.
	_ = s.rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *sseStream) chunk(c logbuf.Chunk) error {
	ev := chunkEvent{DroppedBytes: c.Dropped}
	if utf8.Valid(c.Data) {
		str := string(c.Data)
		ev.Data = &str
	} else {
		ev.DataBase64 = c.Data
	}
	return s.event(c.Stream.String(), ev)
}

// state writes a state event unless it repeats the previous one.
func (s *sseStream) state(snap job.Snapshot) error {
	if snap.State == s.last {
		return nil
	}
	s.last = snap.State
	ev := stateEvent{State: snap.State, Reason: snap.Reason}
	if snap.Exited {
		code := snap.ExitCode
		ev.ExitCode = &code
	}
	return s.event("state", ev)
}

func (s *server) streamLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	follow := false
	if v := r.URL.Query().Get("follow"); v != "" {
		var err error
		if follow, err = strconv.ParseBool(v); err != nil {
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "follow must be a boolean")
			return
		}
	}
	buf, err := s.ex.Logs(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Armed before anything is read so no transition can slip between the
	// snapshot and the wait.
	changed, err := s.ex.Changed(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	st := &sseStream{w: w, rc: http.NewResponseController(w)}

	if !follow {
		for _, c := range buf.History() {
			if st.chunk(c) != nil {
				return
			}
		}
		if snap, err := s.ex.Get(id); err == nil && snap.State.Terminal() {
			st.state(snap)
		}
		return
	}

	// Closing the subscription on return is the only effect a client
	// disconnect has: the job itself is never tied to the request context.
	sub := buf.Subscribe()
	defer sub.Close()
	for _, c := range sub.History {
		if st.chunk(c) != nil {
			return
		}
	}
	snap, err := s.ex.Get(id)
	if err != nil || st.state(snap) != nil {
		return
	}
	for {
		select {
		case c, ok := <-sub.C:
			if !ok {
				// The executor closes the log after the final transition,
				// so this snapshot is terminal (or the job was deleted).
				if snap, err := s.ex.Get(id); err == nil {
					st.state(snap)
				}
				return
			}
			if st.chunk(c) != nil {
				return
			}
		case <-changed:
			if changed, err = s.ex.Changed(id); err != nil {
				return
			}
			snap, err := s.ex.Get(id)
			if err != nil {
				return
			}
			// Output written before the transition is already queued on
			// sub.C; emit it first so the state event follows the output
			// that produced it.
			if !drain(st, sub) || st.state(snap) != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// drain writes every chunk already queued on the subscription. It reports
// false if the client is gone.
func drain(st *sseStream, sub *logbuf.Subscription) bool {
	for {
		select {
		case c, ok := <-sub.C:
			if !ok {
				return true
			}
			if st.chunk(c) != nil {
				return false
			}
		default:
			return true
		}
	}
}
