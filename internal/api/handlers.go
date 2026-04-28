// Package api exposes the REST endpoints over net/http.
//
// POST   /api/v1/network-measure            — start a test, returns 202 + task_id
// GET    /api/v1/network-measure/{task_id}  — poll status / retrieve result
// DELETE /api/v1/network-measure/{task_id}  — evict a terminal task from memory
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
	mux.HandleFunc("DELETE /api/v1/network-measure/{task_id}", h.deleteMeasure)
}

// healthz is a simple liveness/readiness probe target.
func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// startMeasure handles POST /api/v1/network-measure.
//
// Flow:
//  1. Acquire the global lock; reject with 409 if a measurement is already running.
//  2. Discover ready nodes (pod-based snapshot) — determines the ETA and the
//     exact node set that will be tested. Releases the lock and returns 503 on error.
//  3. Register the task as "pending" and store its CancelFunc.
//  4. Launch the executor in a background goroutine that releases the lock on exit.
//  5. Return 202 Accepted with the enriched response including nodes and ETA.
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

	// ── Pod-based node discovery (snapshot) ──────────────────────────────────
	// Discover now so the 202 response can report exactly which nodes will be
	// tested. The snapshot is passed directly to the background goroutine —
	// cluster changes after this point do not affect the current task.
	discoverCtx, discoverCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer discoverCancel()
	snapshot, err := h.executor.DiscoverNodes(discoverCtx)
	if err != nil {
		h.mu.Lock()
		h.isRunning = false
		h.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":  "node discovery failed: " + err.Error(),
			"status": "unavailable",
		})
		return
	}

	// ── ETA calculation ───────────────────────────────────────────────────────
	nNodes := len(snapshot.IPs)
	var rounds int
	if nNodes > 0 {
		if nNodes%2 == 0 {
			rounds = nNodes - 1
		} else {
			rounds = nNodes
		}
	}
	estimatedSeconds := rounds * (executor.IperfInitialDuration + 5) // 10 s test + 5 s cooldown

	// ── Task registration ─────────────────────────────────────────────────────
	taskID := uuid.New().String()
	task := &store.Task{
		ID:        taskID,
		Status:    store.StatusPending,
		CreatedAt: time.Now().UTC(),
	}
	h.store.Set(taskID, task)

	// The cancel func is stored before the goroutine starts so that any caller
	// with access to the store (e.g. integration tests) can abort the run.
	ctx, cancel := context.WithCancel(context.Background())
	h.store.SetCancel(taskID, cancel)

	go func() {
		defer func() {
			h.mu.Lock()
			h.isRunning = false
			h.mu.Unlock()
		}()
		h.executor.Run(ctx, taskID, h.store, snapshot)
	}()

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"task_id":                    taskID,
		"status":                     "accepted",
		"message":                    "Measurement started.",
		"node_count":                 nNodes,
		"nodes":                      snapshot.IPs,
		"total_rounds":               rounds,
		"estimated_duration_seconds": estimatedSeconds,
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

// deleteMeasure handles DELETE /api/v1/network-measure/{task_id}.
//
// Evicts a terminal task from the in-memory store to free RAM immediately,
// without waiting for the TTL sweeper. Active (pending/running) tasks are
// rejected so callers cannot remove in-flight state.
//
// Responses:
//   - 200 OK          — task evicted from store
//   - 400 Bad Request — task is still pending or running; cannot delete
//   - 404 Not Found   — unknown task_id
func (h *Handler) deleteMeasure(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")

	task, ok := h.store.Get(taskID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}

	if task.Status == store.StatusRunning || task.Status == store.StatusPending {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":  "task is still active; wait for it to finish before deleting",
			"status": task.Status,
		})
		return
	}

	h.store.Delete(taskID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"task_id": taskID,
		"message": "task deleted",
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
