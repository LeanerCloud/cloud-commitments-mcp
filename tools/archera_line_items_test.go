package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
)

func listingWire(t *testing.T, number int) string {
	t.Helper()
	body, err := os.ReadFile("testdata/archera_line_item.json")
	require.NoError(t, err)
	id := fmt.Sprintf("%08x-aaaa-4aaa-8aaa-aaaaaaaaaaaa", number)
	if number == 2 {
		id = strings.ToUpper(id)
	}
	return strings.ReplaceAll(string(body), "LINE_ID", id)
}

func listingTransport(t *testing.T, total int, edit func(*http.Response, *http.Request)) *recordingTransport {
	t.Helper()
	return &recordingTransport{respond: func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/comparison") {
			return okTransport(readWire(t)).RoundTrip(r)
		}
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "https", r.URL.Scheme)
		assert.Equal(t, "api.archera.ai", r.URL.Host)
		assert.Equal(t, "/v1/org/"+testArcheraOrg+"/commitment-plans/"+testArcheraPlan+"/line-items", r.URL.Path)
		assert.Equal(t, strings.TrimSpace(os.Getenv(EnvArcheraAPIKey)), r.Header.Get("x-api-key"))
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		require.NoError(t, err)
		assert.Equal(t, "100", r.URL.Query().Get("page_size"))
		assert.Equal(t, "id", r.URL.Query().Get("order_by"))
		assert.Len(t, r.URL.Query(), 3)
		pages := (total + 99) / 100
		header := http.Header{"X-Pagination": {fmt.Sprintf(`{"total":%d,"total_pages":%d,"page":%d,"first_page":1,"last_page":%d}`, total, pages, page, pages)}}
		rows := []string{}
		for i := (page - 1) * 100; i < min(page*100, total); i++ {
			rows = append(rows, listingWire(t, total-i))
		}
		response := &http.Response{StatusCode: http.StatusOK, Header: header, Request: r, Body: io.NopCloser(strings.NewReader("[" + strings.Join(rows, ",") + "]"))}
		if edit != nil {
			edit(response, r)
		}
		return response, nil
	}}
}

func callListing(t *testing.T, rt http.RoundTripper, args map[string]any) (*mcp.CallToolResult, string, error) {
	t.Helper()
	return callArcheraTool(context.Background(), t, &archeraLineItemsTool{httpClient: &http.Client{Transport: rt}}, archeraLineItemsName, args)
}

func TestArcheraListingWindowOrdersCaseInsensitivelyWithRawTieBreak(t *testing.T) {
	t.Parallel()
	rows := []insurance.LineItem{{ID: "b"}, {ID: "a"}, {ID: "B"}, {ID: "A"}, {ID: "c"}}
	_, err := archeraListingWindow(rows, 0, 0, "")
	require.NoError(t, err)
	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.ID)
	}
	assert.Equal(t, []string{"A", "a", "B", "b", "c"}, got)
}

func TestArcheraLineItemsWindowsAndIDReuse(t *testing.T) {
	setArcheraEnv(t)
	for _, tc := range []struct{ offset, limit, total, want, next int }{
		{0, 100, 101, 100, 100}, {1, 1, 101, 1, 2}, {0, 200, 101, 101, -1},
		{101, 100, 101, 0, -1}, {102, 100, 101, -1, -1}, {0, 100, 0, 0, -1},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			rt := listingTransport(t, tc.total, nil)
			args := map[string]any{"offset": tc.offset, "limit": tc.limit}
			if tc.offset == 0 && tc.limit == 100 {
				args = nil
			}
			result, protocol, err := callListing(t, rt, args)
			require.NoError(t, err)
			if tc.want < 0 {
				require.True(t, result.IsError)
				assert.Empty(t, result.StructuredContent)
				assert.Contains(t, resultText(result), "offset")
				return
			}
			got := structured(t, result)
			assert.EqualValues(t, tc.total, got["total"])
			assert.EqualValues(t, tc.offset, got["offset"])
			assert.EqualValues(t, tc.limit, got["limit"])
			rows := got["line_items"].([]any)
			require.Len(t, rows, tc.want)
			assert.NotNil(t, rows)
			if tc.next < 0 {
				assert.Nil(t, got["next_offset"])
			} else {
				assert.EqualValues(t, tc.next, got["next_offset"])
			}
			assert.Contains(t, got["note"], "no atomic snapshot")
			assert.NotEmpty(t, got["non_gating_disclosure"])
			assert.NotEmpty(t, got["sponsorship_disclosure"])
			for _, forbidden := range []string{testArcheraKey, testArcheraOrg, testArcheraPlan, "private-account", "private-catalog", "private-provider-offering"} {
				assert.NotContains(t, protocol, forbidden)
			}
			require.Equal(t, max(1, (tc.total+99)/100), rt.count())
			if tc.offset == 1 {
				row := rows[0].(map[string]any)
				assert.Len(t, row, 13)
				assert.Equal(t, "00000002-AAAA-4AAA-8AAA-AAAAAAAAAAAA", row["line_item_id"])
				assert.Equal(t, "cccccccc-cccc-4ccc-8ccc-cccccccccccc", row["offer_id"])
				assert.Equal(t, true, row["is_selected"])
				assert.Equal(t, "9007199254740993", row["selected_quantity"])
				assert.Equal(t, "0."+strings.Repeat("0", 100)+"1", row["selected_commitment"])
				assert.Equal(t, "contract-type", row["commitment_type"])
				assert.Equal(t, "one_year", row["contract_term"])
				assert.Equal(t, "no_upfront", row["payment_option"])
				assert.Equal(t, "aws", row["provider"])
				for _, nullable := range []string{"region", "instance_family", "native_plan_type"} {
					assert.Nil(t, row[nullable])
				}
				assert.Equal(t, "31536000.25", row["native_duration_seconds"])
				comparison, _, err := callArchera(t, rt, map[string]any{"line_item_ids": []string{row["line_item_id"].(string)}})
				require.NoError(t, err)
				require.False(t, comparison.IsError, resultText(comparison))
				require.Equal(t, 3, rt.count())
				assert.Equal(t, []string{row["line_item_id"].(string)}, rt.reqs[2].URL.Query()["line_item_ids"])
			}
		})
	}
}

