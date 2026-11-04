package tools

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #82: the ledger adds and compares whole cents, so float residuals cannot
// flip an exact-boundary check.
func TestBudgetLedgerExactBoundary(t *testing.T) {
	l := &budgetLedger{}
	_, err := l.reserve(0.1, 0.3)
	require.NoError(t, err)
	_, err = l.reserve(0.2, 0.3)
	require.NoError(t, err, "0.10 + 0.20 is exactly the 0.30 budget")
}

func TestBudgetLedgerReleaseLeavesNoResidual(t *testing.T) {
	l := &budgetLedger{}
	a, err := l.reserve(0.1, 0.3)
	require.NoError(t, err)
	b, err := l.reserve(0.2, 0.3)
	require.NoError(t, err)
	a.settle(false)
	b.settle(false)
	_, err = l.reserve(0.3, 0.3)
	require.NoError(t, err, "after releasing everything the whole budget is free")
}

// Spend rounds up and the limit rounds down, so rounding never admits more
// than exact arithmetic would.
func TestBudgetLedgerRoundsTowardRefusing(t *testing.T) {
	l := &budgetLedger{}
	_, err := l.reserve(0.011, 0.02) // 2 cents of 2
	require.NoError(t, err)
	_, err = l.reserve(0.001, 0.02)
	require.Error(t, err, "a sub-cent spend counts as a whole cent")

	l = &budgetLedger{}
	_, err = l.reserve(0.01, 0.019) // the limit is 1 cent
	require.NoError(t, err)
	_, err = l.reserve(0.001, 0.019)
	require.Error(t, err, "a sub-cent budget remainder is not spendable")
}

func TestBudgetLedgerRefusesUnrepresentableAmounts(t *testing.T) {
	for _, usd := range []float64{1e14, math.NaN(), math.Inf(1), -1} {
		_, err := (&budgetLedger{}).reserve(usd, maxBudgetUSD)
		assert.Error(t, err, "spend %v", usd)
	}
	for _, limit := range []float64{maxBudgetUSD + 1, math.Inf(1), math.NaN()} {
		_, err := (&budgetLedger{}).reserve(1, limit)
		assert.Error(t, err, "limit %v", limit)
	}
}

func TestSpendAndLimitCents(t *testing.T) {
	for _, c := range []struct {
		usd        float64
		spend, lim usdCents
	}{
		{0, 0, 0}, {0.07, 7, 7}, {0.29, 29, 29}, {1.005, 101, 100}, {0.001, 1, 0}, {60, 6000, 6000},
		{1e-300, 1, 0}, {maxBudgetUSD, maxBudgetUSD * 100, maxBudgetUSD * 100},
	} {
		s, err := spendCents(c.usd)
		require.NoError(t, err, c.usd)
		assert.Equal(t, c.spend, s, "spend %v", c.usd)
		l, err := limitCents(c.usd)
		require.NoError(t, err, c.usd)
		assert.Equal(t, c.lim, l, "limit %v", c.usd)
	}
	_, err := spendCents(maxBudgetUSD + 0.01)
	assert.Error(t, err)
}

func TestUSDCentsJSON(t *testing.T) {
	for cents, want := range map[usdCents]string{0: "0", 30: "0.3", 6000: "60", 1234: "12.34", 5: "0.05", maxBudgetUSD * 100: "10000000000000"} {
		b, err := json.Marshal(cents)
		require.NoError(t, err)
		assert.Equal(t, want, string(b))
		var back usdCents
		require.NoError(t, json.Unmarshal(b, &back))
		assert.Equal(t, cents, back, "round trip %s", want)
	}
	for raw, want := range map[string]usdCents{"0.105": 11, "1e2": 10000, "0.30000000000000004": 31} {
		var got usdCents
		require.NoError(t, json.Unmarshal([]byte(raw), &got), raw)
		assert.Equal(t, want, got, "a legacy non-cent amount %s rounds up", raw)
	}
	for _, raw := range []string{`null`, `"1"`, `-1`, `true`, `1e14`} {
		var got usdCents
		assert.Error(t, json.Unmarshal([]byte(raw), &got), raw)
	}
}
