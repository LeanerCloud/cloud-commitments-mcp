//go:build !(linux || darwin)

package tools

import (
	"fmt"
	"runtime"
	"time"
)

// lockBudgetFile fails closed: a shared budget file without cross-process
// locking would silently allow overspend, so it is unsupported here.
func lockBudgetFile(path string, _ time.Duration) (func(), error) {
	return nil, fmt.Errorf("budget state file %s needs cross-process file locking, which is not supported on %s", path, runtime.GOOS)
}
