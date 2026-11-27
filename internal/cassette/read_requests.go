package cassette

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// InteractionFilter selects interactions by what the cassette stores in the
// interactions table. A zero field does not filter.
type InteractionFilter struct {
	Channel   string   // llm or mcp
	Method    string   // HTTP method
	Upstreams []string // any of these upstreams
}

// filterClause selects the interactions that match an InteractionFilter,
// with the arguments of filterArgs. It is constant, so that no SQL is built
// from values.
const filterClause = ` WHERE (? = '' OR i.channel = ?) AND (? = '' OR i.http_method = ?)
	AND (? = 0 OR i.upstream IN (SELECT value FROM json_each(?))) ORDER BY i.id`

func (f InteractionFilter) args() ([]any, error) {
	upstreams, err := json.Marshal(f.Upstreams)
	if err != nil {
		return nil, fmt.Errorf("encoding upstreams: %w", err)
	}
	return []any{f.Channel, f.Channel, f.Method, f.Method, len(f.Upstreams), string(upstreams)}, nil
}

// InteractionsFiltered returns the interactions that match f, in arrival
// order.
func (c *Cassette) InteractionsFiltered(ctx context.Context, f InteractionFilter) ([]Interaction, error) {
	reqs, err := c.queryInteractions(ctx, f, false)
	if err != nil {
		return nil, err
	}
	out := make([]Interaction, len(reqs))
	for i, r := range reqs {
		out[i] = r.Interaction
	}
	return out, nil
}

// InteractionWithRequest is an interaction together with its request body.
type InteractionWithRequest struct {
	Interaction
	// Request is the body stored under RequestBlob. It is nil when the
	// interaction has no request blob.
	Request []byte
	// RequestErr is the error that Blob returns for RequestBlob, when its row
	// is missing. It is per interaction, not for the whole read, because a
	// cassette is untrusted input and the caller may never need that body.
	RequestErr error
}

// InteractionsWithRequests returns the interactions that match f, in arrival
// order, with their request bodies, in one query instead of one Blob call
// per interaction. It holds every returned body in memory at once.
func (c *Cassette) InteractionsWithRequests(ctx context.Context, f InteractionFilter) ([]InteractionWithRequest, error) {
	return c.queryInteractions(ctx, f, true)
}

func (c *Cassette) queryInteractions(ctx context.Context, f InteractionFilter, withRequests bool) ([]InteractionWithRequest, error) {
	query := `SELECT ` + interactionColumns + ` FROM interactions i` + filterClause
	if withRequests {
		query = `SELECT ` + interactionColumns + `, b.sha256, b.data FROM interactions i
			LEFT JOIN blobs b ON b.sha256 = i.request_blob` + filterClause
	}
	args, err := f.args()
	if err != nil {
		return nil, err
	}
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading interactions of %s: %w", c.path, err)
	}
	defer rows.Close()

	var out []InteractionWithRequest
	for rows.Next() {
		var (
			key  sql.NullString
			data []byte
		)
		// The blob columns follow interactionColumns, so scanInteraction is
		// reused with two more destinations.
		scan := rows.Scan
		if withRequests {
			scan = func(dest ...any) error { return rows.Scan(append(dest, &key, &data)...) }
		}
		in, err := scanInteraction(scanFunc(scan))
		if err != nil && in.ID != 0 {
			return nil, fmt.Errorf("reading interaction %d of %s: %w", in.ID, c.path, err)
		}
		if err != nil {
			return nil, fmt.Errorf("reading interactions of %s: %w", c.path, err)
		}
		r := InteractionWithRequest{Interaction: in}
		if in.RequestBlob != nil {
			if key.Valid {
				r.Request = data
			} else {
				r.RequestErr = fmt.Errorf("reading blob %s: %w", *in.RequestBlob, ErrNotFound)
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading interactions of %s: %w", c.path, err)
	}
	return out, nil
}

// scanFunc adapts a function to the Scan method that scanInteraction needs.
type scanFunc func(dest ...any) error

func (f scanFunc) Scan(dest ...any) error { return f(dest...) }
