package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const maskTestKey = "test-key-SYNTHETIC-0000"

// splitKey splices sep after every character of the key.
func splitKey(sep string) string {
	return strings.Join(strings.Split(maskTestKey, ""), sep)
}

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

// maskCases are echoes the strip-then-mask pipeline must fully redact.
func maskCases() map[string]string {
	zw, bidi := "\u200b", "\u202e"
	return map[string]string{
		"plain":            "bad key " + maskTestKey,
		"zero-width split": "bad key " + splitKey(zw),
		"bidi split":       "bad key " + splitKey(bidi),
		"control split":    "bad key " + splitKey("\x01"),
		"newline split":    "bad key " + splitKey("\n"),
		"twice":            "bad key " + maskTestKey + " and " + splitKey(zw),
		"twice both split": splitKey(zw) + " then " + splitKey(bidi),
		"straddles cap":    strings.Repeat("a", 250) + maskTestKey,
		"straddles split":  strings.Repeat("a", 250) + splitKey(zw),
		"last 10 bytes":    strings.Repeat("a", 246) + splitKey(zw),
		"invalid utf-8":    "\xff" + maskTestKey + "\xfe",
	}
}

// withholdCases are splitters the strip leaves in place: the key stays split
// (or displays as the key), so the whole text must be withheld.
func withholdCases() map[string]string {
	seps := map[string]string{
		"combining acute":     "\u0301",
		"enclosing mark":      "\u0488",
		"variation selector":  "\ufe0f",
		"variation sel supp":  "\U000e0100",
		"cgj":                 "\u034f",
		"hangul choseong":     "\u115f",
		"hangul jungseong":    "\u1160",
		"hangul filler":       "\u3164",
		"halfwidth hangul":    "\uffa0",
		"space":               " ",
		"no-break space":      "\u00a0",
		"ideographic space":   "\u3000",
		"invalid byte":        "\xff",
		"replacement char":    "\ufffd",
		"invalid and combine": "\xff\u0301",
	}
	out := map[string]string{}
	for n, sep := range seps {
		out[n] = "bad key " + splitKey(sep)
	}
	return out
}

func TestArcheraErrorMasksKeyAfterSanitizing(t *testing.T) {
	t.Parallel()
	for name, msg := range maskCases() {
		t.Run(name, func(t *testing.T) {
			got := archeraError(errors.New(msg), maskTestKey).Error()
			assert.NotContains(t, got, maskTestKey)
			assert.LessOrEqual(t, len(got), 256)
			assert.NotEqual(t, archeraWithheld, got, "a fully maskable echo is redacted, not withheld")
		})
	}
}

func TestArcheraErrorWithholdsTextWhenKeyStaysReadable(t *testing.T) {
	t.Parallel()
	for name, msg := range withholdCases() {
		t.Run(name, func(t *testing.T) {
			got := archeraError(errors.New(msg), maskTestKey)
			assert.EqualError(t, got, archeraWithheld)
			assert.NotContains(t, got.Error(), maskTestKey)
		})
	}
}

func TestArcheraErrorWithholdsWholeTextWithKeyAnywhere(t *testing.T) {
	t.Parallel()
	fe0f, ff := splitKey("\ufe0f"), splitKey("\xff")
	half := maskTestKey[:10] + "\ufe0f" + maskTestKey[10:]
	cases := map[string]string{
		"text after":          "bad key " + fe0f + " was rejected",
		"key at start":        fe0f + " was rejected",
		"key in the middle":   "org x: " + ff + " not valid for plan y",
		"split at one place":  "bad key " + half + " was rejected",
		"two splitters":       "bad key " + maskTestKey[:5] + "\ufe0f" + maskTestKey[5:12] + "\xff" + maskTestKey[12:] + " tail",
		"split key then text": "bad key " + fe0f + strings.Repeat("z", 400),
	}
	for pad := 200; pad <= 262; pad++ {
		cases[fmt.Sprintf("straddles cut pad %d", pad)] = strings.Repeat("a", pad) + fe0f + " was rejected"
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			assert.EqualError(t, archeraError(errors.New(msg), maskTestKey), archeraWithheld)
		})
	}
}

