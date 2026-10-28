package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	awsprovider "github.com/LeanerCloud/cloud-commitments-go/providers/aws"
)

// This file pins the wire encoding of the OpenSearch reserved-instance
// purchase (issue #89). aws-sdk-go-v2/service/opensearch v1.85.1 serializes
// requests through smithy-go's schema-based engine and the restJson1
// protocol, replacing the generated per-operation serializers. Every other
// test in this package mocks provider.ServiceClient, so nothing exercised the
// bytes the SDK actually puts on the wire. Here the real provider client
// (awsprovider.NewOpenSearchClient, the constructor the AWS provider's
// GetServiceClient dispatches to) is pointed at a local httptest server via
// aws.Config.BaseEndpoint, and the MCP tool's handle drives a confirmed
// purchase into it. All responses are fixtures; no request can leave the
// loopback server (see loopbackOnlyTransport).

const (
	osWireOfferingID   = "fixture-offering-r6g-1yr-all-upfront"
	osWireReservedID   = "fixture-reserved-instance-1"
	osWireInstanceType = "r6g.large.search"
	osWirePurchasePath = "/2021-01-01/opensearch/purchaseReservedInstanceOffering"
	osWireOfferingPath = "/2021-01-01/opensearch/reservedInstanceOfferings"
	osWireReservedPath = "/2021-01-01/opensearch/reservedInstances"
	osWireTagsPath     = "/2021-01-01/tags"
)

// osWireOfferingsBody is a recorded-shape DescribeReservedInstanceOfferings
// response with a single 1-year all-upfront offering that matches the
// purchase the test makes.
var osWireOfferingsBody = fmt.Sprintf(`{"ReservedInstanceOfferings":[{`+
	`"ReservedInstanceOfferingId":%q,"InstanceType":%q,"Duration":31536000,`+
	`"FixedPrice":1000.0,"UsagePrice":0.0,"CurrencyCode":"USD","PaymentOption":"ALL_UPFRONT",`+
	`"RecurringCharges":[{"RecurringChargeAmount":0.0,"RecurringChargeFrequency":"Hourly"}]}]}`,
	osWireOfferingID, osWireInstanceType)

// osWireSTSBody is an STS GetCallerIdentity (awsQuery/XML) response. STS
// shares the BaseEndpoint, so the provider's post-purchase tagging step
// resolves its account ID here too. The account is deliberately not a
// 12-digit number so it can never look like a real account ID.
const osWireSTSBody = `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">` +
	`<GetCallerIdentityResult><Arn>arn:aws:iam::fixture-account:user/fixture</Arn>` +
	`<UserId>FIXTUREUSERID</UserId><Account>fixture-account</Account></GetCallerIdentityResult>` +
	`<ResponseMetadata><RequestId>fixture-request</RequestId></ResponseMetadata></GetCallerIdentityResponse>`

// osWireRequest is one request the fake endpoint received.
type osWireRequest struct {
	header http.Header
	query  url.Values
	method string
	path   string
	body   []byte
}

// osWireServer is a fake OpenSearch + STS endpoint that records every
// request and answers with fixtures. It fails the test on any request it
// does not recognize.
type osWireServer struct {
	t        *testing.T
	srv      *httptest.Server
	requests []osWireRequest
	mu       sync.Mutex
}

func newOSWireServer(t *testing.T) *osWireServer {
	t.Helper()
	s := &osWireServer{t: t}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *osWireServer) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("read request body: %v", err)
	}
	s.mu.Lock()
	s.requests = append(s.requests, osWireRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query(), header: r.Header.Clone(), body: body})
	s.mu.Unlock()

	key := r.Method + " " + r.URL.Path
	switch {
	case key == "GET "+osWireOfferingPath:
		writeOSWireJSON(w, osWireOfferingsBody)
	case key == "GET "+osWireReservedPath:
		writeOSWireJSON(w, `{"ReservedInstances":[]}`)
	case key == "POST "+osWirePurchasePath:
		writeOSWireJSON(w, fmt.Sprintf(`{"ReservedInstanceId":%q,"ReservationName":"fixture"}`, osWireReservedID))
	case key == "POST "+osWireTagsPath:
		writeOSWireJSON(w, `{}`)
	case key == "POST /" && strings.Contains(string(body), "Action=GetCallerIdentity"):
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, osWireSTSBody)
	default:
		s.t.Errorf("unexpected request to fake OpenSearch endpoint: %s (body %q)", key, body)
		http.Error(w, "unexpected request", http.StatusNotImplemented)
	}
}

func writeOSWireJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

// purchaseRequests returns every PurchaseReservedInstanceOffering request.
func (s *osWireServer) purchaseRequests() []osWireRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []osWireRequest
	for _, r := range s.requests {
		if r.path == osWirePurchasePath {
			out = append(out, r)
		}
	}
	return out
}

// loopbackOnlyTransport refuses any request that is not addressed to the
// fake endpoint, so a regression in endpoint resolution fails the test
// instead of reaching real AWS.
type loopbackOnlyTransport struct {
	next http.RoundTripper
	host string
}

