package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// NewRelic runs NRQL queries through New Relic's NerdGraph API. It only
// ever sends the read-only `nrql` query, never a mutation.
type NewRelic struct {
	http      httpClient
	accountID int64
	sem       chan struct{}
}

// newRelicConcurrency follows New Relic's guidance of at most five complex
// queries in flight at once.
const newRelicConcurrency = 5

// nrqlTimeout is the NRQL timeout in seconds (NerdGraph's default is 5).
const nrqlTimeout = 60

// NewNewRelic returns a NerdGraph client for one account.
func NewNewRelic(endpoint, apiKey string, accountID int64, timeout time.Duration) *NewRelic {
	if timeout < (nrqlTimeout+5)*time.Second {
		timeout = (nrqlTimeout + 5) * time.Second
	}
	c := newHTTP(endpoint, "", timeout)
	c.name = "newrelic"
	c.header = map[string]string{"API-Key": apiKey}
	// The endpoint is the full NerdGraph URL; requests post to it as is.
	return &NewRelic{http: c, accountID: accountID, sem: make(chan struct{}, newRelicConcurrency)}
}

const nerdgraphQuery = `query($id: Int!, $nrql: Nrql!) { actor { account(id: $id) { nrql(query: $nrql, timeout: %d) { results } } } }`

// NRQL runs one NRQL query and returns its result rows.
func (n *NewRelic) NRQL(ctx context.Context, nrql string) ([]map[string]any, error) {
	if n.http.header["API-Key"] == "" {
		return nil, fmt.Errorf("%w: no New Relic API key (set NEW_RELIC_API_KEY or newrelic.api_key)", ErrUnavailable)
	}
	select {
	case n.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-n.sem }()
	body, err := json.Marshal(map[string]any{
		"query":     fmt.Sprintf(nerdgraphQuery, nrqlTimeout),
		"variables": map[string]any{"id": n.accountID, "nrql": nrql},
	})
	if err != nil {
		return nil, err
	}
	out, err := n.http.postJSON(ctx, "", body, nrql)
	if err != nil {
		return nil, err
	}
	var r struct {
		Data struct {
			Actor struct {
				Account *struct {
					NRQL *struct {
						Results []map[string]any `json:"results"`
					} `json:"nrql"`
				} `json:"account"`
			} `json:"actor"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("decode NerdGraph response: %w", err)
	}
	if len(r.Errors) > 0 {
		msgs := make([]string, len(r.Errors))
		for i, e := range r.Errors {
			msgs[i] = e.Message
		}
		msg := strings.Join(msgs, "; ")
		if strings.Contains(strings.ToLower(msg), "api key") || strings.Contains(strings.ToLower(msg), "unauthorized") || strings.Contains(strings.ToLower(msg), "access denied") {
			return nil, fmt.Errorf("%w: New Relic: %s", ErrUnavailable, msg)
		}
		return nil, fmt.Errorf("NRQL: %s", msg)
	}
	if r.Data.Actor.Account == nil || r.Data.Actor.Account.NRQL == nil {
		return nil, fmt.Errorf("%w: New Relic returned no data for account %d (check the account ID and the key's access)", ErrUnavailable, n.accountID)
	}
	res := r.Data.Actor.Account.NRQL.Results
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		preview, _ := json.Marshal(res)
		if len(preview) > 600 {
			preview = append(preview[:600], "…"...)
		}
		slog.Debug("nrql result", slog.String("nrql", nrql), slog.Int("rows", len(res)), slog.String("preview", string(preview)))
	}
	return res, nil
}

// NRQLTime formats t as epoch milliseconds for SINCE/UNTIL.
func NRQLTime(t time.Time) string { return fmt.Sprintf("%d", t.UnixMilli()) }

// NRQLRange returns the SINCE/UNTIL clause for [start, end].
func NRQLRange(start, end time.Time) string {
	return " SINCE " + NRQLTime(start) + " UNTIL " + NRQLTime(end)
}

// NRQLNumber reads a numeric result value: a number, or the first number
// inside an object (percentile results look like {"95": 0.42}).
func NRQLNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case map[string]any:
		for _, k := range sortedKeys(x) {
			if f, ok := NRQLNumber(x[k]); ok {
				return f, true
			}
		}
	}
	return 0, false
}

// NRQLString reads a string result value ("" for null).
func NRQLString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return fmt.Sprintf("%g", x)
	case bool:
		return fmt.Sprintf("%t", x)
	}
	return ""
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0 && ks[j] < ks[j-1]; j-- {
			ks[j], ks[j-1] = ks[j-1], ks[j]
		}
	}
	return ks
}
