package storage

import "testing"

// Every advisory lock this package takes must have its own key: two features
// that pick the same number serialize against each other by accident, and
// nobody reading either one can see it. (SPEC-backup-import's import lock
// first shipped with the key SPEC-household-basics had already used for the
// account lock.)
func TestAdvisoryLockKeysAreDistinct(t *testing.T) {
	keys := map[string]int64{
		"migrateLockKey":  migrateLockKey,
		"accountsLockKey": accountsLockKey,
		"importLockKey":   importLockKey,
	}
	seen := map[int64]string{}
	for name, k := range keys {
		if other, dup := seen[k]; dup {
			t.Errorf("%s and %s share advisory lock key %#x", name, other, k)
		}
		seen[k] = name
	}
}
