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
func (api *API) handleEventStream(w http.ResponseWriter, r *http.Request) {
	api.runSSE(w, r, "", "/api/v1/events")
}

// handleTaskEventStream GET /api/v1/tasks/{id}/events — per-task SSE stream.
func (api *API) handleTaskEventStream(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, codeBadInput, "missing task id")
		return
	}
	api.runSSE(w, r, taskID, "/api/v1/tasks/{id}/events")
}

// runSSE serves the SSE contract: replay events with id > Last-Event-ID, then
// live-tail new rows. taskID == "" means tenant-global; otherwise scoped.
func (api *API) runSSE(w http.ResponseWriter, r *http.Request, taskID, where string) {
	principal, ok := requireAccess(w, r, authz.ActionEventStream, taskID)
	if !ok {
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
	sent, err := api.drainBatch(ctx, w, flusher, principal.TenantID, taskID, lastID)
	if err != nil {
		api.log.Warn("sse replay failed", "where", where, "error", err)
		return
	}
	lastID = sent

	// Live tail + heartbeat.
	heartbeatTicker := time.NewTicker(sseHeartbeat)
	defer heartbeatTicker.Stop()
	for {
		// Poll for new rows. ctx cancel wins on the next tick; a future upgrade
		// to LISTEN/NOTIFY removes the polling latency.
		got, err := api.drainBatch(ctx, w, flusher, principal.TenantID, taskID, lastID)
		if err != nil {
			api.log.Warn("sse tail failed", "where", where, "error", err)
			return
		}
		if got != lastID {
			lastID = got
		}
		select {
		case <-ctx.Done():
			return
		case <-heartbeatTicker.C:
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
func (api *API) drainBatch(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, tenantID, taskID string, afterID int64) (int64, error) {
	var (
		rows []sqlc.Event
		err  error
	)
	if taskID == "" {
		rows, err = api.queries.ListEventsAfter(ctx, sqlc.ListEventsAfterParams{
			TenantID: tenantID, ID: afterID, Limit: sseReplayBatch,
		})
	} else {
		rows, err = api.queries.ListEventsAfterTask(ctx, sqlc.ListEventsAfterTaskParams{
			TenantID: tenantID, TaskID: nullStr(taskID), ID: afterID, Limit: sseReplayBatch,
		})
	}
	if err != nil {
		return afterID, err
	}
	for _, event := range rows {
		if err := writeSSEFrame(w, event); err != nil {
			return afterID, err
		}
		afterID = event.ID
	}
	flusher.Flush()
	return afterID, nil
}

// writeSSEFrame writes one id:/event:/data: frame. Returns the write error so
// the caller can detect a gone client.
//
// The data object mixes two facts from the event row into the payload the
// event's source recorded: the run id and the actor. Both live on the row
// rather than in every producer's payload, which is why the docs can promise
// them on every frame while individual payloads never repeat them — and why a
// system-written event cannot read on the stream as the task author acting.
// The task id is mixed in only when the row carries one (a tenant-global event
// gets no empty placeholder), a producer's own key always wins over the
// mixed-in value, and a non-object payload is passed through untouched rather
// than rewritten into one.
func writeSSEFrame(w http.ResponseWriter, event sqlc.Event) error {
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\n", event.ID, event.Type); err != nil {
		return err
	}
	payload := event.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	merged := payload
	var data map[string]json.RawMessage
	if err := json.Unmarshal(payload, &data); err == nil && data != nil {
		if event.TaskID.Valid && event.TaskID.String != "" {
			mixInFrameFact(data, "task_id", event.TaskID.String)
		}
		mixInFrameFact(data, "actor", event.Actor)
		if encoded, encodeErr := json.Marshal(data); encodeErr == nil {
			merged = encoded
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", merged); err != nil {
		return err
	}
	return nil
}

// mixInFrameFact sets key to value unless the producer's payload already
// carries it: the frame supplements the payload, never rewrites it.
func mixInFrameFact(data map[string]json.RawMessage, key, value string) {
	if _, exists := data[key]; exists || value == "" {
		return
	}
	encoded, encodeErr := json.Marshal(value)
	if encodeErr != nil {
		return
	}
	data[key] = encoded
}

// parseLastEventID parses the Last-Event-ID header. Empty/invalid → 0, which
// means "replay from the start".
func parseLastEventID(rawHeader string) int64 {
	if rawHeader == "" {
		return 0
	}
	parsed, err := strconv.ParseInt(rawHeader, 10, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}