func (l loopbackOnlyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != l.host {
		return nil, fmt.Errorf("test transport refused non-loopback request to %s", r.URL.Host)
	}
	return l.next.RoundTrip(r)
}

// awsConfig returns an aws.Config that routes every SDK client at the fake
// endpoint with static fixture credentials (no profile, env or IMDS lookup).
func (s *osWireServer) awsConfig(region string) aws.Config {
	return aws.Config{
		Region:       region,
		BaseEndpoint: aws.String(s.srv.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "FIXTUREKEYID", SecretAccessKey: "fixture-signing-value", Source: "fixture"}, nil
		}),
		HTTPClient: &http.Client{Transport: loopbackOnlyTransport{
			next: s.srv.Client().Transport,
			host: strings.TrimPrefix(s.srv.URL, "http://"),
		}},
		RetryMaxAttempts: 1,
	}
}

// TestOpenSearchRIPurchaseWireEncoding drives a confirmed MCP OpenSearch RI
// purchase through the real provider client and SDK serializer and asserts
// the exact HTTP request PurchaseReservedInstanceOffering puts on the wire:
// route, content type, SigV4 signing scope, and the JSON body's field names,
// value types and values (InstanceCount must stay a JSON number).
func TestOpenSearchRIPurchaseWireEncoding(t *testing.T) {
	t.Parallel()
	srv := newOSWireServer(t)

	tool := NewAWSOpenSearchRIPurchaseTool().(*simpleAWSRIPurchaseTool)
	var gotService common.ServiceType
	var gotRegion string
	tool.createProvider = func(_ string, cfg *provider.Config) (provider.Provider, error) {
		return &recordingProvider{
			fakeProvider: &fakeProvider{name: "aws"},
			client:       awsprovider.NewOpenSearchClient(srv.awsConfig(cfg.Region)),
			gotService:   &gotService,
			gotRegion:    &gotRegion,
		}, nil
	}

	args := validSimpleArgs()
	args.ResourceType = osWireInstanceType
	args.Count = 3
	args.DryRun = boolPtr(false)
	args.Confirm = boolPtr(true)

	_, resp, err := tool.handle(context.Background(), nil, args)
	require.NoError(t, err)
	assert.True(t, resp.Success)
	assert.Equal(t, osWireReservedID, resp.CommitmentID)
	assert.Equal(t, common.ServiceOpenSearch, gotService)
	assert.Equal(t, "us-east-1", gotRegion)

	purchases := srv.purchaseRequests()
	require.Len(t, purchases, 1, "exactly one PurchaseReservedInstanceOffering call")
	req := purchases[0]

	assert.Equal(t, http.MethodPost, req.method)
	assert.Empty(t, req.query, "purchase input must be sent in the body, not the query string")
	assert.Equal(t, "application/json", req.header.Get("Content-Type"))
	assert.Contains(t, req.header.Get("Authorization"), "/us-east-1/es/aws4_request",
		"request must be SigV4-signed for the es service in the purchase region")

	assertOSWirePurchaseBody(t, req.body, expectedOSWireReservationName(t, tool, args), 3)
}

// expectedOSWireReservationName recomputes the ReservationName the provider
// derives from the MCP idempotency token, so the test pins the exact value.
func expectedOSWireReservationName(t *testing.T, tool *simpleAWSRIPurchaseTool, args simpleAWSRIPurchaseArgs) string {
	t.Helper()
	rec, region, dryRun, _, err := tool.recommendationFromArgs(args)
	require.NoError(t, err)
	scope := requestCredentialScope(args.AWSProfile, dryRun, "AWS_PROFILE")
	token := common.DeriveIdempotencyToken(idempotencyKeyFor(region, rec, scope, args.IdempotencyNonce, ""), 0)
	name := common.IdempotentReservationID("opensearch-id-", token)
	require.NotEmpty(t, name)
	return name
}

// assertOSWirePurchaseBody checks the serialized JSON body field by field on
// the raw JSON, so a value that changes type (e.g. a quoted "3") fails even
// though it would decode to the same Go value.
func assertOSWirePurchaseBody(t *testing.T, body []byte, wantName string, wantCount int) {
	t.Helper()
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &fields), "purchase body must be a JSON object: %s", body)

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"InstanceCount", "ReservationName", "ReservedInstanceOfferingId"}, keys,
		"purchase body must carry exactly these restJson1 member names: %s", body)

	assert.JSONEq(t, fmt.Sprintf("%d", wantCount), string(fields["InstanceCount"]), "InstanceCount must be a JSON number")
	assert.Equal(t, fmt.Sprintf("%d", wantCount), string(fields["InstanceCount"]), "InstanceCount must be an unquoted integer")
	assert.JSONEq(t, fmt.Sprintf("%q", osWireOfferingID), string(fields["ReservedInstanceOfferingId"]))
	assert.JSONEq(t, fmt.Sprintf("%q", wantName), string(fields["ReservationName"]))
}