func TestArcheraLineItemsSchemaAndRuntimeBounds(t *testing.T) {
	setArcheraEnv(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	require.NoError(t, NewArcheraLineItemsTool().Register(server))
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	require.NoError(t, err)
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	defer cs.Close()
	list, err := cs.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, list.Tools, 1)
	schema := list.Tools[0].InputSchema.(map[string]any)["properties"].(map[string]any)
	require.Len(t, schema, 2, "no credential, organization, plan or URL arguments")
	limit := schema["limit"].(map[string]any)
	assert.Equal(t, "integer", limit["type"])
	assert.NotContains(t, limit, "default", "schema defaults panic the SDK when the call sends explicit `\"arguments\": null` (omitted arguments are safe)")
	assert.EqualValues(t, 1, limit["minimum"])
	assert.EqualValues(t, 200, limit["maximum"])
	assert.EqualValues(t, 0, schema["offset"].(map[string]any)["minimum"])
	for _, args := range []map[string]any{
		{"limit": 0}, {"limit": -1}, {"limit": 201}, {"limit": 1.5}, {"limit": nil},
		{"offset": -1}, {"offset": 1.5}, {"offset": json.Number("9223372036854775808")}, {"limit": "bad"},
	} {
		rt := listingTransport(t, 1, nil)
		result, _, err := callListing(t, rt, args)
		assert.True(t, err != nil || result.IsError)
		assert.Zero(t, rt.count())
	}
	for _, args := range []archeraLineItemsArgs{{Offset: -1}, {Limit: new(int64)}, {Limit: ptrInt64(201)}} {
		rt := listingTransport(t, 1, nil)
		_, _, err := (&archeraLineItemsTool{httpClient: &http.Client{Transport: rt}}).handle(ctx, nil, args)
		require.Error(t, err)
		assert.Zero(t, rt.count(), "handler bounds also apply when schema validation is bypassed")
	}
}

func quoteJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	require.NoError(t, err)
	return string(b)
}

func ptrInt64(value int64) *int64 { return &value }

func TestArcheraLineItemsBadConfigurationSendsNothing(t *testing.T) {
	for _, setting := range []string{EnvArcheraAPIKey, EnvArcheraOrgID, EnvArcheraPlanID} {
		for _, bad := range []string{"", "bad\u200bvalue"} {
			t.Run(setting+bad, func(t *testing.T) {
				setArcheraEnv(t)
				t.Setenv(setting, bad)
				rt := listingTransport(t, 1, nil)
				result, protocol, err := callListing(t, rt, nil)
				require.NoError(t, err)
				require.True(t, result.IsError)
				assert.Zero(t, rt.count())
				assert.NotContains(t, protocol, testArcheraKey)
			})
		}
	}
}

