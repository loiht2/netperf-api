// Package api exposes the REST endpoints over net/http.
//
// POST   /api/v1/network-measure            — start a test, returns 202 + task_id
// GET    /api/v1/network-measure/{task_id}  — poll status / retrieve result
// DELETE /api/v1/network-measure/{task_id}  — cancel a running test
// GET    /healthz                           — liveness / readiness probe
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/netperf/netperf-api/internal/executor"
	"github.com/netperf/netperf-api/internal/store"
)

// Handler wires the HTTP layer to the task store and executor.
type Handler struct {
	store     *store.Store
	executor  *executor.Executor
	mu        sync.Mutex // guards isRunning
	isRunning bool       // true while a measurement goroutine is active
}

// New creates a Handler.
func New(s *store.Store, e *executor.Executor) *Handler {
	return &Handler{store: s, executor: e}
}

// RegisterRoutes registers all API routes on mux.
// Requires Go 1.22+ for method-qualified patterns and {task_id} wildcards.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("POST /api/v1/network-measure", h.startMeasure)
	mux.HandleFunc("GET /api/v1/network-measure/{task_id}", h.getMeasure)
	mux.HandleFunc("DELETE /api/v1/network-measure/{task_id}", h.cancelMeasure)
}

// healthz is a simple liveness/readiness probe target.
func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// startMeasure handles POST /api/v1/network-measure.
//
// Flow:
//  1. Acquire the global lock; reject with 409 if a measurement is already running.
//  2. Count ready worker nodes for an ETA estimate.
//  3. Generate a UUID task_id and record the task as "pending".
//  4. Create a cancellable context and store its CancelFunc under the task_id
//     so the DELETE handler can abort it later.
//  5. Launch the executor in a background goroutine that releases the lock on exit.
//  6. Return 202 Accepted with task_id and estimated_duration_seconds.
func (h *Handler) startMeasure(w http.ResponseWriter, r *http.Request) {
	// ── Global execution lock ────────────────────────────────────────────────
	h.mu.Lock()
	if h.isRunning {
		h.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error":  "A network measurement is already in progress. Please try again later.",
			"status": "conflict",
		})
		return
	}
	h.isRunning = true
	h.mu.Unlock()

	// ── ETA estimate ─────────────────────────────────────────────────────────
	// Count nodes now (fast list call) so we can tell the caller how long to wait.
	// On error nNodes = 0 → estimatedSeconds = 0 (safe: executor will report the
	// real failure once it runs).
	nodeCtx, nodeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer nodeCancel()
	nNodes, _ := h.executor.CountReadyNodes(nodeCtx)

	var rounds int
	if nNodes > 0 {
		if nNodes%2 == 0 {
			rounds = nNodes - 1
		} else {
			rounds = nNodes
		}
	}
	estimatedSeconds := rounds * (executor.IperfDuration + 5) // 10s test + 5s cooldown

	// ── Task registration ─────────────────────────────────────────────────────
	taskID := uuid.New().String()
	task := &store.Task{
		ID:        taskID,
		Status:    store.StatusPending,
		CreatedAt: time.Now().UTC(),
	}
	h.store.Set(taskID, task)

	// context.WithCancel lets the DELETE endpoint abort mid-flight execs.
	// The cancel func is stored before the goroutine starts to eliminate the
	// race where DELETE arrives before SetCancel is called.
	ctx, cancel := context.WithCancel(context.Background())
	h.store.SetCancel(taskID, cancel)

	go func() {
		defer func() {
			h.mu.Lock()
			h.isRunning = false
			h.mu.Unlock()
		}()
		h.executor.Run(ctx, taskID, h.store)
	}()

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"task_id":                    taskID,
		"status":                     "accepted",
		"estimated_duration_seconds": estimatedSeconds,
		"message":                    "Measurement started. Please poll the GET endpoint.",
	})
}

// getMeasure handles GET /api/v1/network-measure/{task_id}.
// Returns the current status and, when completed, the full result matrix.
func (h *Handler) getMeasure(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")

	task, ok := h.store.Get(taskID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}

	resp := map[string]interface{}{
		"task_id":    task.ID,
		"status":     task.Status,
		"created_at": task.CreatedAt,
	}
	if task.Duration != "" {
		resp["duration"] = task.Duration
	}
	switch task.Status {
	case store.StatusCompleted:
		resp["result"] = task.Result
	case store.StatusFailed, store.StatusCanceled:
		resp["error"] = task.Error
	}

	writeJSON(w, http.StatusOK, resp)
}

// cancelMeasure handles DELETE /api/v1/network-measure/{task_id}.
//
// It calls the stored CancelFunc for the task, which propagates context
// cancellation down into every in-flight remotecommand.StreamWithContext call.
// The background goroutine will drain, mark the task as "canceled", and clean
// up the cancel func itself via defer.
//
// Responses:
//   - 202 Accepted  — cancel signal sent; task will transition to "canceled"
//   - 404 Not Found — unknown task_id
//   - 409 Conflict  — task already finished (completed / failed / canceled)
func (h *Handler) cancelMeasure(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")

	task, ok := h.store.Get(taskID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}

	// Guard against cancelling a task that already reached a terminal state.
	// This check is best-effort; the authoritative answer is whether a cancel
	// func is still registered (store.Cancel uses LoadAndDelete atomically).
	switch task.Status {
	case store.StatusCompleted, store.StatusFailed, store.StatusCanceled:
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error":  "task is not running",
			"status": task.Status,
		})
		return
	}

	// store.Cancel invokes the CancelFunc and removes it atomically.
	// Returns false only if the task finished between our status check above
	// and this call — a harmless race that is safe to surface as a 409.
	if !h.store.Cancel(taskID) {
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error":  "task already finished",
			"status": task.Status,
		})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"task_id": taskID,
		"message": "cancellation signal sent; poll GET to confirm status=canceled",
	})
}

// writeJSON marshals v to JSON and writes it with the given HTTP status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
