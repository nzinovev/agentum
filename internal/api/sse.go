package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// SSE event types. The runner emits these into the events table; the SSE
// handler frames each row as id:/event:/data: and the UI consumes them. See
// docs/api.md for the contract.
const (
	EvTaskStateChanged = "task.state_changed"
	EvStageInvocation  = "stage.invocation_started"
	EvStageStream      = "stage.stream"
	EvStageTool        = "stage.tool"
	EvStageStopped     = "stage.stopped"
	EvStageResult      = "stage.result"
	EvMemoryCommitted  = "memory.committed"
	EvRunLog           = "run.log"
)

// sse poll/tune knobs.
const (
	sseReplayBatch = 500 // max rows per replay query
	ssePollPeriod  = 500 * time.Millisecond
	sseHeartbeat   = 15 * time.Second
)

// handleEventStream GET /api/v1/events — tenant-global SSE stream.
func (a *API) handleEventStream(w http.ResponseWriter, r *http.Request) {
	a.runSSE(w, r, "", "/api/v1/events")
}

// handleTaskEventStream GET /api/v1/tasks/{id}/events — per-task SSE stream.
func (a *API) handleTaskEventStream(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, codeBadInput, "missing task id")
		return
	}
	a.runSSE(w, r, taskID, "/api/v1/tasks/{id}/events")
}

// runSSE serves the SSE contract: replay events with id > Last-Event-ID, then
// live-tail new rows. taskID == "" means tenant-global; otherwise scoped.
func (a *API) runSSE(w http.ResponseWriter, r *http.Request, taskID, where string) {
	p, ok := authz.PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "unresolved principal")
		return
	}
	if d := authz.Can(r.Context(), p, "event:stream", taskID); !d.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, d.Reason)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, codeInternal, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering in compose

	lastID := parseLastEventID(r.Header.Get("Last-Event-ID"))
	ctx := r.Context()

	// Replay: everything with id > lastID that already exists.
	sent, err := a.drainBatch(ctx, w, flusher, p.TenantID, taskID, lastID)
	if err != nil {
		a.log.Warn("sse replay failed", "where", where, "error", err)
		return
	}
	lastID = sent

	// Live tail + heartbeat.
	hb := time.NewTicker(sseHeartbeat)
	defer hb.Stop()
	for {
		// Poll for new rows. ctx cancel wins on the next tick; a future upgrade
		// to LISTEN/NOTIFY removes the polling latency.
		got, err := a.drainBatch(ctx, w, flusher, p.TenantID, taskID, lastID)
		if err != nil {
			a.log.Warn("sse tail failed", "where", where, "error", err)
			return
		}
		if got != lastID {
			lastID = got
		}
		select {
		case <-ctx.Done():
			return
		case <-hb.C:
			// Comment-frame keepalive (per the SSE spec, comments start with ':').
			if _, err := fmt.Fprintf(w, ": ping %d\n\n", time.Now().Unix()); err != nil {
				return
			}
			flusher.Flush()
		case <-time.After(ssePollPeriod):
		}
	}
}

// drainBatch queries one batch of events with id > afterID and writes them as
// SSE frames. Returns the new high-water id (== afterID if nothing was sent).
func (a *API) drainBatch(ctx context.Context, w http.ResponseWriter, f http.Flusher, tenantID, taskID string, afterID int64) (int64, error) {
	var (
		rows []sqlc.Event
		err  error
	)
	if taskID == "" {
		rows, err = a.q.ListEventsAfter(ctx, sqlc.ListEventsAfterParams{
			TenantID: tenantID, ID: afterID, Limit: sseReplayBatch,
		})
	} else {
		rows, err = a.q.ListEventsAfterTask(ctx, sqlc.ListEventsAfterTaskParams{
			TenantID: tenantID, TaskID: nullStr(taskID), ID: afterID, Limit: sseReplayBatch,
		})
	}
	if err != nil {
		return afterID, err
	}
	for _, ev := range rows {
		if err := writeSSEFrame(w, ev.ID, ev.Type, ev.Payload); err != nil {
			return afterID, err
		}
		afterID = ev.ID
	}
	f.Flush()
	return afterID, nil
}

// writeSSEFrame writes one id:/event:/data: frame. Returns the write error so
// the caller can detect a gone client.
func writeSSEFrame(w http.ResponseWriter, id int64, eventType string, payload json.RawMessage) error {
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\n", id, eventType); err != nil {
		return err
	}
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	return nil
}

// parseLastEventID parses the Last-Event-ID header. Empty/invalid → 0, which
// means "replay from the start".
func parseLastEventID(v string) int64 {
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
