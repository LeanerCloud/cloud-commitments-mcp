package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// budgetStateVersion is bumped when the file format changes; an unknown
// version is treated as corrupt rather than guessed at.
const budgetStateVersion = 1

const budgetLockTimeout = 10 * time.Second

// budgetState is the persisted file. Month is the UTC calendar month
// (YYYY-MM) the amounts belong to. SpentUSD is settled spend; every entry in
// Reservations is a purchase that was started and not settled, and counts as
// spent until an operator resets the file.
type budgetState struct {
	Month        string              `json:"month"`
	Reservations []budgetReservEntry `json:"reservations"`
	SpentUSD     float64             `json:"spent_usd"`
	Version      int                 `json:"version"`
}

type budgetReservEntry struct {
	ID  string  `json:"id"`
	USD float64 `json:"usd"`
}

// totalUSD is spend plus every unsettled reservation.
func (s budgetState) totalUSD() float64 {
	total := s.SpentUSD
	for _, r := range s.Reservations {
		total += r.USD
	}
	return total
}

func validBudgetAmount(v float64) bool { return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

func (s budgetState) validate() error {
	if s.Version != budgetStateVersion {
		return fmt.Errorf("unsupported version %d", s.Version)
	}
	if _, err := time.Parse("2006-01", s.Month); err != nil {
		return fmt.Errorf("invalid month %q: %w", s.Month, err)
	}
	if !validBudgetAmount(s.SpentUSD) {
		return fmt.Errorf("invalid spent_usd %v", s.SpentUSD)
	}
	seen := make(map[string]bool, len(s.Reservations))
	for _, r := range s.Reservations {
		if r.ID == "" || !validBudgetAmount(r.USD) {
			return fmt.Errorf("invalid reservation %+v", r)
		}
		if seen[r.ID] {
			return fmt.Errorf("duplicate reservation id %q", r.ID)
		}
		seen[r.ID] = true
	}
	return nil
}

// fileBudget is the cumulative budget persisted in a state file shared by
// every server process that points at the same path. All access is a
// read-modify-write under an flock on path+".lock".
type fileBudget struct {
	now  func() time.Time
	path string
}

func newFileBudget(path string) *fileBudget {
	return &fileBudget{path: path, now: time.Now}
}

func (f *fileBudget) month() string { return f.now().UTC().Format("2006-01") }

// update runs fn on the current state under the lock and persists the result
// when fn returns write=true. A state from an earlier month is replaced by an
// empty one for the current month before fn sees it. A missing file is an
// empty state; any other read or parse problem is an error (fail closed).
func (f *fileBudget) update(fn func(*budgetState) (write bool, err error)) error {
	if err := ensureAuditLogDirectory(f.path); err != nil {
		return fmt.Errorf("prepare budget state directory for %s: %w", f.path, err)
	}
	unlock, err := lockBudgetFile(f.path+".lock", budgetLockTimeout)
	if err != nil {
		return err
	}
	defer unlock()

	st, err := f.load()
	if err != nil {
		return err
	}
	rolled := false
	// YYYY-MM compares chronologically as a string. A file from a later month
	// (clock skew or a future-dated file) is never rolled back or rewritten.
	switch m := f.month(); {
	case st.Month > m:
		return fmt.Errorf("budget state file %s is for %s but the current UTC month is %s; refusing instead of resetting (fix the clock or the file)", f.path, st.Month, m)
	case st.Month < m:
		// New month: spend restarts at 0, but unsettled reservations may have
		// bought something, so they keep counting until settled or reset.
		st, rolled = budgetState{Version: budgetStateVersion, Month: m, Reservations: st.Reservations}, true
	}
	write, err := fn(&st)
	if err != nil {
		return err
	}
	if write || rolled {
		return f.save(st)
	}
	return nil
}

func (f *fileBudget) load() (budgetState, error) {
	data, err := os.ReadFile(f.path) // #nosec G304 G703 -- operator-set path, not tool input
	if errors.Is(err, fs.ErrNotExist) {
		return budgetState{Version: budgetStateVersion}, nil
	}
	if err != nil {
		return budgetState{}, fmt.Errorf("read budget state file %s: %w", f.path, err)
	}
	var st budgetState
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return budgetState{}, fmt.Errorf("budget state file %s is corrupt: %w", f.path, err)
	}
	if dec.More() {
		return budgetState{}, fmt.Errorf("budget state file %s is corrupt: trailing data", f.path)
	}
	if err := st.validate(); err != nil {
		return budgetState{}, fmt.Errorf("budget state file %s is corrupt: %w", f.path, err)
	}
	return st, nil
}

