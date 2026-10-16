// Package gen produces synthetic microservice telemetry with injected faults,
// so Spoor can be tested against real backends without a heavy demo app.
//
// The model is a small shop: a frontend calls catalog, recommendation, ad and
// checkout services; checkout calls payment and publishes orders that an
// accounting service consumes from Kafka. Every request walks its call tree,
// so a failure or slowdown deep in the tree shows up in callers too, as it
// would in a real system.
package gen

import "time"

// call is one operation and the downstream calls it makes.
type call struct {
	service  string
	op       string        // span name of the server span
	client   string        // span name the caller records for this call
	median   time.Duration // own work, before children
	sigma    float64       // log-normal spread
	errProb  float64
	children []*call
	errorLog string // pattern logged at ERROR when this call fails on its own
}

// entry is a frontend operation with its request rate.
type entry struct {
	root *call
	rps  float64
}

// Product IDs requested from the catalog. The one-product fault targets the first.
var productIDs = []string{"OLJCESPC7Z", "66VCHSJNUP", "1YMWWN1N4O", "L9ECAV7KIM", "2ZYFJ3GM2N", "0PUK6V6EV0", "LS4PSXUNUM", "9SIQT8TOJO", "6E92ZMYYFZ", "HQTGWGPNH4"}

func topology() []entry {
	catalog := func() *call {
		return &call{service: "product-catalog", op: "GET /products/{id}", client: "catalog.GetProduct", median: 8 * time.Millisecond, sigma: 0.35, errProb: 0.0005,
			errorLog: "failed to load product {id}: database query returned error"}
	}
	payment := &call{service: "payment", op: "POST /charge", client: "payment.Charge", median: 45 * time.Millisecond, sigma: 0.4, errProb: 0.001,
		errorLog: "charge declined for order {n}: insufficient funds"}
	checkout := &call{service: "checkout", op: "POST /checkout", client: "checkout.PlaceOrder", median: 25 * time.Millisecond, sigma: 0.35, errProb: 0.0005,
		children: []*call{payment}, errorLog: "cart lookup timed out after {n}ms"}
	recommendation := &call{service: "recommendation", op: "GET /recommendations", client: "recommendation.List", median: 18 * time.Millisecond, sigma: 0.35, errProb: 0.0005,
		children: []*call{catalog()}, errorLog: "recommendation model unavailable"}
	ad := &call{service: "ad", op: "GET /ads", client: "ad.GetAds", median: 12 * time.Millisecond, sigma: 0.3, errProb: 0.0005,
		errorLog: "ad index refresh failed"}
	fe := func(op string, median time.Duration, child *call) *call {
		return &call{service: "frontend", op: op, median: median, sigma: 0.3, errProb: 0.0002, children: []*call{child},
			errorLog: "upstream request failed for " + op}
	}
	return []entry{
		{root: fe("GET /api/products/{id}", 4*time.Millisecond, catalog()), rps: 8},
		{root: fe("GET /api/recommendations", 4*time.Millisecond, recommendation), rps: 4},
		{root: fe("GET /api/ads", 3*time.Millisecond, ad), rps: 6},
		{root: fe("POST /api/checkout", 6*time.Millisecond, checkout), rps: 1.5},
	}
}

// Services that have resource metrics, with their baseline CPU (cores) and memory (bytes).
var resources = map[string]struct{ cpu, mem float64 }{
	"frontend":        {0.20, 180 << 20},
	"product-catalog": {0.12, 120 << 20},
	"recommendation":  {0.10, 300 << 20},
	"ad":              {0.08, 220 << 20},
	"checkout":        {0.06, 140 << 20},
	"payment":         {0.05, 90 << 20},
	"accounting":      {0.04, 110 << 20},
}

// Histogram bucket bounds in seconds (OpenTelemetry defaults, converted).
var buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}
