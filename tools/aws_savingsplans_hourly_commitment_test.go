package tools

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateHourlyCommitmentRejectsSubCentAmounts is the regression guard
// for the silent money-rounding found in review. The Savings Plans client
// renders CreateSavingsPlanInput.Commitment with %.2f
// (providers/aws/services/savingsplans/client.go), so before this check a
// caller asking for $0.004/hour silently committed to "0.00" and one asking
// for $10.005/hour silently committed to "10.01" -- in both cases spending
// real money on a figure they never asked for.
//
// The whole-cent cases matter just as much as the rejections: 0.07 and 0.29
// are not exactly representable in float64 (0.07*100 is 7.000000000000001),
// so a naive exact-integer-cents test would reject perfectly billable
// amounts and make the tool unusable for most real commitments.
func TestValidateHourlyCommitmentRejectsSubCentAmounts(t *testing.T) {
	t.Parallel()

	t.Run("whole-cent amounts are accepted", func(t *testing.T) {
		t.Parallel()
		for _, v := range []float64{0.01, 0.07, 0.29, 1, 5, 8.11, 10, 10.5, 12.34, 1000.99} {
			assert.NoError(t, validateHourlyCommitment(v), "%v is a whole number of cents and must be accepted", v)
		}
	})

	t.Run("sub-cent amounts are rejected naming what AWS would have charged", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			wantBilled string
			value      float64
		}{
			{value: 0.004, wantBilled: "0.00"},
			{value: 10.005, wantBilled: "10.01"},
			{value: 1.0 / 3.0, wantBilled: "0.33"},
			{value: 0.001, wantBilled: "0.00"},
		} {
			err := validateHourlyCommitment(tc.value)
			require.Error(t, err, "%v is finer than a cent and must be rejected", tc.value)
			assert.Contains(t, err.Error(), "finer than one cent")
			assert.Contains(t, err.Error(), tc.wantBilled,
				"the error must name the amount AWS would actually have been asked to commit")
		}
	})

	t.Run("non-positive amounts keep their own error", func(t *testing.T) {
		t.Parallel()
		for _, v := range []float64{0, -5, -0.01} {
			err := validateHourlyCommitment(v)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be > 0")
		}
	})

	// NaN and +Inf both fail the "<= 0" check (NaN compares false to
	// everything, +Inf compares true to > 0) and %.2f renders them as "NaN"
	// and "+Inf" respectively, both of which strconv.ParseFloat happily
	// re-parses -- so neither used to hit any rejection path. Pin that they
	// are now refused explicitly rather than silently passed through to a
	// real AWS purchase call.
	t.Run("NaN and Inf are rejected", func(t *testing.T) {
		t.Parallel()
		for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			err := validateHourlyCommitment(v)
			require.Error(t, err, "%v must be rejected as a non-finite commitment", v)
			assert.Contains(t, err.Error(), "finite")
		}
	})
}

// TestSavingsPlanRecommendationFromArgs_RejectsSubCentCommitment pins that
// the sub-cent guard is reachable through the tool's real validation entry
// point, not only when validateHourlyCommitment is called directly.
func TestSavingsPlanRecommendationFromArgs_RejectsSubCentCommitment(t *testing.T) {
	t.Parallel()
	args := validSavingsPlansArgs()
	args.HourlyCommitment = 10.005

	_, _, _, _, err := savingsPlanRecommendationFromArgs(args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "finer than one cent")
}
