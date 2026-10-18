package tools

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArcheraSuccessKeyDiagnostic(t *testing.T) {
	captureLogs := func(t *testing.T) []*syncBuffer {
		standard, structuredLog, shared := &syncBuffer{}, &syncBuffer{}, &syncBuffer{}
		previousStandard, previousSlog := log.Writer(), slog.Default()
		previousFlags := log.Flags()
		previousShared, previousLevel := logging.SetOutput(shared), logging.GetLevel()
		t.Cleanup(func() {
			slog.SetDefault(previousSlog)
			log.SetOutput(previousStandard)
			log.SetFlags(previousFlags)
			logging.SetOutput(previousShared)
			logging.SetLevelValue(previousLevel)
		})
		slog.SetDefault(slog.New(slog.NewJSONHandler(structuredLog, nil)))
		log.SetOutput(standard)
		logging.SetLevelValue(logging.LevelDebug)
		return []*syncBuffer{standard, structuredLog, shared}
	}
	checkSinks := func(t *testing.T, res *mcp.CallToolResult, protocol, key string, logs []*syncBuffer) {
		text := resultText(res)
		if !res.IsError {
			var parsed map[string]any
			require.NoError(t, json.Unmarshal([]byte(text), &parsed))
			parsedJSON, err := json.Marshal(parsed)
			require.NoError(t, err)
			assert.NotContains(t, string(parsedJSON), key, "sink=parsed text JSON")
		}
		structuredJSON, err := json.Marshal(res.StructuredContent)
		require.NoError(t, err)
		fullJSON, err := json.Marshal(res)
		require.NoError(t, err)
		for _, sink := range []struct{ name, value string }{
			{"text", text},
			{"StructuredContent", string(structuredJSON)}, {"full result", string(fullJSON)}, {"protocol", protocol},
			{"standard log", logs[0].String()}, {"slog", logs[1].String()}, {"shared log", logs[2].String()},
		} {
			assert.NotContains(t, sink.value, key, "sink=%s", sink.name)
		}
	}
	for _, field := range []string{"control", "guaranteed_display_name", "region", "commitment_type"} {
		for _, role := range []string{"current", "candidate"} {
			t.Run(role+"/"+field, func(t *testing.T) {
				setArcheraEnv(t)
				logs := captureLogs(t)
				var wire map[string]any
				require.NoError(t, json.Unmarshal(readWire(t), &wire))
				entry := entryOf(wire)
				if role == "candidate" {
					entry = candOf(wire)
				}
				offer := entry["offer"].(map[string]any)
				want := "synthetic-prefix-" + testArcheraKey + "-suffix"
				switch field {
				case "control":
					offer["guaranteed_display_name"], offer["region"], entry["commitment_type"] = "Ordinary offer", "eu-west-1", "aws/AmazonEC2"
				case "commitment_type":
					entry[field] = want
				default:
					offer[field] = want
				}
				body, err := json.Marshal(wire)
				require.NoError(t, err)
				decoded, err := insurance.DecodeComparison(bytes.NewReader(body), testArcheraOrg, testArcheraPlan, time.Now())
				require.NoError(t, err, "setup: exact response must pass the real decoder")
				require.NotEmpty(t, decoded.Rows)
				decodedOffer := decoded.Rows[0].Current
				if role == "candidate" {
					require.NotEmpty(t, decoded.Rows[0].Candidates)
					decodedOffer = decoded.Rows[0].Candidates[0]
				}
				require.NotNil(t, decodedOffer.SelectedAmount)
				require.Equal(t, "2", decodedOffer.SelectedAmount.RatString())
				switch field {
				case "guaranteed_display_name":
					require.NotNil(t, decodedOffer.GuaranteedDisplayName)
					require.Equal(t, want, *decodedOffer.GuaranteedDisplayName)
				case "region":
					require.NotNil(t, decodedOffer.Region)
					require.Equal(t, want, *decodedOffer.Region)
				case "commitment_type":
					require.Equal(t, want, decodedOffer.CommitmentType)
				}
				rt := okTransport(body)
				res, protocol, err := callArchera(t, rt, nil)
				require.NoError(t, err, "setup: CallTool must succeed")
				require.NotNil(t, res)
				if field == "control" {
					require.False(t, res.IsError)
				} else {
					require.True(t, res.IsError, "credential-bearing decoded success must be rejected")
					assert.Empty(t, res.StructuredContent)
					assert.NotContains(t, resultText(res), "monthly_730h")
				}
				require.Equal(t, 1, rt.count())
				request := rt.reqs[0]
				assert.Equal(t, http.MethodGet, request.Method)
				assert.Equal(t, "https", request.URL.Scheme)
				assert.Equal(t, "api.archera.ai", request.URL.Host)
				assert.Equal(t, "/v1/org/"+testArcheraOrg+"/commitment-plans/"+testArcheraPlan+"/comparison", request.URL.Path)
				assert.Empty(t, request.URL.RawQuery)
				assert.True(t, request.Header.Get("x-api-key") == testArcheraKey, "credential is confined to intercepted header")
				if field == "control" {
					var parsed map[string]any
					require.NoError(t, json.Unmarshal([]byte(resultText(res)), &parsed))
					for _, output := range []map[string]any{structured(t, res), parsed} {
						assert.Equal(t, common.ArcheraNonGatingDisclosure, output["non_gating_disclosure"])
						assert.Equal(t, common.ArcheraSponsorshipDisclosure, output["sponsorship_disclosure"])
						row := output["rows"].([]any)[0].(map[string]any)
						returned := row["current"].(map[string]any)
						if role == "candidate" {
							returned = row["candidates"].([]any)[0].(map[string]any)
						}
						assert.Equal(t, "Ordinary offer", returned["archera_offer_name"])
						assert.Equal(t, "eu-west-1", returned["region"])
						assert.Equal(t, "aws/AmazonEC2", returned["commitment_type"])
						upfront := "21.21"
						if role == "candidate" {
							upfront = "34.34"
						}
						assert.Equal(t, upfront, returned["upfront_one_time_cost"])
						assert.Nil(t, output["currency"])
						assert.Equal(t, entry["contract_term"], returned["contract_term"])
						assert.Equal(t, archeraCleanString(entry["offer_id"].(string)), returned["offer_id"])
					}
				}
				t.Log("real decoder acceptance and CallTool rejection/control established before sink checks")
				checkSinks(t, res, protocol, testArcheraKey, logs)
			})
		}
	}

	for _, tc := range []struct {
		mutate    func(map[string]any)
		name, key string
	}{
		{func(w map[string]any) {
			entryOf(w)["offer"].(map[string]any)["guaranteed_display_name"] = strings.Join(strings.Split(testArcheraKey, ""), "\x00\u200b")
		}, "split name", testArcheraKey},
		{func(w map[string]any) {
			candOf(w)["commitment_type"] = strings.Join(strings.Split(testArcheraKey, ""), "\u0301 ")
		}, "combining spaced type", testArcheraKey},
		{func(w map[string]any) {
			entryOf(w)["offer"].(map[string]any)["guaranteed_display_name"] = strings.Repeat("x", 250) + testArcheraKey
		}, "cap name", testArcheraKey},
		{func(w map[string]any) { candOf(w)["commitment_type"] = strings.Repeat("x", 250) + testArcheraKey }, "cap type", testArcheraKey},
		{func(w map[string]any) { hypLI(w)["actual_commitment_type"] = testArcheraKey }, "hypothetical type", testArcheraKey},
		{func(w map[string]any) { w["data"].([]any)[0].(map[string]any)["line_item_id"] = testArcheraOrg }, "row ID", testArcheraOrg},
		{func(w map[string]any) { hypLI(w)["line_item_id"] = testArcheraOrg }, "hypothetical ID", testArcheraOrg},
		{func(w map[string]any) { entryOf(w)["offer_id"] = testArcheraOrg }, "offer ID", testArcheraOrg},
		{func(w map[string]any) { candOf(w)["lease_menu_item_id"] = testArcheraOrg }, "lease ID", testArcheraOrg},
		{func(map[string]any) {}, "plan ID", testArcheraPlan},
		{func(map[string]any) {}, "term", "one_year"},
		{func(map[string]any) {}, "payment", "no_upfront"},
		{func(map[string]any) {}, "provider", "aws"},
		{func(map[string]any) {}, "exact decimal", "21.21"},
		{func(map[string]any) {}, "short numeric", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setArcheraEnv(t)
			t.Setenv(EnvArcheraAPIKey, tc.key)
			logs := captureLogs(t)
			var wire map[string]any
			require.NoError(t, json.Unmarshal(readWire(t), &wire))
			tc.mutate(wire)
			body, err := json.Marshal(wire)
			require.NoError(t, err)
			_, err = insurance.DecodeComparison(bytes.NewReader(body), testArcheraOrg, testArcheraPlan, time.Now())
			require.NoError(t, err, "successful-decoder boundary must be reached")
			rt := okTransport(body)
			res, protocol, err := callArchera(t, rt, nil)
			require.NoError(t, err)
			require.True(t, res.IsError, "whole result must be rejected")
			assert.Empty(t, res.StructuredContent)
			assert.NotContains(t, resultText(res), "monthly_730h")
			assert.Equal(t, 1, rt.count())
			assert.Equal(t, "https://api.archera.ai/v1/org/"+testArcheraOrg+"/commitment-plans/"+testArcheraPlan+"/comparison", rt.reqs[0].URL.String())
			assert.Equal(t, http.MethodGet, rt.reqs[0].Method)
			assert.True(t, rt.reqs[0].Header.Get("x-api-key") == tc.key)
			if len(tc.key) > 1 {
				checkSinks(t, res, protocol, tc.key, logs)
			}
		})
	}

	t.Run("negative controls", func(t *testing.T) {
		setArcheraEnv(t)
		logs := captureLogs(t)
		t.Setenv(EnvArcheraAPIKey, "")
		rt := okTransport(readWire(t))
		res, protocol, err := callArchera(t, rt, nil)
		require.NoError(t, err)
		require.True(t, res.IsError)
		assert.Zero(t, rt.count())
		assert.NotContains(t, protocol, testArcheraKey)
		t.Setenv(EnvArcheraAPIKey, testArcheraKey)
		var wire map[string]any
		require.NoError(t, json.Unmarshal(readWire(t), &wire))
		entryOf(wire)["contract_term"] = "invalid-" + testArcheraKey
		body, err := json.Marshal(wire)
		require.NoError(t, err)
		_, err = insurance.DecodeComparison(bytes.NewReader(body), testArcheraOrg, testArcheraPlan, time.Now())
		require.Error(t, err)
		for _, rt := range []*recordingTransport{okTransport(body), {status: http.StatusUnauthorized, body: []byte(`{"message":"bad key ` + testArcheraKey + `"}`)}} {
			res, protocol, err := callArchera(t, rt, nil)
			require.NoError(t, err)
			require.True(t, res.IsError)
			assert.Equal(t, 1, rt.count())
			assert.NotContains(t, resultText(res), testArcheraKey)
			assert.NotContains(t, protocol, testArcheraKey)
			full, err := json.Marshal(res)
			require.NoError(t, err)
			assert.NotContains(t, string(full), testArcheraKey)
		}
		for i, output := range logs {
			assert.NotContains(t, output.String(), testArcheraKey, "log sink %d", i)
		}
	})
}
