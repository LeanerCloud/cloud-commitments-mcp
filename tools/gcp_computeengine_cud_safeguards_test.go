package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/recfilter"
	"github.com/LeanerCloud/cloud-commitments-go/providers/gcp/services/computeengine"
)

// fakeGCPCommitmentsService implements computeengine.CommitmentsService (the
// exported library seam for tests; the library's own MockCommitmentsService is
// test-only) so a real computeengine.Client can run its purchase and
// existing-commitment code paths without any GCP credentials or network.
type fakeGCPCommitmentsService struct {
	commitments []*computepb.Commitment
	inserts     []*computepb.InsertRegionCommitmentRequest
}

func (f *fakeGCPCommitmentsService) List(_ context.Context, _ *computepb.ListRegionCommitmentsRequest) computeengine.CommitmentsIterator {
	return &fakeGCPCommitmentsIterator{commitments: f.commitments}
}

func (f *fakeGCPCommitmentsService) Insert(_ context.Context, req *computepb.InsertRegionCommitmentRequest) (computeengine.CommitmentsOperation, error) {
	f.inserts = append(f.inserts, req)
	return &fakeGCPCommitmentsOperation{}, nil
}

func (f *fakeGCPCommitmentsService) Get(_ context.Context, _ *computepb.GetRegionCommitmentRequest) (*computepb.Commitment, error) {
	return nil, errors.New("fakeGCPCommitmentsService: unexpected Get call")
}

func (f *fakeGCPCommitmentsService) Close() error { return nil }

type fakeGCPCommitmentsIterator struct {
	commitments []*computepb.Commitment
	idx         int
}

func (it *fakeGCPCommitmentsIterator) Next() (*computepb.Commitment, error) {
	if it.idx >= len(it.commitments) {
		return nil, iterator.Done
	}
	c := it.commitments[it.idx]
	it.idx++
	return c, nil
}

type fakeGCPCommitmentsOperation struct{}

func (o *fakeGCPCommitmentsOperation) Wait(_ context.Context, _ ...gax.CallOption) error { return nil }

// TestGCPComputeEnginePurchaseReportsExplicitZeroCost is the pin-sensitive
// regression guard at the library boundary for cloud-commitments-go's
// purchase-cost safeguard: a real Compute Engine CUD purchase must report an
// explicit zero upfront cost, not a fabricated one copied from the
// recommendation. At the pre-safeguard gcp pin (ce95136) PurchaseCommitment
// returned result.Cost = &rec.CommitmentCost, so a recommendation priced at
// 1000 surfaced as a $1000 upfront charge that was never paid; at a32fd1a and
// later it returns a non-nil pointer to 0.0 (CUDs are billed monthly, with no
// upfront charge), which marshals to an explicit "cost":0.
func TestGCPComputeEnginePurchaseReportsExplicitZeroCost(t *testing.T) {
	ctx := context.Background()

	client, err := computeengine.NewClient(ctx, "test-project", "us-central1")
	require.NoError(t, err)
	fake := &fakeGCPCommitmentsService{}
	client.SetCommitmentsService(fake)

	rec, _, _, _, err := gcpComputeEngineRecommendationFromArgs(gcpComputeEngineCUDPurchaseArgs{
		Region:      "us-central1",
		MachineType: "n2-standard-4",
		VCPUCount:   4,
		MemoryGB:    16,
		TermYears:   1,
	})
	require.NoError(t, err)
	// A nonzero commitment estimate is exactly the input the old pin leaked
	// into the purchase result; the safeguard zeroes it.
	rec.CommitmentCost = 1000

	result, err := client.PurchaseCommitment(ctx, rec, common.PurchaseOptions{Source: common.PurchaseSourceMCP})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.NotEmpty(t, fake.inserts, "the fake Insert must have been called")

	// Assert on the wire form, not just the Go value: a caller keys off the
	// JSON, and the safeguard contract is an explicit zero there, not an
	// omitted or fabricated field.
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(encoded, &wire))
	cost, present := wire["cost"]
	require.True(t, present, "purchase result JSON must carry an explicit cost field, got %s", encoded)
	assert.Equal(t, 0.0, cost, "a Compute Engine CUD purchase has no upfront charge; the cost must be an explicit zero, got %s", encoded)
}

// TestGCPComputeEngineDedupeDefersFamilyWithRecentCUD is the pin-sensitive
// regression guard at the library boundary for the GCP dedupe safeguard: a
// recommendation whose machine family is covered by a recently purchased
// ACTIVE CUD in the same project and region must be filtered out, not passed
// through for a second purchase. At the pre-safeguard gcp pin (ce95136) the
// client never populated StartDate (so filterRecentCommitments dropped every
// real CUD from the lookback window) and exposed no
// FilterRecommendationsForRecentCommitments, so this recommendation passed
// through; at a32fd1a the family pool match filters it.
//
// This pins the library boundary; the search tool's own use of the checker
// is covered in search_dedupe_test.go.
func TestGCPComputeEngineDedupeDefersFamilyWithRecentCUD(t *testing.T) {
	ctx := context.Background()

	client, err := computeengine.NewClient(ctx, "test-project", "us-central1")
	require.NoError(t, err)

	// A well-formed recent ACTIVE CUD: non-nil name, a known commitment type
	// covering the n2 family, an explicit positive VCPU amount (the new
	// collectCommitments errors on malformed records), a StartTimestamp inside
	// the 24h lookback, and Status ACTIVE, or filterRecentCommitments drops it
	// and the test would go green for the wrong reason.
	fake := &fakeGCPCommitmentsService{
		commitments: []*computepb.Commitment{
			{
				Name:           proto.String("existing-n2-cud"),
				Type:           proto.String(computepb.Commitment_GENERAL_PURPOSE_N2.String()),
				Status:         proto.String(computepb.Commitment_ACTIVE.String()),
				StartTimestamp: proto.String(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)),
				Resources: []*computepb.ResourceCommitment{
					{Type: proto.String(computepb.ResourceCommitment_VCPU.String()), Amount: proto.Int64(8)},
					{Type: proto.String(computepb.ResourceCommitment_MEMORY.String()), Amount: proto.Int64(32768)},
				},
			},
		},
	}
	client.SetCommitmentsService(fake)

	rec, _, _, _, err := gcpComputeEngineRecommendationFromArgs(gcpComputeEngineCUDPurchaseArgs{
		Region:      "us-central1",
		MachineType: "n2-standard-4",
		VCPUCount:   4,
		MemoryGB:    16,
		TermYears:   1,
	})
	require.NoError(t, err)
	// Pool keying matches on {Account, Region, family}: the recommendation's
	// Account must name the same project the client lists commitments for.
	rec.Account = "test-project"

	passed, filtered, err := recfilter.NewDuplicateChecker(24).AdjustRecommendationsForExisting(ctx, []common.Recommendation{rec}, client)
	require.NoError(t, err)
	assert.Empty(t, passed, "a recommendation covered by a recent in-family CUD must not pass through to purchase")
	require.Len(t, filtered, 1, "the in-family recommendation must be filtered against the recent CUD")
	assert.Equal(t, "n2-standard-4", filtered[0].ResourceType)
}
