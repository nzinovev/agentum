package server

import (
	"fmt"
	"net/http"
	"time"
)

// handleEventStream is the SSE endpoint for the live agent/event stream.
//
// Resumability (todo, ties into the events table): honor Last-Event-ID by
// replaying rows with events.id > lastID before switching to the live tail. The
// same durable log feeds the audit trail, so one schema serves both.
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering in compose

	lastEventID := r.Header.Get("Last-Event-ID")
	s.log.Info("sse attach", "last_event_id", lastEventID)

	// Stub: emit a heartbeat every 15s. Real impl tails the events table and
	// flushes each row as id:/event:/data: SSE frames.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case t := <-ticker.C:
			// A write error means the client is gone; end the stream.
			if _, err := fmt.Fprintf(w, "event: ping\ndata: %s\n\n", t.Format(time.RFC3339Nano)); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
