package tools

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// The MCPB manifest only supports an inclusive `min`, so a cap's manifest
// bound must be the tightest value that still admits everything the server's
// env parsing accepts. A float cap accepts any value > 0, so the tight
// inclusive bound is 0 (the server then rejects 0 itself); an integer cap
// accepts >= 1, so the bound is 1. This pins both to the real parsers.
func TestManifestMinMatchesServerCapBounds(t *testing.T) {
	raw, err := os.ReadFile("../mcpb/manifest.json")
	require.NoError(t, err)
	var m struct {
		Server struct {
			MCPConfig struct {
				Env map[string]string `json:"env"`
			} `json:"mcp_config"`
		} `json:"server"`
		UserConfig map[string]struct {
			Min *float64 `json:"min"`
		} `json:"user_config"`
	}
	require.NoError(t, json.Unmarshal(raw, &m))
	ref := regexp.MustCompile(`^\$\{user_config\.(\w+)\}$`)

	accepts := func(c capSetting, isInt bool, v float64) bool {
		t.Setenv(c.env, strconv.FormatFloat(v, 'f', -1, 64))
		var err error
		if isInt {
			_, err = c.readInt()
		} else {
			_, err = c.readFloat()
		}
		return err == nil
	}

	// The USD totals (capUSD, capBudget) are tracked by mcp#82.
	caps := []struct {
		c     capSetting
		isInt bool
	}{{capCount, true}, {capHourly, false}, {capMemory, false}}
	for _, tc := range caps {
		t.Run(tc.c.env, func(t *testing.T) {
			sub := ref.FindStringSubmatch(m.Server.MCPConfig.Env[tc.c.env])
			require.NotNil(t, sub, "manifest does not feed %s from a user_config key", tc.c.env)
			cfg, ok := m.UserConfig[sub[1]]
			require.True(t, ok, "user_config.%s missing", sub[1])
			require.NotNil(t, cfg.Min, "user_config.%s has no min", sub[1])
			min := *cfg.Min

			if tc.isInt {
				require.True(t, accepts(tc.c, true, min), "server rejects the manifest min %v", min)
				require.False(t, accepts(tc.c, true, min-1), "manifest min %v excludes a value %v the server accepts", min, min-1)
				return
			}
			require.False(t, accepts(tc.c, false, min), "the inclusive min %v must be a value the server rejects (server needs > 0)", min)
			require.True(t, accepts(tc.c, false, min+1e-9), "manifest min %v is above a value the server accepts", min)
		})
	}
}
