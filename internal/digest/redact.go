package digest

import "regexp"

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
