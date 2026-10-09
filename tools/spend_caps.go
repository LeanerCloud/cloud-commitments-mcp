package tools

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
)

// Operator-set spend caps. They are environment variables, not tool
// arguments: a cap the model could raise would not bound a hallucinating or
// prompt-injected model. Each applies PER CALL, not cumulatively, and
// none is a USD cap: they bound the size of one purchase request.
const (
	// EnvMaxCount caps rec.Count (instances or nodes; vCPUs for GCP CUDs)
	// for every non-Savings-Plan real purchase.
	EnvMaxCount = "CUDLY_MCP_MAX_COUNT"
	// EnvMaxHourlyCommitment caps the USD/hour commitment of a Savings Plan.
	EnvMaxHourlyCommitment = "CUDLY_MCP_MAX_HOURLY_COMMITMENT"
	// EnvMaxMemoryGB caps the memory committed by a GCP CUD. MaxCount bounds
	// only vcpu_count, so memory_gb needs its own ceiling.
	EnvMaxMemoryGB = "CUDLY_MCP_MAX_MEMORY_GB"
)

// capSetting names one cap for error messages: the environment variable and,
// for the MCPB bundle, the Claude Desktop setting that feeds it.
type capSetting struct {
	env     string
	desktop string
}

var (
	capCount  = capSetting{EnvMaxCount, "Maximum count per purchase call"}
	capHourly = capSetting{EnvMaxHourlyCommitment, "Maximum hourly commitment (USD/hour)"}
	capMemory = capSetting{EnvMaxMemoryGB, "Maximum committed memory (GB)"}
)

func (c capSetting) unset() error {
	return fmt.Errorf("refusing real purchase: spend cap %s is not set; set it (Claude Desktop: %q) to the largest value one purchase call may use",
		c.env, c.desktop)
}

func (c capSetting) invalid(kind string) error {
	return fmt.Errorf("refusing real purchase: spend cap %s must be a positive %s (Claude Desktop: %q)",
		c.env, kind, c.desktop)
}

// readCapRaw returns the trimmed value of the cap variable, or the unset error
// for both an unset and an empty/whitespace-only variable.
func (c capSetting) readRaw() (string, error) {
	v := strings.TrimSpace(os.Getenv(c.env))
	if v == "" {
		return "", c.unset()
	}
	return v, nil
}

func (c capSetting) readInt() (int, error) {
	v, err := c.readRaw()
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, c.invalid("integer")
	}
	return n, nil
}

func (c capSetting) readFloat() (float64, error) {
	v, err := c.readRaw()
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, c.invalid("number")
	}
	return f, nil
}

// enforceSpendCaps refuses a real purchase whose size exceeds the operator
// caps, or whose cap is not configured. Caps are routed by CommitmentType, not
// by Count==0, so a Savings Plan can never skip its hourly cap by carrying a
// zero count. Fail closed: a missing, empty or invalid cap is a refusal, never
// "unlimited".
func enforceSpendCaps(rec common.Recommendation) error {
	if rec.CommitmentType == common.CommitmentSavingsPlan {
		return enforceHourlyCap(rec)
	}
	if err := enforceCountCap(rec); err != nil {
		return err
	}
	if rec.CommitmentType == common.CommitmentCUD {
		return enforceMemoryCap(rec)
	}
	return nil
}

func enforceHourlyCap(rec common.Recommendation) error {
	details, ok := rec.Details.(*common.SavingsPlanDetails)
	if !ok || details == nil {
		return fmt.Errorf("refusing real purchase: Savings Plan recommendation carries no hourly commitment to check against %s", EnvMaxHourlyCommitment)
	}
	limit, err := capHourly.readFloat()
	if err != nil {
		return err
	}
	h := details.HourlyCommitment
	if math.IsNaN(h) || math.IsInf(h, 0) || h <= 0 {
		return fmt.Errorf("refusing real purchase: hourly commitment must be a positive finite number")
	}
	if h > limit {
		return fmt.Errorf("refusing real purchase: hourly_commitment %v exceeds the operator cap %s=%v", h, EnvMaxHourlyCommitment, limit)
	}
	return nil
}

func enforceCountCap(rec common.Recommendation) error {
	limit, err := capCount.readInt()
	if err != nil {
		return err
	}
	if rec.Count < 1 {
		return fmt.Errorf("refusing real purchase: count must be at least 1, got %d", rec.Count)
	}
	if rec.Count > limit {
		return fmt.Errorf("refusing real purchase: count %d exceeds the operator cap %s=%d", rec.Count, EnvMaxCount, limit)
	}
	return nil
}

func enforceMemoryCap(rec common.Recommendation) error {
	details, ok := rec.Details.(common.ComputeDetails)
	if !ok {
		return fmt.Errorf("refusing real purchase: GCP CUD recommendation carries no memory to check against %s", EnvMaxMemoryGB)
	}
	limit, err := capMemory.readFloat()
	if err != nil {
		return err
	}
	m := details.MemoryGB
	if math.IsNaN(m) || math.IsInf(m, 0) || m <= 0 {
		return fmt.Errorf("refusing real purchase: memory_gb must be a positive finite number")
	}
	if m > limit {
		return fmt.Errorf("refusing real purchase: memory_gb %v exceeds the operator cap %s=%v", m, EnvMaxMemoryGB, limit)
	}
	return nil
}
