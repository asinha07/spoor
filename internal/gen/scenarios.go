package gen

import (
	"fmt"
	"sort"
	"time"
)

// Effect changes one service's behaviour during the fault window. progress is
// 0 at the start of the window and 1 at its end, for faults that build up.
type Effect struct {
	ErrProb     func(progress float64, productID string) float64
	LatencyMult func(progress float64) float64
	CPU         func(progress float64) float64 // multiplier on baseline CPU
	Memory      func(progress float64) float64 // multiplier on baseline memory
	Lag         func(progress float64) float64 // absolute consumer lag
	ErrorLog    string                         // replaces the service's error pattern when set
}

// Scenario is one reproducible incident plus its ground truth.
type Scenario struct {
	Name        string
	Description string
	Effects     map[string]Effect
	// Truth: the faulty service and the finding kinds that count as identifying it.
	TruthService string
	TruthKinds   []string
	TruthDetail  string
}

func constant(v float64) func(float64) float64 { return func(float64) float64 { return v } }
func ramp(from, to float64) func(float64) float64 {
	return func(p float64) float64 { return from + (to-from)*p }
}

// Scenarios are the six benchmark incidents, mirroring the plan.
var Scenarios = []Scenario{
	{
		Name:        "payment-failure",
		Description: "The card processor rejects 40% of charges; checkout and frontend fail as a result.",
		Effects: map[string]Effect{
			"payment": {
				ErrProb:  func(float64, string) float64 { return 0.4 },
				ErrorLog: "charge failed: card processor returned 503 for order {n}",
			},
		},
		TruthService: "payment",
		TruthKinds:   []string{"error_rate_up", "new_log_pattern", "span_errors_up"},
		TruthDetail:  "payment service: failing charge call",
	},
	{
		Name:        "catalog-one-product",
		Description: "The product catalog fails for exactly one product ID.",
		Effects: map[string]Effect{
			"product-catalog": {
				ErrProb: func(_ float64, id string) float64 {
					if id == productIDs[0] {
						return 1
					}
					return 0.0005
				},
				ErrorLog: "failed to load product {id}: feature flag productCatalogFailure enabled",
			},
		},
		TruthService: "product-catalog",
		TruthKinds:   []string{"error_rate_up", "new_log_pattern", "span_errors_up"},
		TruthDetail:  "product-catalog: one product ID (" + productIDs[0] + ")",
	},
	{
		Name:        "ad-high-cpu",
		Description: "The ad service saturates its CPU and slows down.",
		Effects: map[string]Effect{
			"ad": {CPU: constant(9), LatencyMult: constant(6)},
		},
		TruthService: "ad",
		TruthKinds:   []string{"resource_up", "latency_up", "span_slower"},
		TruthDetail:  "ad service: CPU saturation",
	},
	{
		Name:        "recommendation-cache-leak",
		Description: "The recommendation cache leaks memory; latency creeps up with it.",
		Effects: map[string]Effect{
			"recommendation": {Memory: ramp(1.3, 5), LatencyMult: ramp(1.2, 3.5)},
		},
		TruthService: "recommendation",
		TruthKinds:   []string{"resource_up", "latency_up", "span_slower"},
		TruthDetail:  "recommendation service: cache memory leak",
	},
	{
		Name:        "kafka-queue-problems",
		Description: "The accounting consumer falls behind on the orders topic.",
		Effects: map[string]Effect{
			"accounting": {Lag: ramp(500, 6000), LatencyMult: constant(5)},
		},
		TruthService: "accounting",
		TruthKinds:   []string{"resource_up", "span_slower"},
		TruthDetail:  "accounting: Kafka consumer lag on orders",
	},
	{
		Name:        "control",
		Description: "No fault. Nothing should score at or above the report threshold.",
		Effects:     map[string]Effect{},
	},
}

// ScenarioByName finds a scenario.
func ScenarioByName(name string) (Scenario, error) {
	for _, s := range Scenarios {
		if s.Name == name {
			return s, nil
		}
	}
	names := make([]string, len(Scenarios))
	for i, s := range Scenarios {
		names[i] = s.Name
	}
	sort.Strings(names)
	return Scenario{}, fmt.Errorf("unknown scenario %q (have %v)", name, names)
}

// Plan fixes when a scenario's data starts and when its fault window runs.
type Plan struct {
	Scenario   Scenario
	DataStart  time.Time // first sample
	FaultStart time.Time
	FaultEnd   time.Time // last sample; the window under test is [FaultStart, FaultEnd)
	Seed       int64
}

// GroundTruth is written next to the data so tests and the benchmark can grade answers.
type GroundTruth struct {
	Scenario    string   `json:"scenario"`
	Description string   `json:"description"`
	Since       string   `json:"since"`
	Until       string   `json:"until"`
	Service     string   `json:"service,omitempty"`
	Kinds       []string `json:"kinds,omitempty"`
	Detail      string   `json:"detail,omitempty"`
}

// Truth returns the plan's ground truth record.
func (p Plan) Truth() GroundTruth {
	return GroundTruth{
		Scenario:    p.Scenario.Name,
		Description: p.Scenario.Description,
		Since:       p.FaultStart.UTC().Format(time.RFC3339),
		Until:       p.FaultEnd.UTC().Format(time.RFC3339),
		Service:     p.Scenario.TruthService,
		Kinds:       p.Scenario.TruthKinds,
		Detail:      p.Scenario.TruthDetail,
	}
}
