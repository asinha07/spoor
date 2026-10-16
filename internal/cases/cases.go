// Package cases defines the tool calls exercised per scenario. The integration
// test records backend responses for exactly these calls and the golden tests
// replay them, so both sides must use this one list.
package cases

import (
	"context"
	"fmt"

	"github.com/asinha07/spoor/internal/digest"
	"github.com/asinha07/spoor/internal/gen"
)

// Case is one tool call.
type Case struct {
	Name string
	Tool string // what_changed, new_errors, trace_diff, list_services
	Req  digest.Request
}

// For returns the calls made for one scenario.
func For(t gen.GroundTruth) []Case {
	base := digest.Request{Since: t.Since, Until: t.Until}
	big := base
	big.BudgetTokens = digest.MaxBudget
	week := big
	week.Baseline = "7d" // generated scenarios never have data a week earlier
	cs := []Case{
		{"list_services", "list_services", base},
		{"what_changed_all", "what_changed", big},
		{"what_changed_all_default_budget", "what_changed", base},
		{"what_changed_all_empty_7d_baseline", "what_changed", week},
	}
	if t.Service == "" {
		return append(cs,
			Case{"new_errors_all", "new_errors", big},
			Case{"trace_diff_all", "trace_diff", big},
		)
	}
	svc := big
	svc.Service = t.Service
	return append(cs,
		Case{"what_changed_service", "what_changed", svc},
		Case{"new_errors_service", "new_errors", svc},
		Case{"trace_diff_service", "trace_diff", svc},
	)
}

// Run executes c against e.
func Run(ctx context.Context, e *digest.Engine, c Case) (any, error) {
	switch c.Tool {
	case "what_changed":
		return e.WhatChanged(ctx, c.Req)
	case "new_errors":
		return e.NewErrors(ctx, c.Req)
	case "trace_diff":
		return e.TraceDiff(ctx, c.Req)
	case "list_services":
		return e.ListServices(ctx, c.Req.Since, c.Req.Until)
	}
	return nil, fmt.Errorf("unknown tool %q", c.Tool)
}
