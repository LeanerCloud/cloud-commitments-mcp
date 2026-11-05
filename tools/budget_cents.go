package tools

import (
	"fmt"
	"math/big"
	"strconv"
)

// usdCents is a budget amount in whole US cents. The cumulative budget adds
// and compares cents so float residuals cannot flip a boundary check (#82).
// In JSON it is a USD number ("spent_usd": 60.25), the format older releases
// wrote, so the state file stays readable across versions.
type usdCents int64

// maxBudgetUSD bounds every budget amount. Below it, cents/100 formats to its
// exact decimal, so a state file round-trips without loss.
const maxBudgetUSD = 1e13

const maxBudgetCents = usdCents(maxBudgetUSD * 100)

// spendCents converts a spend amount, rounding up so spend is never
// undercounted. Half-even or half-up rounding could drop up to half a cent.
func spendCents(usd float64) (usdCents, error) {
	return toCents(usd, true)
}

// limitCents converts a budget limit, rounding down, so rounding can only
// refuse earlier than exact arithmetic, never admit more.
func limitCents(usd float64) (usdCents, error) {
	return toCents(usd, false)
}

// toCents converts exactly, from the shortest decimal that prints usd (0.07 is
// 7 cents, although 0.07*100 is 7.000000000000001 in float64). NaN and ±Inf
// print as words that do not parse.
func toCents(usd float64, roundUp bool) (usdCents, error) {
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(usd, 'g', -1, 64))
	if !ok {
		return 0, fmt.Errorf("budget amount %v is not a finite number", usd)
	}
	return ratCents(r, roundUp)
}

func ratCents(r *big.Rat, roundUp bool) (usdCents, error) {
	if r.Sign() < 0 || r.Cmp(new(big.Rat).SetFloat64(maxBudgetUSD)) > 0 {
		return 0, fmt.Errorf("budget amount %s USD is outside 0 to %.0f", r.FloatString(2), float64(maxBudgetUSD))
	}
	c := new(big.Rat).Mul(r, big.NewRat(100, 1))
	q, m := new(big.Int).QuoRem(c.Num(), c.Denom(), new(big.Int))
	if roundUp && m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return usdCents(q.Int64()), nil
}

// String formats the amount as USD with two decimals.
func (c usdCents) String() string { return fmt.Sprintf("%d.%02d", c/100, c%100) }

func (c usdCents) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatFloat(float64(c)/100, 'f', -1, 64)), nil
}

// UnmarshalJSON reads the number literal exactly and rounds it up to cents, so
// a non-cent amount in a file written before #82 is never undercounted. null,
// a quoted number or a negative or out-of-range amount is an error (encoding/json
// passes the raw token, and none of those parse as a big.Rat in range).
func (c *usdCents) UnmarshalJSON(b []byte) error {
	r, ok := new(big.Rat).SetString(string(b))
	if !ok {
		return fmt.Errorf("budget amount %s is not a number", b)
	}
	v, err := ratCents(r, true)
	if err != nil {
		return err
	}
	*c = v
	return nil
}
