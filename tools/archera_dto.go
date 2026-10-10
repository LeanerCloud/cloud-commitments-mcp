// This mapping mirrors cloud-commitments-platform internal/archera/dto.go field
// for field (same JSON names, notes and cleaning) so every consumer renders the
// same comparison.

package tools

import (
	"errors"
	"math/big"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
)

// ErrArcheraUnrepresentable is returned when a vendor number has no exact finite
// decimal form (the DTO never rounds or converts to float).
var ErrArcheraUnrepresentable = errors.New("archera value has no exact decimal representation")

const (
	dtoTitle         = "Archera commitment plan comparison (read-only)"
	currencyNote     = "currency not provided by Archera"
	unknownVerdict   = "unknown"
	maxVendorStrLen  = 256
	offerNameCaveat  = "Archera's name for this offer; a plan comparison is a hypothetical rollup, not a bindable quote, and an insured target is subject to Archera underwriting allowances, not a guarantee."
	basisNote        = "Monthly figures are 730-hour monthly rates; the Archera premium is already included in commitment cost totals; upfront figures are one-time amounts and are never summed with monthly rates."
	deltaBasisNote   = "Hypothetical deltas compare against the current plan, not against on-demand."
	supportedSummary = "supported"
)

// archeraDecimal renders r as the shortest exact decimal string. nil (unknown) stays
// nil. The denominator of a reduced rational is finite in decimal exactly when
// it has no prime factor other than 2 and 5, and the digits needed equal
// max(power of 2, power of 5).
func archeraDecimal(r *big.Rat) (*string, error) {
	if r == nil {
		return nil, nil
	}
	d := new(big.Int).Set(r.Denom())
	two, five, zero := big.NewInt(2), big.NewInt(5), new(big.Int)
	count := func(p *big.Int) int {
		n, m := 0, new(big.Int)
		for {
			q, rem := new(big.Int).QuoRem(d, p, m)
			if rem.Cmp(zero) != 0 {
				return n
			}
			d, n = q, n+1
		}
	}
	c2, c5 := count(two), count(five)
	if d.Cmp(big.NewInt(1)) != 0 {
		return nil, ErrArcheraUnrepresentable
	}
	s := r.FloatString(max(c2, c5))
	return &s, nil
}

