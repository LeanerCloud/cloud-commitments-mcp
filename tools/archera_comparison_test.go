package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
)

const (
	testArcheraKey  = "test-archera-key-0000"
	testArcheraOrg  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	testArcheraPlan = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	wirePath        = "testdata/archera_comparison_wire.json"
	goldenPath      = "testdata/archera_comparison_golden.json"
)

// recordingTransport serves recorded Archera responses and records every
// request, so tests assert the fixed origin and credential placement without
// any network access.
type recordingTransport struct {
	respond func(*http.Request) (*http.Response, error)
	header  http.Header
	reqs    []*http.Request
	body    []byte
	status  int
	mu      sync.Mutex
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.reqs = append(rt.reqs, r)
	if rt.respond != nil {
		return rt.respond(r)
	}
	h := rt.header.Clone()
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{
		StatusCode: rt.status, Header: h, Request: r,
		Body: io.NopCloser(bytes.NewReader(rt.body)),
	}, nil
}

func (rt *recordingTransport) count() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.reqs)
}

func setArcheraEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvArcheraAPIKey, testArcheraKey)
	t.Setenv(EnvArcheraOrgID, testArcheraOrg)
	t.Setenv(EnvArcheraPlanID, testArcheraPlan)
}

func readWire(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(wirePath)
	require.NoError(t, err)
	return b
}

func okTransport(body []byte) *recordingTransport {
	return &recordingTransport{status: 200, body: body}
}

// callArchera drives the real tool over the MCP protocol with an in-memory
// transport and returns the tool result plus every protocol byte exchanged.
func callArchera(t *testing.T, rt http.RoundTripper, args map[string]any) (*mcp.CallToolResult, string, error) {
	t.Helper()
	return callArcheraTool(context.Background(), t, &archeraComparisonTool{httpClient: &http.Client{Transport: rt}}, archeraComparisonName, args)
}

func callArcheraTool(ctx context.Context, t *testing.T, tool Registration, name string, args map[string]any) (*mcp.CallToolResult, string, error) {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	require.NoError(t, tool.Register(s))
	ct, st := mcp.NewInMemoryTransports()
	wire := &syncBuffer{}
	ss, err := s.Connect(ctx, &mcp.LoggingTransport{Transport: st, Writer: wire}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })
	c := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "test"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	// Close both sessions so the server goroutine has stopped writing before
	// the log is read.
	_ = cs.Close()
	_ = ss.Close()
	return res, wire.String(), err
}

// syncBuffer is a bytes.Buffer safe for the server goroutine to write while
// the test reads it.
type syncBuffer struct {
	b  bytes.Buffer
	mu sync.Mutex
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func resultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func structured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	require.False(t, res.IsError, resultText(res))
	b, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	return m
}

