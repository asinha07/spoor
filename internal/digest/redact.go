package digest

import (
	"regexp"
	"strings"

	"github.com/asinha07/spoor/internal/config"
)

// redactions scrub likely secrets and personal data from log examples before
// they leave Spoor. Order matters: specific patterns run before generic ones.
var redactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "<JWT>"},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "$1 <TOKEN>"},
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key)(["']?\s*[:=]\s*["']?)[^\s"',;&]+`), "$1$2<REDACTED>"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "<EMAIL>"},
	// UUIDs before card numbers: a UUID's digit groups can look like a card.
	{regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), "<UUID>"},
	{regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`), "<CARD>"},
	{regexp.MustCompile(`\b[0-9a-fA-F]{24,}\b`), "<HEX>"},
	{regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), "<IP>"},
}

// Redact scrubs s and truncates it to max runes.
func Redact(s string, max int) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return truncate(s, max)
}

func truncate(s string, max int) string {
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// idToken finds candidate identifiers: word-like tokens of six or more
// characters. Only those mixing letters and digits are replaced.
var idToken = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_-]{5,}`)

// redactor scrubs every text field of a response: the built-in rules above,
// identifiers that mix letters and digits (order, cart, customer IDs), then
// the user's own rules. Service names are never treated as identifiers.
type redactor struct {
	rules []struct {
		re   *regexp.Regexp
		repl string
	}
}

func newRedactor(rules []config.Rule) *redactor {
	r := &redactor{}
	for _, rule := range rules {
		re, err := regexp.Compile(rule.Regex)
		if err != nil {
			continue // rejected by config validation
		}
		repl := rule.Replace
		if repl == "" {
			repl = "<REDACTED>"
		}
		r.rules = append(r.rules, struct {
			re   *regexp.Regexp
			repl string
		}{re, repl})
	}
	return r
}

func (r *redactor) text(s string, allow map[string]bool) string {
	if s == "" {
		return s
	}
	for _, x := range redactions {
		s = x.re.ReplaceAllString(s, x.repl)
	}
	s = idToken.ReplaceAllStringFunc(s, func(tok string) string {
		if allow[tok] || !looksLikeID(tok) {
			return tok
		}
		return "<ID>"
	})
	for _, x := range r.rules {
		s = x.re.ReplaceAllString(s, x.repl)
	}
	return s
}

// envelope redacts every text field an agent will read. Trace IDs and
// evidence links are kept: they are needed to verify a finding and carry no
// log content.
func (r *redactor) envelope(env *Envelope, services []string) {
	allow := map[string]bool{}
	for _, s := range services {
		allow[s] = true
		for _, part := range strings.FieldsFunc(s, func(c rune) bool { return c == '.' || c == '/' }) {
			allow[part] = true
		}
	}
	for i := range env.Findings {
		f := &env.Findings[i]
		f.Title, f.Example = r.text(f.Title, allow), r.text(f.Example, allow)
		f.Before, f.After = r.text(f.Before, allow), r.text(f.After, allow)
	}
	for i := range env.SteadyErrors {
		env.SteadyErrors[i].Example = r.text(env.SteadyErrors[i].Example, allow)
	}
	for k, v := range env.Coverage {
		env.Coverage[k] = r.text(v, allow)
	}
	env.Summary = r.text(env.Summary, allow)
}

// numberWithUnit matches durations and sizes such as 1500ms or 512MiB.
var numberWithUnit = regexp.MustCompile(`(?i)^[0-9]+(ns|us|µs|ms|s|m|h|d|b|kb|mb|gb|tb|kib|mib|gib|tib)$`)

// looksLikeID reports whether tok has a segment (split on - and _) that
// mixes digits with at least two letters, the shape of generated
// identifiers (ord_7f3k2m9q, OLJCESPC7Z). Timestamps (2026-10-15T17), numbers
// with units (1500ms) and phrases like 200-per-service are not IDs.
func looksLikeID(tok string) bool {
	for _, seg := range strings.FieldsFunc(tok, func(c rune) bool { return c == '-' || c == '_' }) {
		letters, digits := 0, 0
		for _, c := range seg {
			switch {
			case c >= '0' && c <= '9':
				digits++
			case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
				letters++
			}
		}
		if digits > 0 && letters >= 2 && !numberWithUnit.MatchString(seg) {
			return true
		}
	}
	return false
}