// archeraCleanString removes control, format and line/paragraph-separator characters
// from vendor text and caps it at 256 bytes on a rune boundary.
func archeraCleanString(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > maxVendorStrLen {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// archeraProductSupportDTO is the documentation-derived verdict for one offer.
// Source and Evidence appear only for a supported verdict; allowance and
// customer eligibility are always unknown.
type archeraProductSupportDTO struct {
	Status              string `json:"status"`
	Source              string `json:"source,omitempty"`
	Evidence            string `json:"evidence,omitempty"`
	UnderwritingAllowed string `json:"underwriting_allowance"`
	CustomerEligibility string `json:"customer_eligibility"`
}

// archeraFinancialsDTO carries the 730-hour monthly rate block.
type archeraFinancialsDTO struct {
	CommitmentCostTotal *string `json:"monthly_730h_commitment_cost_total"`
	CloudProviderCost   *string `json:"monthly_730h_cloud_provider_cost"`
	Premium             *string `json:"monthly_730h_archera_premium"`
	GrossSavings        *string `json:"monthly_730h_gross_savings"`
	NetSavings          *string `json:"monthly_730h_net_savings"`
	CoveredOnDemandCost *string `json:"monthly_730h_covered_on_demand_cost"`
}

// archeraTotalsDTO is a plan-wide rollup.
type archeraTotalsDTO struct {
	archeraFinancialsDTO
	UpfrontCost *string `json:"upfront_one_time_cost"`
}

// archeraDeltaDTO is the vendor's candidate-minus-current difference.
type archeraDeltaDTO struct {
	MonthlyNetSavings *string `json:"monthly_730h_net_savings"`
	UpfrontCost       *string `json:"upfront_one_time_cost"`
	DiscountRate      *string `json:"discount_rate"`
	BreakevenDays     *string `json:"breakeven_days"`
}

// archeraHypotheticalDeltaDTO compares a hypothetical rollup against the current plan.
type archeraHypotheticalDeltaDTO struct {
	MonthlyNetSavings     *string `json:"monthly_730h_net_savings"`
	MonthlyCommitmentCost *string `json:"monthly_730h_commitment_cost"`
	UpfrontCost           *string `json:"upfront_one_time_cost"`
}

// archeraLineItemDTO is how one line item lands inside a hypothetical rollup.
type archeraLineItemDTO struct {
	ActualTerm           *string `json:"actual_term"`
	ActualPaymentOption  *string `json:"actual_payment_option"`
	ActualCommitmentType *string `json:"actual_commitment_type"`
	LineItemID           string  `json:"line_item_id"`
	Reason               string  `json:"reason"`
}

// archeraHypotheticalDTO is a rollup for one (term, archeraPayment option) combination.
type archeraHypotheticalDTO struct {
	ContractTerm   *string                     `json:"contract_term"`
	PaymentOption  string                      `json:"payment_option"`
	Totals         archeraTotalsDTO            `json:"totals"`
	DeltaVsCurrent archeraHypotheticalDeltaDTO `json:"delta_vs_current"`
	LineItems      []archeraLineItemDTO        `json:"line_items"`
}

// archeraOfferDTO is one offer (current or candidate) for a line item.
type archeraOfferDTO struct {
	Monthly              archeraFinancialsDTO     `json:"monthly"`
	DeltaVsCurrent       archeraDeltaDTO          `json:"delta_vs_current"`
	LeaseMenuItemID      *string                  `json:"lease_menu_item_id"`
	ArcheraOfferName     *string                  `json:"archera_offer_name,omitempty"`
	Region               *string                  `json:"region"`
	ContractTerm         *string                  `json:"contract_term"`
	PaymentOption        *string                  `json:"payment_option"`
	UpfrontCost          *string                  `json:"upfront_one_time_cost"`
	BreakevenDays        *string                  `json:"breakeven_days"`
	DiscountRate         *string                  `json:"discount_rate"`
	ProductSupport       archeraProductSupportDTO `json:"archera_product_support"`
	ArcheraOfferNameNote string                   `json:"archera_offer_name_note,omitempty"`
	CommitmentType       string                   `json:"commitment_type"`
	OfferID              string                   `json:"offer_id"`
	Provider             string                   `json:"provider"`
	LeaseAttached        bool                     `json:"lease_attached"`
	IsCurrent            bool                     `json:"is_current"`
}

// archeraRowDTO is a line item's current offer plus its alternatives.
type archeraRowDTO struct {
	LineItemID string            `json:"line_item_id"`
	Current    archeraOfferDTO   `json:"current"`
	Candidates []archeraOfferDTO `json:"candidates"`
}

// archeraComparisonDTO is the platform response. No org ID is included.
type archeraComparisonDTO struct {
	Current               archeraTotalsDTO         `json:"current"`
	Currency              *string                  `json:"currency"`
	BasisNote             string                   `json:"basis_note"`
	FetchedAt             string                   `json:"fetched_at"`
	CurrencyNote          string                   `json:"currency_note"`
	Title                 string                   `json:"title"`
	DeltaBasisNote        string                   `json:"delta_basis_note"`
	PlanID                string                   `json:"plan_id"`
	NonGatingDisclosure   string                   `json:"non_gating_disclosure"`
	SponsorshipDisclosure string                   `json:"sponsorship_disclosure"`
	Hypotheticals         []archeraHypotheticalDTO `json:"hypotheticals"`
	Rows                  []archeraRowDTO          `json:"rows"`
	PremiumIncluded       bool                     `json:"premium_included"`
}

type archeraMapper struct{ err error }

func (m *archeraMapper) dec(r *big.Rat) *string {
	s, err := archeraDecimal(r)
	if err != nil && m.err == nil {
		m.err = err
	}
	return s
}

func archeraCleanPtr(s *string) *string {
	if s == nil {
		return nil
	}
	c := archeraCleanString(*s)
	return &c
}

func (m *archeraMapper) financials(f insurance.Financials) archeraFinancialsDTO {
	return archeraFinancialsDTO{
		CommitmentCostTotal: m.dec(f.CommitmentCostTotal), CloudProviderCost: m.dec(f.CloudProviderCost),
		Premium: m.dec(f.Premium), GrossSavings: m.dec(f.GrossSavings), NetSavings: m.dec(f.NetSavings),
		CoveredOnDemandCost: m.dec(f.CoveredOnDemandCost),
	}
}

func (m *archeraMapper) totals(t insurance.Totals) archeraTotalsDTO {
	return archeraTotalsDTO{archeraFinancialsDTO: m.financials(t.Monthly), UpfrontCost: m.dec(t.UpfrontCost)}
}

func (m *archeraMapper) delta(d insurance.OfferDelta) archeraDeltaDTO {
	return archeraDeltaDTO{MonthlyNetSavings: m.dec(d.MonthlyNetSavings), UpfrontCost: m.dec(d.UpfrontCost),
		DiscountRate: m.dec(d.DiscountRate), BreakevenDays: m.dec(d.BreakevenDays)}
}

func archeraPayment(p *insurance.PaymentOption) *string {
	if p == nil {
		return nil
	}
	s := archeraCleanString(string(*p))
	return &s
}

func (m *archeraMapper) offer(e *insurance.OfferEntry) archeraOfferDTO {
	support := insurance.AssessProductSupport(e.Provider, e.CommitmentType)
	ps := archeraProductSupportDTO{Status: unknownVerdict, UnderwritingAllowed: unknownVerdict, CustomerEligibility: unknownVerdict}
	if support.Status == insurance.ProductSupportSupported {
		ps.Status, ps.Source, ps.Evidence = supportedSummary, support.Source, support.Evidence
	}
	o := archeraOfferDTO{
		OfferID: archeraCleanString(e.OfferID), IsCurrent: e.IsCurrent, Provider: archeraCleanString(string(e.Provider)),
		CommitmentType: archeraCleanString(e.CommitmentType), Region: archeraCleanPtr(e.Region), ContractTerm: archeraCleanPtr(e.ContractTerm),
		PaymentOption: archeraPayment(e.PaymentOption), LeaseAttached: e.LeaseBacked(), LeaseMenuItemID: archeraCleanPtr(e.LeaseMenuItemID),
		ProductSupport: ps, DiscountRate: m.dec(e.DiscountRate), BreakevenDays: m.dec(e.BreakevenDays),
		Monthly: m.financials(e.Monthly), UpfrontCost: m.dec(e.UpfrontCost), DeltaVsCurrent: m.delta(e.Delta),
	}
	if e.GuaranteedDisplayName != nil {
		o.ArcheraOfferName, o.ArcheraOfferNameNote = archeraCleanPtr(e.GuaranteedDisplayName), offerNameCaveat
	}
	return o
}

// buildArcheraComparison maps the contract type to the platform DTO. Money is exact
// decimal text or null; every vendor string is cleaned and capped. It returns
// ErrArcheraUnrepresentable when any number has no exact decimal form.
func buildArcheraComparison(c *insurance.Comparison) (*archeraComparisonDTO, error) {
	m := &archeraMapper{}
	out := &archeraComparisonDTO{
		Title: dtoTitle, PlanID: archeraCleanString(c.PlanID), FetchedAt: c.FetchedAt.UTC().Format(time.RFC3339),
		CurrencyNote: currencyNote, PremiumIncluded: true, BasisNote: basisNote, DeltaBasisNote: deltaBasisNote,
		Current:       m.totals(c.Current),
		Hypotheticals: []archeraHypotheticalDTO{}, Rows: []archeraRowDTO{},
		NonGatingDisclosure: common.ArcheraNonGatingDisclosure, SponsorshipDisclosure: common.ArcheraSponsorshipDisclosure,
	}
	for i := range c.Hypotheticals {
		h := &c.Hypotheticals[i]
		hd := archeraHypotheticalDTO{
			ContractTerm: archeraCleanPtr(h.ContractTerm), PaymentOption: archeraCleanString(string(h.PaymentOption)),
			Totals: m.totals(h.Totals), LineItems: []archeraLineItemDTO{},
			DeltaVsCurrent: archeraHypotheticalDeltaDTO{MonthlyNetSavings: m.dec(h.DeltaMonthlyNetSavings),
				MonthlyCommitmentCost: m.dec(h.DeltaMonthlyCommitmentCost), UpfrontCost: m.dec(h.DeltaUpfrontCost)},
		}
		for _, li := range h.LineItems {
			hd.LineItems = append(hd.LineItems, archeraLineItemDTO{
				LineItemID: archeraCleanString(li.LineItemID), Reason: archeraCleanString(string(li.Reason)), ActualTerm: archeraCleanPtr(li.ActualTerm),
				ActualPaymentOption: archeraPayment(li.ActualPaymentOption), ActualCommitmentType: archeraCleanPtr(li.ActualCommitmentType),
			})
		}
		out.Hypotheticals = append(out.Hypotheticals, hd)
	}
	for i := range c.Rows {
		r := &c.Rows[i]
		rd := archeraRowDTO{LineItemID: archeraCleanString(r.LineItemID), Current: m.offer(&r.Current), Candidates: []archeraOfferDTO{}}
		for j := range r.Candidates {
			rd.Candidates = append(rd.Candidates, m.offer(&r.Candidates[j]))
		}
		out.Rows = append(out.Rows, rd)
	}
	if m.err != nil {
		return nil, m.err
	}
	return out, nil
}
