package tools

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
)

// budgetLedger is the per-process cumulative purchase budget. It lives in
// memory only: it resets on restart, is not shared across processes or MCP
// clients, and cannot see purchases made elsewhere.
type budgetLedger struct {
	mu        sync.Mutex
	committed float64 // USD of purchases that were, or may have been, made
	reserved  float64 // USD of purchases in flight
}

var processBudget = &budgetLedger{}

// EnvBudgetState names the optional state file that persists the cumulative
// budget. Set, the budget is per UTC calendar month, survives restarts and is
// shared by every process using the same path (same directory and permission
// rules as EnvAuditLog: parents created 0700, file 0600). Unset or blank, the
// budget stays the in-memory per-process ledger above.
const EnvBudgetState = "CUDLY_MCP_BUDGET_STATE"

// budgetNow is the clock for the persisted budget's month; tests replace it.
var budgetNow = time.Now

// reserveBudget holds usd against limit in the persisted budget when
// EnvBudgetState is set and in the process ledger otherwise.
func reserveBudget(usd, limit float64) (*budgetReservation, error) {
	path := strings.TrimSpace(os.Getenv(EnvBudgetState))
	if path == "" {
		return processBudget.reserve(usd, limit)
	}
	fb := newFileBudget(path)
	fb.now = budgetNow
	return fb.reserve(usd, limit)
}

// budgetReservation holds part of the budget while a purchase is in flight.
type budgetReservation struct {
	ledger    *budgetLedger // in-memory mode
	file      *fileBudget   // persisted mode (EnvBudgetState)
	id        string        // file mode: reservation ID in the state file
	usd       float64
	committed float64 // USD settled before this reservation, for logging
}

// reserve refuses when committed+reserved+usd would exceed limit (equal is
// allowed) and otherwise holds usd until settle.
func (l *budgetLedger) reserve(usd, limit float64) (*budgetReservation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.committed+l.reserved+usd > limit {
		return nil, fmt.Errorf("refusing real purchase: %.2f USD would exceed the cumulative budget %s=%.2f (%.2f USD remaining; the budget is per process and resets on restart)",
			usd, EnvBudgetUSD, limit, max(0, limit-l.committed-l.reserved))
	}
	l.reserved += usd
	return &budgetReservation{ledger: l, usd: usd, committed: l.committed}, nil
}

// settle moves the reservation into committed when spent, otherwise releases it.
func (r *budgetReservation) settle(spent bool) {
	if r.file != nil {
		r.file.settle(r.id, r.usd, spent)
		return
	}
	r.ledger.mu.Lock()
	defer r.ledger.mu.Unlock()
	r.ledger.reserved -= r.usd
	if spent {
		r.ledger.committed += r.usd
	}
}

func (l *budgetLedger) committedUSD() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.committed
}

// purchaseSpent reports whether a purchase that reached PurchaseCommitment
// must stay counted. A clean no-op (nil error, no success, no error) or an
// adopted existing commitment returned with no error is released. A failure is
// released only when every error carries common.ErrRefusedBeforePurchase, none
// carries ErrOutcomeUnknown, and the result shows no success or commitment ID:
// the provider refused before any purchase call. Any other error, including
// one returned beside ExistingCommitment, may have spent money and stays counted.
func purchaseSpent(result common.PurchaseResult, err error) bool {
	if err != nil || result.Error != nil {
		return !refusedBeforePurchase(result, err)
	}
	if result.ExistingCommitment {
		return false
	}
	return result.Success
}

func refusedBeforePurchase(result common.PurchaseResult, err error) bool {
	if result.Success || result.CommitmentID != "" || result.ExistingCommitment {
		return false
	}
	for _, e := range []error{err, result.Error} {
		if e == nil {
			continue
		}
		if !errors.Is(e, common.ErrRefusedBeforePurchase) || errors.Is(e, common.ErrOutcomeUnknown) {
			return false
		}
	}
	return true
}
