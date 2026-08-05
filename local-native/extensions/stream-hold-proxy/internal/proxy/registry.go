package proxy

import (
	"sort"
	"sync"
	"time"
)

type holdSnapshot struct {
	RequestID  string     `json:"request_id"`
	Method     string     `json:"method"`
	Path       string     `json:"path"`
	StartedAt  time.Time  `json:"started_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	Attempts   int        `json:"attempts"`
	Phase      string     `json:"phase"`
	LastStatus int        `json:"last_status,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
	NextRetry  *time.Time `json:"next_retry_at,omitempty"`
}

type holdRegistry struct {
	mu     sync.RWMutex
	active map[string]holdSnapshot
}

func newHoldRegistry() *holdRegistry {
	return &holdRegistry{active: make(map[string]holdSnapshot)}
}

func (r *holdRegistry) start(requestID, method, path string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active[requestID] = holdSnapshot{
		RequestID: requestID,
		Method:    method,
		Path:      path,
		StartedAt: now.UTC(),
		UpdatedAt: now.UTC(),
		Phase:     "starting",
	}
}

func (r *holdRegistry) attempt(requestID string, attempt int, now time.Time) {
	r.update(requestID, func(state *holdSnapshot) {
		state.Attempts = attempt
		state.Phase = "requesting"
		state.UpdatedAt = now.UTC()
		state.NextRetry = nil
	})
}

func (r *holdRegistry) failed(requestID string, status int, message string, nextRetry time.Time) {
	r.update(requestID, func(state *holdSnapshot) {
		state.Phase = "holding"
		state.LastStatus = status
		state.LastError = truncate(message, 512)
		state.UpdatedAt = time.Now().UTC()
		next := nextRetry.UTC()
		state.NextRetry = &next
	})
}

func (r *holdRegistry) finish(requestID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.active, requestID)
}

func (r *holdRegistry) list() []holdSnapshot {
	r.mu.RLock()
	result := make([]holdSnapshot, 0, len(r.active))
	for _, state := range r.active {
		result = append(result, state)
	}
	r.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool {
		return result[i].StartedAt.Before(result[j].StartedAt)
	})
	return result
}

func (r *holdRegistry) update(requestID string, update func(*holdSnapshot)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, exists := r.active[requestID]
	if !exists {
		return
	}
	update(&state)
	r.active[requestID] = state
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
