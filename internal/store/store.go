// Package store provides a concurrency-safe in-memory task state store.
// It holds two parallel sync.Maps: one for task state, one for the
// CancelFunc that lets callers abort a running measurement mid-flight.
//
// Memory management: call StartSweeper once at startup to automatically evict
// completed/failed tasks older than a configurable TTL. Individual tasks can
// also be removed on demand via Delete.
package store

import (
	"context"
	"log"
	"sync"
	"time"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

// isTerminal reports whether s is a terminal (non-running) state.
func isTerminal(s Status) bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCanceled
}

// Task holds the lifecycle state of a single measurement run.
type Task struct {
	ID         string      `json:"task_id"`
	Status     Status      `json:"status"`
	CreatedAt  time.Time   `json:"created_at"`
	FinishedAt *time.Time  `json:"finished_at,omitempty"`
	Duration   string      `json:"duration,omitempty"`
	Result     interface{} `json:"result,omitempty"`
	Error      string      `json:"error,omitempty"`
}

// Store is a thread-safe container for task state and cancellation handles.
type Store struct {
	tasks   sync.Map // taskID → *Task
	cancels sync.Map // taskID → context.CancelFunc
}

func New() *Store { return &Store{} }

// ── Task operations ───────────────────────────────────────────────────────────

// Set stores (or replaces) a task. A defensive copy is stored so the caller
// retains ownership of its pointer and concurrent readers of the stored value
// are never exposed to in-place writes. When the task status is terminal and
// FinishedAt has not been set yet, it is stamped with the current UTC time so
// the TTL sweeper has an accurate age reference.
func (s *Store) Set(id string, t *Task) {
	stored := *t // defensive copy — isolates stored pointer from caller's pointer
	if isTerminal(stored.Status) && stored.FinishedAt == nil {
		now := time.Now().UTC()
		stored.FinishedAt = &now
	}
	s.tasks.Store(id, &stored)
}

// Get returns a copy of the stored task so callers cannot mutate shared state.
func (s *Store) Get(id string) (*Task, bool) {
	v, ok := s.tasks.Load(id)
	if !ok {
		return nil, false
	}
	t := *v.(*Task)
	return &t, true
}

// Delete removes a task and its associated cancel func from both maps.
// Safe to call when the id is absent (no-op).
func (s *Store) Delete(id string) {
	s.tasks.Delete(id)
	s.cancels.Delete(id)
}

// Sweep scans the task map and deletes every terminal task whose FinishedAt is
// older than ttl. It returns the number of tasks removed and the number of
// tasks still remaining in the map after the sweep.
func (s *Store) Sweep(ttl time.Duration) (removed, remaining int) {
	now := time.Now()
	var toDelete []string
	var total int

	s.tasks.Range(func(key, value any) bool {
		total++
		t := value.(*Task)
		if isTerminal(t.Status) && t.FinishedAt != nil && now.Sub(*t.FinishedAt) > ttl {
			toDelete = append(toDelete, key.(string))
		}
		return true
	})

	for _, id := range toDelete {
		s.tasks.Delete(id)
		s.cancels.Delete(id)
	}

	removed = len(toDelete)
	remaining = total - removed
	return
}

// StartSweeper launches a background goroutine that calls Sweep(ttl) every
// interval. It stops when ctx is cancelled. Call this once at server startup.
func (s *Store) StartSweeper(ctx context.Context, interval, ttl time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				removed, remaining := s.Sweep(ttl)
				if removed > 0 {
					log.Printf("[Sweeper] Cleaned up %d old tasks. Current map size: %d", removed, remaining)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

// ── Cancel-func operations ────────────────────────────────────────────────────

// SetCancel stores the CancelFunc associated with a task.
// Called once, immediately before launching the background goroutine.
func (s *Store) SetCancel(id string, fn context.CancelFunc) {
	s.cancels.Store(id, fn)
}

// Cancel invokes and removes the CancelFunc for the given task in one atomic
// step (LoadAndDelete).  Returns true if a live cancel func was found.
// Returns false when the task has already finished (naturally or was already
// cancelled) — the cancel func is removed by DeleteCancel at that point.
func (s *Store) Cancel(id string) bool {
	v, ok := s.cancels.LoadAndDelete(id)
	if !ok {
		return false
	}
	v.(context.CancelFunc)()
	return true
}

// DeleteCancel removes the CancelFunc without calling it.
// Deferred inside executor.Run so the entry is always cleaned up when the
// goroutine exits, even on panic.  Safe to call when the key is missing.
func (s *Store) DeleteCancel(id string) {
	s.cancels.Delete(id)
}
