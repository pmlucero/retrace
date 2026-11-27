package cassette

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// InteractionFilter selects interactions by what the cassette stores in the
// interactions table. A zero field does not filter.
type InteractionFilter struct {
	Channel   string   // llm or mcp
	Method    string   // HTTP method
	Upstreams []string // any of these upstreams
}

// where returns the WHERE clause that selects f, with its arguments. Columns
// are qualified with the alias i.
func (f InteractionFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.Channel != "" {
		conds = append(conds, "i.channel = ?")
		args = append(args, f.Channel)
	}
	if f.Method != "" {
		conds = append(conds, "i.http_method = ?")
		args = append(args, f.Method)
	}
	if len(f.Upstreams) > 0 {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(f.Upstreams)), ",")
		conds = append(conds, "i.upstream IN ("+marks+")")
		for _, u := range f.Upstreams {
			args = append(args, u)
		}
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
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

// InteractionRequest is an interaction together with its request body.
type InteractionRequest struct {
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
func (c *Cassette) InteractionsWithRequests(ctx context.Context, f InteractionFilter) ([]InteractionRequest, error) {
	return c.queryInteractions(ctx, f, true)
}

func (c *Cassette) queryInteractions(ctx context.Context, f InteractionFilter, withRequests bool) ([]InteractionRequest, error) {
	cols, join := interactionColumns, ""
	if withRequests {
		cols += ", b.sha256, b.data"
		join = " LEFT JOIN blobs b ON b.sha256 = i.request_blob"
	}
	where, args := f.where()
	rows, err := c.db.QueryContext(ctx, `SELECT `+cols+` FROM interactions i`+join+where+` ORDER BY i.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading interactions of %s: %w", c.path, err)
	}
	defer rows.Close()

	var out []InteractionRequest
	for rows.Next() {
		var (
			key  sql.NullString
			data []byte
		)
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
		r := InteractionRequest{Interaction: in}
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