func TestArcheraErrorTrimsKeyBeforeMasking(t *testing.T) {
	t.Parallel()
	got := archeraError(errors.New("bad key "+splitKey("\u200b")), " "+maskTestKey+"\n").Error()
	assert.Equal(t, "bad key [redacted]", got)
}

func TestArcheraErrorLeavesNoKeyPrefixAtCap(t *testing.T) {
	t.Parallel()
	for _, echo := range []string{maskTestKey, splitKey("\u200b")} {
		got := archeraError(errors.New(strings.Repeat("a", 244)+echo), maskTestKey).Error()
		assert.NotContains(t, got, maskTestKey[:8])
	}
}

func TestArcheraErrorContextErrorsAreSanitizedAndKeepIdentity(t *testing.T) {
	t.Parallel()
	assert.Same(t, context.Canceled, archeraError(context.Canceled, maskTestKey))
	for name, echo := range map[string]string{
		"plain": maskTestKey, "split": splitKey("\u200b"), "unstripped split": splitKey("\u0301"),
	} {
		for _, base := range []error{context.DeadlineExceeded, context.Canceled} {
			t.Run(name, func(t *testing.T) {
				got := archeraError(fmt.Errorf("dial %s: %w", echo, base), maskTestKey)
				assert.NotContains(t, got.Error(), maskTestKey)
				assert.ErrorIs(t, got, base)
			})
		}
	}
}

func TestArcheraCleanStringCapBoundary(t *testing.T) {
	t.Parallel()
	assert.Len(t, archeraCleanString(strings.Repeat("a", 256)), 256)
	assert.Len(t, archeraCleanString(strings.Repeat("a", 257)), 256)
}

func TestArcheraErrorMasksKeyOnRealPaths(t *testing.T) {
	check := func(t *testing.T, rt http.RoundTripper) {
		t.Helper()
		res, protocol, err := callArchera(t, rt, nil)
		require.NoError(t, err)
		require.True(t, res.IsError)
		assert.NotContains(t, resultText(res), maskTestKey)
		assert.NotContains(t, protocol, maskTestKey)
	}
	all := maskCases()
	for n, v := range withholdCases() {
		all["withhold "+n] = v
	}
	for name, echo := range all {
		t.Run("decode "+name, func(t *testing.T) {
			setArcheraEnv(t)
			t.Setenv(EnvArcheraAPIKey, maskTestKey)
			var w map[string]any
			require.NoError(t, json.Unmarshal(readWire(t), &w))
			entryOf(w)["contract_term"] = echo
			body, err := json.Marshal(w)
			require.NoError(t, err)
			check(t, okTransport(body))
		})
		t.Run("transport "+name, func(t *testing.T) {
			setArcheraEnv(t)
			t.Setenv(EnvArcheraAPIKey, maskTestKey)
			check(t, failingTransport{errors.New(echo)})
		})
		t.Run("transport deadline "+name, func(t *testing.T) {
			setArcheraEnv(t)
			t.Setenv(EnvArcheraAPIKey, maskTestKey)
			check(t, failingTransport{fmt.Errorf("%s: %w", echo, context.DeadlineExceeded)})
		})
	}
}

func TestArcheraErrorLeavesNoKeyPrefixAtAnyCut(t *testing.T) {
	t.Parallel()
	for name, sep := range map[string]string{"variation selector": "\ufe0f", "invalid byte": "\xff"} {
		for pad := 220; pad <= 262; pad++ {
			got := archeraError(errors.New(strings.Repeat("a", pad)+splitKey(sep)), maskTestKey).Error()
			for k := 6; k <= len(maskTestKey); k++ {
				require.NotContains(t, archeraSkeleton(got), maskTestKey[:k], "%s pad %d", name, pad)
			}
		}
	}
}