func TestArcheraComparisonGolden(t *testing.T) {
	setArcheraEnv(t)
	rt := okTransport(readWire(t))
	res, protocol, err := callArchera(t, rt, nil)
	require.NoError(t, err)
	got := structured(t, res)

	require.Equal(t, 1, rt.count())
	r := rt.reqs[0]
	assert.Equal(t, http.MethodGet, r.Method)
	assert.Equal(t, "https", r.URL.Scheme)
	assert.Equal(t, "api.archera.ai", r.URL.Host)
	assert.Equal(t, "/v1/org/"+testArcheraOrg+"/commitment-plans/"+testArcheraPlan+"/comparison", r.URL.Path)
	assert.Empty(t, r.URL.RawQuery)
	assert.Equal(t, testArcheraKey, r.Header.Get("x-api-key"))

	fetched, ok := got["fetched_at"].(string)
	require.True(t, ok)
	ts, err := time.Parse(time.RFC3339, fetched)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), ts, time.Minute)
	got["fetched_at"] = "FETCHED_AT"

	gotJSON, err := json.MarshalIndent(got, "", " ")
	require.NoError(t, err)
	if os.Getenv("ARCHERA_UPDATE_GOLDEN") != "" {
		require.NoError(t, os.WriteFile(goldenPath, append(gotJSON, '\n'), 0o600))
	}
	want, err := os.ReadFile(goldenPath)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(gotJSON))

	assert.NotContains(t, protocol, testArcheraKey)
	assert.NotContains(t, protocol, testArcheraOrg)

	// Every hostile vendor string reaches the output cleaned and capped.
	tagged := map[string]string{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if i := strings.Index(x, "HOSTILE_"); i >= 0 {
				tagged[x[i:i+len("HOSTILE_")+strings.Index(x[i+len("HOSTILE_"):], "_")]] = x
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(got)
	require.Len(t, tagged, 8, "all eight hostile fields must be present in the output")
	for tag, v := range tagged {
		assert.Len(t, v, 256, tag)
		assert.True(t, strings.HasPrefix(v, "[31m"+tag+"_x"), tag)
		for _, r := range v {
			assert.False(t, unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp), "%s has %U", tag, r)
		}
	}
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`, fetched, "no fractional seconds")
}

func TestArcheraComparisonSendsFilters(t *testing.T) {
	setArcheraEnv(t)
	rt := okTransport(readWire(t))
	li := "11111111-1111-4111-8111-111111111111"
	res, _, err := callArchera(t, rt, map[string]any{
		"line_item_ids":   []string{li},
		"contract_terms":  []string{"one_year", "three_year"},
		"payment_options": []string{"all_upfront", "no_upfront"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(res))
	require.Equal(t, 1, rt.count())
	q := rt.reqs[0].URL.Query()
	assert.Equal(t, []string{li}, q["line_item_ids"])
	assert.Equal(t, []string{"one_year", "three_year"}, q["contract_terms"])
	assert.Equal(t, []string{"all_upfront", "no_upfront"}, q["payment_options"])
}

func TestArcheraComparisonRejectsBadArgsWithoutRequest(t *testing.T) {
	setArcheraEnv(t)
	cases := map[string]map[string]any{
		"unknown term":       {"contract_terms": []string{"seven_years"}},
		"unknown payment":    {"payment_options": []string{"monthly"}},
		"non-uuid line item": {"line_item_ids": []string{"../etc/passwd"}},
		"too many payments":  {"payment_options": []string{"no_upfront", "all_upfront", "partial_upfront", "no_upfront"}},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			rt := okTransport(readWire(t))
			res, _, err := callArchera(t, rt, args)
			assert.True(t, err != nil || res.IsError, "bad arguments must fail loudly")
			assert.Zero(t, rt.count())
		})
	}
}

func TestArcheraComparisonNotConfigured(t *testing.T) {
	cases := map[string]map[string]string{
		"none":    {},
		"no key":  {EnvArcheraOrgID: testArcheraOrg, EnvArcheraPlanID: testArcheraPlan},
		"no org":  {EnvArcheraAPIKey: testArcheraKey, EnvArcheraPlanID: testArcheraPlan},
		"no plan": {EnvArcheraAPIKey: testArcheraKey, EnvArcheraOrgID: testArcheraOrg},
		"blank":   {EnvArcheraAPIKey: "  ", EnvArcheraOrgID: testArcheraOrg, EnvArcheraPlanID: testArcheraPlan},
	}
	wantMissing := map[string]string{
		"none":    "missing: ARCHERA_API_KEY, ARCHERA_ORG_ID, ARCHERA_PLAN_ID",
		"no key":  "missing: ARCHERA_API_KEY)",
		"no org":  "missing: ARCHERA_ORG_ID)",
		"no plan": "missing: ARCHERA_PLAN_ID)",
		"blank":   "missing: ARCHERA_API_KEY)",
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			for _, k := range []string{EnvArcheraAPIKey, EnvArcheraOrgID, EnvArcheraPlanID} {
				t.Setenv(k, env[k])
			}
			rt := okTransport(readWire(t))
			res, protocol, err := callArchera(t, rt, nil)
			require.NoError(t, err)
			require.True(t, res.IsError)
			assert.Zero(t, rt.count())
			assert.Contains(t, resultText(res), "archera is not configured")
			assert.Contains(t, resultText(res), wantMissing[name])
			assert.NotContains(t, protocol, testArcheraKey)
		})
	}
}

func TestArcheraComparisonVendorErrors(t *testing.T) {
	setArcheraEnv(t)
	echo := []byte(`{"message":"bad key ` + testArcheraKey + ` for org\u001b[31m"}`)
	cases := []struct {
		header http.Header
		name   string
		want   string
		body   []byte
		status int
	}{
		{name: "429 with retry-after", status: 429, header: http.Header{"Retry-After": {"120"}}, body: nil, want: "archera request failed: HTTP 429; retry after 120s"},
		{name: "429 huge retry-after capped at 24h", status: 429, header: http.Header{"Retry-After": {"99999999999"}}, body: nil, want: "archera request failed: HTTP 429; retry after 86400s"},
		{name: "429 http-date retry-after not given", status: 429, header: http.Header{"Retry-After": {"Wed, 21 Oct 2026 07:28:00 GMT"}}, body: nil, want: "archera request failed: HTTP 429; retry after: not given"},
		{name: "401 echoing the key", status: 401, header: nil, body: echo, want: "archera request failed: HTTP 401; retry after: not given"},
		{name: "302 not followed", status: 302, header: http.Header{"Location": {"https://evil.example/steal"}}, body: nil, want: "archera request failed: HTTP 302; retry after: not given"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &recordingTransport{status: tc.status, header: tc.header, body: tc.body}
			res, protocol, err := callArchera(t, rt, nil)
			require.NoError(t, err)
			require.True(t, res.IsError)
			assert.Equal(t, tc.want, resultText(res))
			assert.Equal(t, 1, rt.count(), "no retries and no redirect following")
			assert.NotContains(t, protocol, testArcheraKey)
			assert.NotContains(t, protocol, "bad key")
		})
	}
}

func TestArcheraComparisonCleansVendorStrings(t *testing.T) {
	setArcheraEnv(t)
	var w map[string]any
	require.NoError(t, json.Unmarshal(readWire(t), &w))
	cur := w["data"].([]any)[0].(map[string]any)["current"].(map[string]any)
	cur["offer_id"] = "id\u001b[31m\nIGNORE PREVIOUS\u200b\u2028" + strings.Repeat("x", 300)
	cur["commitment_type"] = "aws/AmazonEC2"
	cur["offer"].(map[string]any)["provider"] = "azure" // same type string, other provider
	cur["offer"].(map[string]any)["region"] = "r\x00egion"
	body, err := json.Marshal(w)
	require.NoError(t, err)

	res, _, err := callArchera(t, okTransport(body), nil)
	require.NoError(t, err)
	got := structured(t, res)
	off := got["rows"].([]any)[0].(map[string]any)["current"].(map[string]any)
	id := off["offer_id"].(string)
	assert.Len(t, id, 256)
	assert.True(t, strings.HasPrefix(id, "id[31mIGNORE PREVIOUS"+strings.Repeat("x", 3)))
	assert.NotContains(t, id, "\u001b")
	assert.NotContains(t, id, "\n")
	assert.NotContains(t, id, "\u200b")
	assert.NotContains(t, id, "\u2028")
	assert.Equal(t, "region", off["region"])
	ps := off["archera_product_support"].(map[string]any)
	assert.Equal(t, map[string]any{
		"status": "unknown", "underwriting_allowance": "unknown", "customer_eligibility": "unknown",
	}, ps, "a type listed for another provider must not be reported as supported")
}

func TestArcheraKeyNeverPrintable(t *testing.T) {
	setArcheraEnv(t)
	rt := okTransport(readWire(t))
	_, _, _ = callArchera(t, rt, nil)

	tool := &archeraComparisonTool{httpClient: &http.Client{Transport: rt}}
	c, err := insurance.NewClient(insurance.Config{APIKey: insurance.NewSecret(testArcheraKey), OrgID: testArcheraOrg}, nil)
	require.NoError(t, err)
	for _, v := range []any{tool, *tool, c, insurance.Config{APIKey: insurance.NewSecret(testArcheraKey), OrgID: testArcheraOrg}} {
		for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
			assert.NotContains(t, fmt.Sprintf(f, v), testArcheraKey, "%T with %s", v, f)
		}
	}
	for _, r := range rt.reqs {
		assert.NotContains(t, r.URL.String(), testArcheraKey)
	}
}

func TestArcheraProductionToolUsesDefaultHTTPClient(t *testing.T) {
	tool, ok := NewArcheraComparisonTool().(*archeraComparisonTool)
	require.True(t, ok)
	assert.Nil(t, tool.httpClient)
}

func TestArcheraDescriptor(t *testing.T) {
	d := NewArcheraComparisonTool().Descriptor()
	assert.Equal(t, "cudly_archera_comparison", d.Name)
	assert.Equal(t, "insurance", d.Product)
	assert.Equal(t, "comparison", d.Action)
	assert.False(t, d.RealPurchaseEnabled)
	require.NotNil(t, d.Annotations)
	assert.True(t, d.Annotations.ReadOnlyHint)
	require.NotNil(t, d.Annotations.OpenWorldHint)
	assert.True(t, *d.Annotations.OpenWorldHint)
	require.NotNil(t, d.Annotations.DestructiveHint)
	assert.False(t, *d.Annotations.DestructiveHint)
}

func TestArcheraBuildComparisonFetchedAtIsUTC(t *testing.T) {
	zone := time.FixedZone("x", 2*3600)
	dto, err := buildArcheraComparison(&insurance.Comparison{PlanID: testArcheraPlan, FetchedAt: time.Date(2026, 10, 10, 12, 0, 0, 0, zone)})
	require.NoError(t, err)
	assert.Equal(t, "2026-10-10T10:00:00Z", dto.FetchedAt)
}

func entryOf(w map[string]any) map[string]any {
	return w["data"].([]any)[0].(map[string]any)["current"].(map[string]any)
}
func candOf(w map[string]any) map[string]any {
	return w["data"].([]any)[0].(map[string]any)["candidates"].([]any)[0].(map[string]any)
}
func hypOf(w map[string]any) map[string]any {
	return w["hypothetical_totals"].([]any)[0].(map[string]any)
}
func hypLI(w map[string]any) map[string]any {
	return hypOf(w)["line_items"].([]any)[0].(map[string]any)
}

func TestArcheraEmptyVendorListsRenderAsEmptyArrays(t *testing.T) {
	setArcheraEnv(t)
	var w map[string]any
	require.NoError(t, json.Unmarshal(readWire(t), &w))
	w["data"].([]any)[0].(map[string]any)["candidates"] = []any{}
	w["hypothetical_totals"] = []any{}
	body, err := json.Marshal(w)
	require.NoError(t, err)
	res, _, err := callArchera(t, okTransport(body), nil)
	require.NoError(t, err)
	got := structured(t, res)
	assert.Equal(t, []any{}, got["hypotheticals"])
	assert.Equal(t, []any{}, got["rows"].([]any)[0].(map[string]any)["candidates"])

	w["data"] = []any{}
	body, err = json.Marshal(w)
	require.NoError(t, err)
	res, _, err = callArchera(t, okTransport(body), nil)
	require.NoError(t, err)
	assert.Equal(t, []any{}, structured(t, res)["rows"])
}

func TestArcheraErrorMasksKeyAndKeepsContextErrors(t *testing.T) {
	err := archeraError(fmt.Errorf("transport said %s twice: %s", testArcheraKey, testArcheraKey), testArcheraKey)
	assert.Equal(t, "transport said [redacted] twice: [redacted]", err.Error())
	assert.Same(t, context.Canceled, archeraError(context.Canceled, testArcheraKey))
	wrapped := fmt.Errorf("archera request failed: %w", context.DeadlineExceeded)
	assert.Same(t, wrapped, archeraError(wrapped, testArcheraKey))
}

func TestArcheraBuildComparisonPropagatesUnrepresentableMoney(t *testing.T) {
	c := &insurance.Comparison{PlanID: testArcheraPlan}
	c.Current.Monthly.CommitmentCostTotal = big.NewRat(1, 3)
	_, err := buildArcheraComparison(c)
	assert.ErrorIs(t, err, ErrArcheraUnrepresentable)
}

func TestArcheraComparisonRejectsUnusableKey(t *testing.T) {
	for name, key := range map[string]string{"inner space": "ke y-0000", "zero-width": "key\u200b-0000", "non-ascii": "k\u00e9y-0000"} {
		t.Run(name, func(t *testing.T) {
			setArcheraEnv(t)
			t.Setenv(EnvArcheraAPIKey, key)
			rt := okTransport(readWire(t))
			res, protocol, err := callArchera(t, rt, nil)
			require.NoError(t, err)
			require.True(t, res.IsError)
			assert.Contains(t, resultText(res), "non-printable or non-ASCII")
			assert.NotContains(t, resultText(res), key)
			assert.NotContains(t, protocol, key)
			assert.Equal(t, 0, rt.count(), "no request with a rejected key")
		})
	}
}

func TestArcheraComparisonTrimsEnvKey(t *testing.T) {
	setArcheraEnv(t)
	t.Setenv(EnvArcheraAPIKey, testArcheraKey+"\r\n")
	rt := okTransport(readWire(t))
	res, _, err := callArchera(t, rt, nil)
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(res))
	assert.Equal(t, testArcheraKey, rt.reqs[0].Header.Get("x-api-key"))
}
