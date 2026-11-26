// Package session holds the parts of a replay of a recorded session that
// compose the providers: the handler that routes its requests and the helper
// that chooses its id. It will host the replay's run (#188).
package session

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/pmlucero/retrace/internal/cassette"
)

// ErrNoDefaultID reports that the default replay id cannot be used, which is
// a usage error (spec §9.3). Other NextID errors are runtime errors.
var ErrNoDefaultID = errors.New("no default replay id")

// NextID returns the default id of a replay of source: source.mode.n,
// with n one more than the highest in use, or 1 (spec §9.3). Only an n in
// decimal digits, without leading zeros and below math.MaxInt64, counts: a
// crafted file name must not make n+1 overflow.
func NextID(dir, source, mode string) (string, error) {
	prefix := source + "." + mode + "."
	// Not filepath.Glob: --dir may hold pattern characters such as '['.
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("listing replays of %s: %w", source, err)
	}
	n := 0
	for _, e := range entries {
		s, hasPrefix := strings.CutPrefix(e.Name(), prefix)
		s, hasSuffix := strings.CutSuffix(s, ".cassette")
		if !hasPrefix || !hasSuffix {
			continue
		}
		if s == "" || s[0] == '0' || strings.Trim(s, "0123456789") != "" {
			continue // not "07", "+7" or "x"
		}
		if k, err := strconv.Atoi(s); err == nil && k > n && k < math.MaxInt64 {
			n = k
		}
	}
	if n >= math.MaxInt64-1 {
		return "", fmt.Errorf("%w: the default replay number of %s would reach 2^63 - 1; name the replay with --as", ErrNoDefaultID, source)
	}
	id := fmt.Sprintf("%s%d", prefix, n+1)
	if err := cassette.ValidateSessionID(id); err != nil {
		return "", fmt.Errorf("%w: %w; name the replay with --as", ErrNoDefaultID, err)
	}
	return id, nil
}
