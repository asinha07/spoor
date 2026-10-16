package digest

import (
	"bytes"
	"encoding/json"
	"strings"
)

// MarshalCompact encodes v as compact JSON without HTML escaping, so "<N>" in
// log patterns stays three characters instead of becoming "\u003cN\u003e".
// This is the exact form agents receive and the form the budget measures.
func MarshalCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// EstimateTokens approximates the tokens an LLM spends on s (characters / 4,
// rounded up). Deliberately simple and deterministic.
func EstimateTokens(s []byte) int { return (len(s) + 3) / 4 }

// Fit trims env so its compact JSON fits within budget tokens. Findings are
// already ranked, so the lowest-scoring ones are dropped first; evidence links
// and examples are shed before whole findings when that is enough. Truncated
// counts the dropped findings.
func Fit(env Envelope, budget int) Envelope {
	if env.Findings == nil {
		env.Findings = []Finding{}
	}
	fits := func(e Envelope) bool {
		b, _ := MarshalCompact(e)
		return EstimateTokens(b) <= budget
	}
	if fits(env) {
		return env
	}
	// Pass 1: drop evidence links from the lower half, then all of them.
	slim := cloneFindings(env.Findings)
	for i := len(slim) / 2; i < len(slim); i++ {
		slim[i].EvidenceLink = ""
	}
	if e := withFindings(env, slim, 0); fits(e) {
		return e
	}
	for i := range slim {
		slim[i].EvidenceLink = ""
		slim[i].Example = ""
	}
	// Steady errors are context, not findings: they go before any finding does.
	if e := withFindings(env, slim, 0); fits(e) {
		return e
	}
	env.SteadyErrors = nil
	// Pass 2: drop findings from the bottom until it fits.
	for n := len(slim); n >= 0; n-- {
		e := withFindings(env, slim[:n], len(slim)-n)
		if fits(e) {
			return e
		}
	}
	// Pass 3: even the bare envelope is too big. Keep only each coverage
	// state's first word (ok, error, partial, no data), then shorten the summary.
	e := withFindings(env, []Finding{}, len(slim))
	short := make(map[string]string, len(e.Coverage))
	for k, v := range e.Coverage {
		if i := strings.IndexAny(v, ":("); i > 0 {
			v = strings.TrimSpace(v[:i])
		}
		short[k] = truncate(v, 24)
	}
	e.Coverage = short
	for len([]rune(e.Summary)) > 40 && !fits(e) {
		e.Summary = truncate(e.Summary, len([]rune(e.Summary))*3/4)
	}
	return e
}

func withFindings(env Envelope, fs []Finding, truncated int) Envelope {
	env.Findings = fs
	env.Truncated += truncated
	return env
}

func cloneFindings(fs []Finding) []Finding {
	out := make([]Finding, len(fs))
	copy(out, fs)
	return out
}