// save replaces the file atomically: temp file in the same directory, fsync,
// rename, then fsync of the directory so the rename survives a crash.
func (f *fileBudget) save(st budgetState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode budget state: %w", err)
	}
	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(f.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create budget state temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(append(data, '\n'))
	serr := tmp.Sync()
	cerr := tmp.Close()
	if jerr := errors.Join(werr, serr, cerr); jerr != nil {
		_ = os.Remove(tmpName) // #nosec G703 -- temp file we created in the operator-set directory
		return fmt.Errorf("write budget state temp file %s: %w", tmpName, jerr)
	}
	if rerr := os.Rename(tmpName, f.path); rerr != nil { // #nosec G703 -- operator-set path, not tool input
		_ = os.Remove(tmpName) // #nosec G703 -- temp file we created in the operator-set directory
		return fmt.Errorf("replace budget state file %s: %w", f.path, rerr)
	}
	d, err := os.Open(dir) // #nosec G304 G703 -- parent of the operator-set path
	if err != nil {
		return fmt.Errorf("open budget state directory %s: %w", dir, err)
	}
	return errors.Join(d.Sync(), d.Close())
}

// reserve refuses when the month's spend plus reservations plus usd would
// exceed limit (equal is allowed) and otherwise records a reservation before
// returning, so the hold is on disk before the purchase call.
func (f *fileBudget) reserve(usd, limit float64) (*budgetReservation, error) {
	id := uuid.NewString()
	var committed float64
	exceeded := false
	err := f.update(func(st *budgetState) (bool, error) {
		if st.totalUSD()+usd > limit {
			exceeded = true
			return false, fmt.Errorf("refusing real purchase: %.2f USD would exceed the cumulative budget %s=%.2f (%.2f USD remaining in %s UTC; state file %s, resets next calendar month)",
				usd, EnvBudgetUSD, limit, max(0, limit-st.totalUSD()), st.Month, f.path)
		}
		st.Reservations = append(st.Reservations, budgetReservEntry{ID: id, USD: usd})
		committed = st.SpentUSD
		return true, nil
	})
	if err != nil && !exceeded {
		return nil, fmt.Errorf("refusing real purchase: the persisted budget %s is unusable, so spend cannot be checked: %w", EnvBudgetState, err)
	}
	if err != nil {
		return nil, err
	}
	return &budgetReservation{file: f, id: id, usd: usd, committed: committed}, nil
}

// settle moves the reservation into spent when spent, otherwise releases it.
// A reservation missing from the file (the month rolled over, or an operator
// reset, while the purchase was in flight) still adds to the current month
// when money was spent. A failed write cannot undo the purchase: the
// reservation then stays on disk and keeps counting, which errs toward
// refusing.
func (f *fileBudget) settle(id string, usd float64, spent bool) {
	err := f.update(func(st *budgetState) (bool, error) {
		for i, r := range st.Reservations {
			if r.ID == id {
				st.Reservations = append(st.Reservations[:i], st.Reservations[i+1:]...)
				break
			}
		}
		if spent {
			st.SpentUSD += usd
		}
		return true, nil
	})
	if err != nil {
		log.Printf("mcp budget: settle %.2f USD (spent=%t) in %s failed, reservation stays counted: %v", usd, spent, f.path, err)
	}
}
