package session

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/pmlucero/retrace/internal/cassette"
)

// InsertFunc writes a new cassette's session row. Tests pass one that fails
// to make the insert fail; nil means (*cassette.Tx).InsertSession. The
// receiver comes first, and so ctx second, because it mirrors that method
// expression.
type InsertFunc func(tx *cassette.Tx, ctx context.Context, s cassette.Session) error

func (f InsertFunc) orDefault() InsertFunc {
	if f == nil {
		return (*cassette.Tx).InsertSession
	}
	return f
}

// CreateCassette creates the cassette at path with session row s. If the row
// cannot be written, it closes and removes the file, so a failed start leaves
// no cassette without a session and the id stays free (#56). Errors from
// cassette.Create are wrapped, so callers can test for cassette.ErrExists.
// Errors name the path or the session already.
func CreateCassette(ctx context.Context, path string, s cassette.Session, insert InsertFunc) (*cassette.Cassette, error) {
	insert = insert.orDefault()
	c, err := cassette.Create(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := c.Write(ctx, func(tx *cassette.Tx) error { return insert(tx, ctx, s) }); err != nil {
		// Create made this file (it refuses an existing one), so removing it
		// cannot touch another session's cassette.
		return nil, errors.Join(err, c.Close(), cassette.Remove(path))
	}
	return c, nil
}

// ReplaceCassette creates the cassette at path with session row s, replacing
// the cassette there, if any (record --force, spec §9.2). The old cassette is
// kept unless the new one gets its session row (#111).
func ReplaceCassette(ctx context.Context, path string, s cassette.Session, insert InsertFunc) (*cassette.Cassette, error) {
	insert = insert.orDefault()
	return cassette.Replace(ctx, path, func(tx *cassette.Tx) error { return insert(tx, ctx, s) })
}

// createReplayCassette creates the cassette of a replay with session row s.
// An explicit id must be new (spec §9.3); a default one moves on to the next
// n if another replay took it first, unless that n would not fit the id
// format or would reach math.MaxInt64. It returns the id it used.
func createReplayCassette(ctx context.Context, dir, id string, explicit bool, s cassette.Session, insert InsertFunc) (*cassette.Cassette, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("creating %s: %w", dir, err)
	}
	for range 100 {
		s.ID = id
		c, err := CreateCassette(ctx, cassette.Path(dir, id), s, insert)
		if errors.Is(err, cassette.ErrExists) && !explicit {
			prefix := strings.TrimRightFunc(id, func(r rune) bool { return r >= '0' && r <= '9' })
			n, _ := strconv.Atoi(strings.TrimPrefix(id, prefix))
			if n >= math.MaxInt64-1 {
				break
			}
			next := prefix + strconv.Itoa(n+1)
			if cassette.ValidateSessionID(next) != nil {
				break
			}
			id = next
			continue
		}
		if errors.Is(err, cassette.ErrExists) {
			return nil, "", fmt.Errorf("session %q already exists in %s", id, dir)
		}
		if err != nil {
			return nil, "", err
		}
		return c, id, nil
	}
	return nil, "", fmt.Errorf("no free default id after %s; name the replay with --as", id)
}
