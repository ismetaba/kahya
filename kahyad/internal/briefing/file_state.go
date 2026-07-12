// file_state.go persists the watched-file collector's own "since last
// run" cutoff (collect_files.go) as a single RFC3339 timestamp in a plain
// text file - deliberately NOT a brain.db table: this value is a cheap,
// non-security-relevant scheduling cursor (kahyad remains brain.db's ONLY
// writer regardless; this file simply never touches brain.db at all,
// avoiding any question of whether writing it would violate that rule).
package briefing

import (
	"os"
	"strings"
	"time"
)

// FileScanState is the file collector's last-run cutoff, stored at Path.
type FileScanState struct {
	Path string
}

// Load returns the persisted cutoff, or the zero time.Time (meaning
// "collect everything - no prior run recorded yet") when Path does not
// exist, is empty, or fails to parse as RFC3339 - a corrupt/foreign state
// file degrades to "no prior run" rather than failing the whole
// collector closed (this value's only consequence is which ALREADY-
// non-secret files get reported as "changed", never a security decision).
func (s FileScanState) Load() (time.Time, error) {
	if s.Path == "" {
		return time.Time{}, nil
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		return time.Time{}, nil
	}
	return t, nil
}

// Save persists t as the new cutoff. A no-op when Path is empty.
func (s FileScanState) Save(t time.Time) error {
	if s.Path == "" {
		return nil
	}
	return os.WriteFile(s.Path, []byte(t.UTC().Format(time.RFC3339)), 0o600)
}
