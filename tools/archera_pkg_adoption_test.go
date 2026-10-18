package tools

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArcheraComparisonSelectedAmountContract(t *testing.T) {
	setArcheraEnv(t)
	cases := []struct {
		value   any
		name    string
		missing bool
		invalid bool
	}{
		{name: "missing", missing: true, invalid: true},
		{name: "null", invalid: true},
		{name: "negative", value: json.Number("-0.01"), invalid: true},
		{name: "zero", value: json.Number("0")},
		{name: "positive", value: json.Number("2")},
	}
	for _, location := range []struct {
		name      string
		candidate int
	}{{name: "current", candidate: -1}, {name: "first candidate", candidate: 0}, {name: "second candidate", candidate: 1}} {
		for _, tc := range cases {
			t.Run(location.name+"/"+tc.name, func(t *testing.T) {
				var wire map[string]any
				require.NoError(t, json.Unmarshal(readWire(t), &wire))
				entry := entryOf(wire)
				if location.candidate >= 0 {
					row := wire["data"].([]any)[0].(map[string]any)
					entry = row["candidates"].([]any)[location.candidate].(map[string]any)
				}
				if tc.missing {
					delete(entry, "selected_amount")
				} else {
					entry["selected_amount"] = tc.value
				}
				body, err := json.Marshal(wire)
				require.NoError(t, err)
				transport := okTransport(body)
				result, protocol, err := callArchera(t, transport, nil)
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.Equal(t, 1, transport.count())
				assert.NotContains(t, protocol, testArcheraKey)
				if tc.invalid {
					require.True(t, result.IsError, "invalid selected_amount must be refused")
					assert.Contains(t, resultText(result), "selected_amount")
					assert.Empty(t, result.StructuredContent)
				} else {
					got := structured(t, result)
					assert.Len(t, got["rows"], 1)
				}
			})
		}
	}
}
