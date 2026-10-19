//go:build !(linux || darwin)

package tools

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// budgetLockSupported mirrors the build constraint of budget_lock_unix.go.
const budgetLockSupported = false

func TestBudgetLockUnsupportedPlatformFailsClosed(t *testing.T) {
	_, err := lockBudgetFile(filepath.Join(t.TempDir(), "b.lock"), time.Second)
	require.ErrorContains(t, err, "not supported")
}
