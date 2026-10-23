package digest

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/asinha07/spoor/internal/config"
)

// normalizer turns a raw error message into its pattern in Go, for backends
// that cannot rewrite text in their query language (New Relic). It follows
// the same steps as the LogsQL and LogQL pipelines: JSON message and code
// fields, first line only, whitespace, leading timestamp, user rules, then
// UUIDs, IPs, letter-digit IDs and numbers.
type normalizer struct {
	messageFields, codeFields []string
	steps                     []normStep
}

type normStep struct {
	re   *regexp.Regexp
	repl string
}

func newNormalizer(c config.LogsConfig) *normalizer {
	n := &normalizer{messageFields: c.MessageFields, codeFields: c.CodeFields}
	add := func(re, repl string) {
		if r, err := regexp.Compile(re); err == nil { // user rules are validated at load
			n.steps = append(n.steps, normStep{r, repl})
		}
	}
	for _, s := range lokiSteps {
		add(s[0], s[1])
	}
	for _, r := range c.Normalize {
		add(r.Regex, r.Replace)
	}
	add(uuidPattern, "<UUID>")
	add(ipPattern, "<IP>")
	if c.IDNormalization() {
		add(idPattern, "<ID>")
	}
	add(hexPattern, "<N>")
	add(numPattern, "<N>")
	return n
}

func (n *normalizer) pattern(raw string) string {
	msg, code := raw, ""
	if t := strings.TrimSpace(raw); strings.HasPrefix(t, "{") && (len(n.messageFields) > 0 || len(n.codeFields) > 0) {
		var obj map[string]any
		if json.Unmarshal([]byte(t), &obj) == nil {
			if m := firstField(obj, n.messageFields); m != "" {
				msg = m
			}
			code = firstField(obj, n.codeFields)
		}
	}
	for _, s := range n.steps {
		msg = s.re.ReplaceAllString(msg, s.repl)
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	if code != "" {
		return code + " " + msg
	}
	return msg
}

// firstField returns the first of fields found in obj as text; dotted names
// reach into nested objects (error.message).
func firstField(obj map[string]any, fields []string) string {
	for _, f := range fields {
		var v any = obj
		for _, part := range strings.Split(f, ".") {
			m, ok := v.(map[string]any)
			if !ok {
				v = nil
				break
			}
			v = m[part]
		}
		if v == nil {
			if direct, ok := obj[f]; ok {
				v = direct
			}
		}
		switch x := v.(type) {
		case string:
			if x != "" {
				return x
			}
		case float64:
			return jsonNumber(x)
		}
	}
	return ""
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}
