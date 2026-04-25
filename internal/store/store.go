// Package store provides a concurrency-safe in-memory task state store.
// It holds two parallel sync.Maps: one for task state, one for the
// CancelFunc that lets callers abort a running measurement mid-flight.
package store

import (
	"context"
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

// Task holds the lifecycle state of a single measurement run.
type Task struct {
	ID        string      `json:"task_id"`
	Status    Status      `json:"status"`
	CreatedAt time.Time   `json:"created_at"`
	Duration  string      `json:"duration,omitempty"`
	Result    interface{} `json:"result,omitempty"`
	Error     string      `json:"error,omitempty"`
}

// Store is a thread-safe container for task state and cancellation handles.
type Store struct {
	tasks   sync.Map // taskID → *Task
	cancels sync.Map // taskID → context.CancelFunc
}

func New() *Store { return &Store{} }

// ── Task operations ───────────────────────────────────────────────────────────

func (s *Store) Set(id string, t *Task) { s.tasks.Store(id, t) }

func (s *Store) Get(id string) (*Task, bool) {
	v, ok := s.tasks.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*Task), true
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
