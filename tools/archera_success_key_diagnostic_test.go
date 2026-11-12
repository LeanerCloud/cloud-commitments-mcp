package tools

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
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
	checkSinks := func(t *testing.T, res *mcp.CallToolResult, protocol string, logs []*syncBuffer) {
		text := resultText(res)
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(text), &parsed))
		parsedJSON, err := json.Marshal(parsed)
		require.NoError(t, err)
		structuredJSON, err := json.Marshal(res.StructuredContent)
		require.NoError(t, err)
		fullJSON, err := json.Marshal(res)
		require.NoError(t, err)
		for _, sink := range []struct{ name, value string }{
			{"text", text}, {"parsed text JSON", string(parsedJSON)},
			{"StructuredContent", string(structuredJSON)}, {"full result", string(fullJSON)}, {"protocol", protocol},
			{"standard log", logs[0].String()}, {"slog", logs[1].String()}, {"shared log", logs[2].String()},
		} {
			assert.NotContains(t, sink.value, testArcheraKey, "sink=%s", sink.name)
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
				outputField := field
				if field == "control" {
					offer["guaranteed_display_name"], offer["region"], entry["commitment_type"] = "Ordinary offer", "eu-west-1", "aws/AmazonEC2"
				} else if field == "commitment_type" {
					entry[field] = want
				} else {
					offer[field] = want
					if field == "guaranteed_display_name" {
						outputField = "archera_offer_name"
					}
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
				require.False(t, res.IsError, "setup: error path is inconclusive")
				require.Equal(t, 1, rt.count())
				request := rt.reqs[0]
				assert.Equal(t, http.MethodGet, request.Method)
				assert.Equal(t, "https", request.URL.Scheme)
				assert.Equal(t, "api.archera.ai", request.URL.Host)
				assert.Equal(t, "/v1/org/"+testArcheraOrg+"/commitment-plans/"+testArcheraPlan+"/comparison", request.URL.Path)
				assert.Empty(t, request.URL.RawQuery)
				assert.True(t, request.Header.Get("x-api-key") == testArcheraKey, "credential is confined to intercepted header")
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
					if field == "control" {
						assert.Equal(t, "Ordinary offer", returned["archera_offer_name"])
						assert.Equal(t, "eu-west-1", returned["region"])
						assert.Equal(t, "aws/AmazonEC2", returned["commitment_type"])
					} else {
						assert.Equal(t, want, returned[outputField], "selected field reachability")
					}
					upfront := "21.21"
					if role == "candidate" {
						upfront = "34.34"
					}
					assert.Equal(t, upfront, returned["upfront_one_time_cost"])
				}
				t.Log("real decoder and CallTool success established before sink checks")
				checkSinks(t, res, protocol, logs)
			})
		}
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
