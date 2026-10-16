package tools

import (
	"fmt"
	"sync"

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

// budgetReservation holds part of the budget while a purchase is in flight.
type budgetReservation struct {
	ledger *budgetLedger
	usd    float64
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
	return &budgetReservation{ledger: l, usd: usd}, nil
}

// settle moves the reservation into committed when spent, otherwise releases it.
func (r *budgetReservation) settle(spent bool) {
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
// must stay counted. Only a clean no-op (nil error, no success, no error) or
// an adopted existing commitment is released; every error and any other
// result may have spent money, so it stays counted.
func purchaseSpent(result common.PurchaseResult, err error) bool {
	if result.ExistingCommitment {
		return false
	}
	return err != nil || result.Success || result.Error != nil
}