func TestArcheraLineItemsLateFailuresAndSharedDeadline(t *testing.T) {
	setArcheraEnv(t)
	for _, failure := range []string{"http", "decode", "metadata", "duplicate", "size", "cancel", "429", "redirect", "none"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var first time.Time
			rt := listingTransport(t, 101, func(response *http.Response, request *http.Request) {
				deadline, ok := request.Context().Deadline()
				require.True(t, ok)
				assert.LessOrEqual(t, time.Until(deadline), 30*time.Second)
				if first.IsZero() {
					first = deadline
				} else {
					assert.Equal(t, first, deadline)
				}
				if request.URL.Query().Get("page") != "2" {
					return
				}
				switch failure {
				case "http":
					response.StatusCode = http.StatusInternalServerError
					response.Body = io.NopCloser(strings.NewReader(testArcheraKey))
				case "decode":
					response.Body = io.NopCloser(strings.NewReader("[{}]"))
				case "metadata":
					response.Header.Set("X-Pagination", "{}")
				case "duplicate":
					response.Body = io.NopCloser(strings.NewReader("[" + listingWire(t, 101) + "]"))
				case "size":
					response.Body = io.NopCloser(strings.NewReader("[" + listingWire(t, 1) + "]" + strings.Repeat(" ", 8<<20)))
				case "cancel":
					cancel()
				case "429":
					response.StatusCode = http.StatusTooManyRequests
					response.Header.Set("Retry-After", "2")
				case "redirect":
					response.StatusCode = http.StatusFound
					response.Header.Set("Location", "https://api.archera.ai/redirect")
				}
			})
			result, protocol, err := callArcheraTool(ctx, t, &archeraLineItemsTool{httpClient: &http.Client{Transport: rt}}, archeraLineItemsName, map[string]any{"limit": 1})
			require.Equal(t, 2, rt.count(), "fetch every vendor page before returning even one row; no retries")
			assert.NotContains(t, protocol, testArcheraKey)
			if failure == "none" {
				require.NoError(t, err)
				require.False(t, result.IsError, resultText(result))
				return
			}
			assert.True(t, err != nil || result.IsError)
			if result != nil {
				assert.Empty(t, result.StructuredContent)
				if failure == "429" {
					assert.Contains(t, resultText(result), "retry after 2s")
				}
				if failure == "redirect" {
					assert.Contains(t, resultText(result), "302")
				}
			}
		})
	}
}

func TestArcheraLineItemsPrivacyAndOutputCap(t *testing.T) {
	var logs bytes.Buffer
	previousLog, previousSlog := log.Writer(), slog.Default()
	log.SetOutput(&logs)
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { log.SetOutput(previousLog); slog.SetDefault(previousSlog) })
	setArcheraEnv(t)
	audit := filepath.Join(t.TempDir(), "listing-audit.jsonl")
	t.Setenv(EnvAuditLog, audit)
	rt := listingTransport(t, 1, func(response *http.Response, _ *http.Request) {
		body := strings.Replace(listingWire(t, 1), `"contract-type"`, quoteJSON(t, testArcheraKey+"\x1b[31m"+strings.Repeat("x", 500)), 1)
		body = strings.Replace(body, "9007199254740993", "0", 1)
		body = strings.Replace(body, "0.1e-100", "0", 1)
		response.Body = io.NopCloser(strings.NewReader("[" + body + "]"))
	})
	result, protocol, err := callListing(t, rt, nil)
	require.NoError(t, err)
	row := structured(t, result)["line_items"].([]any)[0].(map[string]any)
	assert.Equal(t, "0", row["selected_quantity"])
	assert.Equal(t, "0", row["selected_commitment"])
	assert.Contains(t, row["commitment_type"], "[redacted]")
	assert.NotContains(t, protocol+logs.String(), testArcheraKey)
	assert.NotContains(t, protocol, "\\u001b")
	assert.NotContains(t, protocol, strings.Repeat("x", 257))
	_, statErr := os.Stat(audit)
	assert.True(t, os.IsNotExist(statErr), "listing writes no purchase audit record")
	t.Setenv(EnvArcheraAPIKey, "00000001-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	collision := listingTransport(t, 1, nil)
	result, protocol, err = callListing(t, collision, nil)
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Empty(t, result.StructuredContent)
	assert.NotContains(t, protocol+logs.String(), "00000001-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	t.Setenv(EnvArcheraAPIKey, testArcheraKey)
	large := listingTransport(t, 200, func(response *http.Response, _ *http.Request) {
		body, readErr := io.ReadAll(response.Body)
		require.NoError(t, readErr)
		value := strconv.Quote(strings.Repeat("<", 256))
		text := strings.ReplaceAll(string(body), `"contract-type"`, value)
		for _, field := range []string{"region", "instance_family", "plan_type"} {
			text = strings.ReplaceAll(text, `"`+field+`":null`, `"`+field+`":`+value)
		}
		response.Body = io.NopCloser(strings.NewReader(text))
	})
	result, _, err = callListing(t, large, map[string]any{"limit": 200})
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Empty(t, result.StructuredContent)
	assert.Contains(t, resultText(result), "512 KiB")
}

func TestArcheraLineItemsRejectsUnsafeIDOutsideRequestedWindow(t *testing.T) {
	setArcheraEnv(t)
	t.Setenv(EnvArcheraAPIKey, "00000003-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	for _, args := range []map[string]any{{"offset": 0, "limit": 1}, {"offset": 0, "limit": 3}, {"offset": 2, "limit": 1}} {
		result, _, err := callListing(t, listingTransport(t, 3, nil), args)
		require.NoError(t, err)
		require.True(t, result.IsError, "%v", args)
		assert.Empty(t, result.StructuredContent)
		assert.Contains(t, resultText(result), "line-item identities cannot be safely displayed", "%v", args)
	}
}
