package cassette

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ErrInvalidSessionID is returned for a session id that cannot name a
// cassette file (spec §9.2).
var ErrInvalidSessionID = errors.New("invalid session id")

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// windowsReserved holds the device names Windows reserves in any file name,
// with or without an extension. They are rejected on every platform so that
// cassettes can be copied anywhere (spec §9.2).
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// ValidateSessionID reports whether id can name a cassette file: it never
// contains a path separator, never starts with a dot, and is portable across
// file systems (spec §9.2).
func ValidateSessionID(id string) error {
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("%w %q: use 1 to 128 letters, digits, '.', '_' or '-', starting with a letter or digit", ErrInvalidSessionID, id)
	}
	base, _, _ := strings.Cut(id, ".")
	if windowsReserved[strings.ToUpper(base)] {
		return fmt.Errorf("%w %q: %s is a reserved device name on Windows", ErrInvalidSessionID, id, base)
	}
	return nil
}

// Path returns the file of session id in dir. id must be valid.
func Path(dir, id string) string {
	return filepath.Join(dir, id+".cassette")
}

// NewSessionID returns a generated id, YYYYMMDD-HHMMSS-xxxx in local time with
// four random hex digits (spec §9.2), that has no cassette in dir yet.
func NewSessionID(dir string, now time.Time, random io.Reader) (string, error) {
	for range 16 {
		var b [2]byte
		if _, err := io.ReadFull(random, b[:]); err != nil {
			return "", fmt.Errorf("generating session id: %w", err)
		}
		id := fmt.Sprintf("%s-%02x%02x", now.Local().Format("20060102-150405"), b[0], b[1])
		if _, err := os.Stat(Path(dir, id)); errors.Is(err, os.ErrNotExist) {
			return id, nil
		}
	}
	return "", errors.New("generating session id: every candidate already exists")
}
