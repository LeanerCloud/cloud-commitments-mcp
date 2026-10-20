package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
)

// attemptRetention is how long a first-attempt time is kept. The MCP has no
// idempotency horizon constant, so this is a chosen 30 days: far beyond any
// retry window, and a key older than that is a new purchase.
const attemptRetention = 30 * 24 * time.Hour

const attemptStateVersion = 1

// attemptState maps an idempotency token to the UTC time of its first real
// dispatch (PurchaseOptions.RequestedAt).
type attemptState struct {
	Attempts map[string]time.Time `json:"attempts"`
	Version  int                  `json:"version"`
}

var (
	processAttemptsMu sync.Mutex
	processAttempts   = map[string]time.Time{}
)

// firstAttemptTime returns the first-dispatch time for token, recording now
// durably when the key is new. cloud-commitments-go#515 finds an earlier
// Redshift purchase by start time relative to RequestedAt, so a retry must
// reuse the first attempt's time, not its own call time. The record is made
// before the provider is called. With EnvBudgetState set it lives in
// <state>.attempts under the same lock and atomic-write rules as the budget
// file; a corrupt or unwritable file is an error, never a fresh time. Without
// it the time is kept in process memory only (like the in-memory budget), and
// Redshift, whose dedupe depends on it surviving a restart, is refused.
func firstAttemptTime(token string, service common.ServiceType) (time.Time, error) {
	now := budgetNow().UTC()
	path := strings.TrimSpace(os.Getenv(EnvBudgetState))
	if path == "" {
		if service == common.ServiceRedshift {
			return time.Time{}, fmt.Errorf("refusing real purchase: Redshift reserved nodes cannot be tagged, so a retry is deduped by the first attempt's start time, which must survive a restart; set %s", EnvBudgetState)
		}
		processAttemptsMu.Lock()
		defer processAttemptsMu.Unlock()
		if t, ok := processAttempts[token]; ok {
			return t, nil
		}
		processAttempts[token] = now
		return now, nil
	}
	t, err := fileFirstAttempt(path+".attempts", token, now)
	if err != nil {
		return time.Time{}, fmt.Errorf("refusing real purchase: first-attempt state unusable: %w", err)
	}
	return t, nil
}

// fileFirstAttempt is firstAttemptTime for the persisted store at path.
func fileFirstAttempt(path, token string, now time.Time) (time.Time, error) {
	if err := ensureAuditLogDirectory(path); err != nil {
		return time.Time{}, fmt.Errorf("prepare directory for %s: %w", path, err)
	}
	unlock, err := lockBudgetFile(path+".lock", budgetLockTimeout)
	if err != nil {
		return time.Time{}, err
	}
	defer unlock()
	st, err := loadAttempts(path)
	if err != nil {
		return time.Time{}, err
	}
	for k, t := range st.Attempts {
		if now.Sub(t) > attemptRetention {
			delete(st.Attempts, k)
		}
	}
	if t, ok := st.Attempts[token]; ok {
		return t, nil
	}
	st.Attempts[token] = now
	return now, writeStateAtomically(path, st, "first-attempt state")
}

func loadAttempts(path string) (attemptState, error) {
	data, err := os.ReadFile(path) // #nosec G304 G703 -- operator-set path, not tool input
	if errors.Is(err, fs.ErrNotExist) {
		return attemptState{Version: attemptStateVersion, Attempts: map[string]time.Time{}}, nil
	}
	if err != nil {
		return attemptState{}, fmt.Errorf("read %s: %w", path, err)
	}
	var st attemptState
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil || dec.More() || st.Version != attemptStateVersion || st.Attempts == nil {
		return attemptState{}, fmt.Errorf("%s is corrupt", path)
	}
	for k, t := range st.Attempts {
		if k == "" || t.IsZero() {
			return attemptState{}, fmt.Errorf("%s is corrupt: invalid entry %q", path, k)
		}
	}
	return st, nil
}
