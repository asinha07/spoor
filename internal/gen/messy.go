package gen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"time"
)

// MessyLogs is a logs-only scenario shaped like real production logs rather
// than tidy OpenTelemetry output. It checks that Spoor stays honest when:
//
//   - the service is in a "container" field, not service.name, and a few
//     lines carry no service field at all;
//
//   - services disagree on the level field (level, log.level), and one writes
//     its level only inside the message text ("... [ERROR] ...");
//
//   - error messages embed generated IDs (ord_7f3k2m9q, product codes) and
//     stack traces, so the exact text of one error varies line by line;
//
//   - one error family gets new variants (new IDs, new quoted names) in the
//     window without being a new problem.
//
//   - some errors are pretty-printed JSON, and some carry a Java log prefix
//     ([ERROR] [thread] [correlationId:...] [Class.java:N]).
//
// The truth: orders starts a genuinely new error, renderer starts a new JSON
// error, and billing's errors rise about 60% on flat traffic. search's
// errors rise with its traffic (+60% both), which is not a finding, and its
// four lines of a new error are too few to matter. Nothing else changed.
type MessyTruth struct {
	GroundTruth
	// Expected findings, as service -> finding kind.
	Expect map[string]string `json:"expect"`
	// Services that must not have findings.
	Quiet []string `json:"quiet"`
}

// MessyServiceField and the level fields used by the scenario, for configs.
const (
	MessyServiceField = "container"
)

type messySvc struct {
	name      string
	levelKey  string // "" = level only in the message text
	rate      float64
	errRate   float64
	errors    []string
	windowErr func(r *rand.Rand) string // extra error emitted only in the window
	variant   func(r *rand.Rand) string // in the window, replaces half of the usual errors
	fewNew    string                    // in the window, exactly 4 lines of a new error (too few to report)
	windowMul float64                   // error multiplier in the window
	trafMul   float64                   // traffic multiplier in the window
}

func randAlnum(r *rand.Rand, n int, alphabet string) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(b)
}

func sessionID(r *rand.Rand) string {
	return "ses_" + randAlnum(r, 8, "abcdefghijkmnpqrstuvwxyz0123456789")
}
func productCode(r *rand.Rand) string {
	return randAlnum(r, 8, "ABCDEFGHJKLMNPQRSTUVWXYZ") + fmt.Sprint(r.Intn(10)) + randAlnum(r, 1, "ABCDEFGHJKLMNPQRSTUVWXYZ")
}

var routes = []string{"home", "docs", "pricing", "account", "search", "status"}

// GenerateMessy writes the scenario's logs, ending at end, with history
// baseline windows of length win before the fault window, and returns the
// ground truth.
func GenerateMessy(ctx context.Context, sink Sink, end time.Time, win time.Duration, history int, seed int64) (MessyTruth, error) {
	r := rand.New(rand.NewSource(seed))
	start := end.Add(-time.Duration(history+1) * win)
	faultStart := end.Add(-win)
	stack := "\n\tat com.example.cart.CartService.load(CartService.java:%d)\n\tat com.example.web.Handler.handle(Handler.java:%d)"
	services := []messySvc{
		{name: "api-gateway", levelKey: "level", rate: 30, errRate: 0.02, errors: []string{
			`no route matched for "%L"`,
		}, variant: func(r *rand.Rand) string {
			// Same family, new quoted names and IDs: not a new problem.
			return fmt.Sprintf(`no route matched for "%s-%s"`, routes[r.Intn(len(routes))], randAlnum(r, 4, "xyz"))
		}, windowMul: 1, trafMul: 1},
		{name: "orders", levelKey: "level", rate: 20, errRate: 0.02, errors: []string{
			"session %C not found for account %C",
		}, windowErr: func(r *rand.Rand) string {
			return fmt.Sprintf("payment provider timeout after %dms for order %s", 3000+r.Intn(2000), sessionID(r))
		}, windowMul: 1, trafMul: 1},
		{name: "billing", levelKey: "log.level", rate: 20, errRate: 0.05, errors: []string{
			"invoice %C could not be generated: tax service returned %N",
		}, windowMul: 1.6, trafMul: 1},
		{name: "search", levelKey: "level", rate: 25, errRate: 0.03, errors: []string{
			"query for %P timed out",
		}, windowMul: 1.6, trafMul: 1.6},
		{name: "catalog", levelKey: "", rate: 20, errRate: 0.03, errors: []string{
			"failed to price product %P" + stack,
		}, windowMul: 1, trafMul: 1},
		// Pretty-printed JSON errors: the first line is "{". A new JSON
		// error starts in the window and must not hide among the old ones.
		{name: "renderer", levelKey: "level", rate: 20, errRate: 0.04, errors: []string{
			"{\n  \"message\": \"No matching template found\",\n  \"code\": \"TEMPLATE_404\",\n  \"requestId\": \"%C\"\n}",
			"{\n  \"message\": \"Template not found for kind: invoice\",\n  \"requestId\": \"%C\"\n}",
		}, windowErr: func(r *rand.Rand) string {
			return fmt.Sprintf("{\n  \"message\": \"filters accept at most %d comma-separated values\",\n  \"requestId\": \"%s\"\n}", 10+r.Intn(3), sessionID(r))
		}, windowMul: 1, trafMul: 1},
		// Java log prefix: thread, correlation ID and line number vary.
		{name: "accounts", levelKey: "level", rate: 20, errRate: 0.04, errors: []string{
			"[ERROR] [http-nio-8080-exec-%d] [correlationId:%C-%C] [ProfileController.java:88] profile fetch failed for account %C",
			"[ERROR] [http-nio-8080-exec-%d] [correlationId:%C-%C] [AddressBookService.java:214] address book is null for session %C",
		}, windowMul: 1, trafMul: 1},
	}
	services[3].fewNew = "Password reset link expired" // search: busy, so 4 new lines are noise
	fill := func(tmpl string) string {
		var out bytes.Buffer
		for i := 0; i < len(tmpl); i++ {
			if tmpl[i] == '%' && i+1 < len(tmpl) {
				switch tmpl[i+1] {
				case 'C':
					out.WriteString(sessionID(r))
					i++
					continue
				case 'P':
					out.WriteString(productCode(r))
					i++
					continue
				case 'N':
					fmt.Fprint(&out, 500+r.Intn(4))
					i++
					continue
				case 'L':
					out.WriteString(routes[r.Intn(3)])
					i++
					continue
				case 'd':
					fmt.Fprint(&out, 10+r.Intn(400))
					i++
					continue
				}
			}
			out.WriteByte(tmpl[i])
		}
		return out.String()
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	emit := func(t time.Time, s messySvc, isErr bool, msg string) error {
		level := "INFO"
		if isErr {
			level = "ERROR"
		}
		doc := map[string]any{"_time": t.UTC().Format(time.RFC3339Nano), MessyServiceField: s.name}
		if s.levelKey == "" {
			msg = t.UTC().Format("2006-01-02T15:04:05.000Z") + " [" + level + "] " + msg
		} else {
			doc[s.levelKey] = level
		}
		doc["_msg"] = msg
		return enc.Encode(doc)
	}
	for t := start; t.Before(end); t = t.Add(time.Second) {
		inWin := !t.Before(faultStart)
		for _, s := range services {
			rate, errRate := s.rate, s.errRate
			if inWin {
				rate *= s.trafMul
				errRate *= s.windowMul / s.trafMul // errors scale with traffic by default
			}
			n := poissonInt(r, rate/10)
			for i := 0; i < n; i++ {
				ts := t.Add(time.Duration(r.Int63n(int64(time.Second))))
				if r.Float64() < errRate {
					msg := fill(s.errors[r.Intn(len(s.errors))])
					if inWin && s.variant != nil && r.Float64() < 0.5 {
						msg = s.variant(r)
					}
					if err := emit(ts, s, true, msg); err != nil {
						return MessyTruth{}, err
					}
					continue
				}
				if err := emit(ts, s, false, "request handled in "+fmt.Sprint(5+r.Intn(200))+"ms"); err != nil {
					return MessyTruth{}, err
				}
			}
			if s.fewNew != "" && !t.Before(faultStart) && t.Sub(faultStart) < 4*time.Second {
				if err := emit(t, s, true, s.fewNew); err != nil {
					return MessyTruth{}, err
				}
			}
			if inWin && s.windowErr != nil && r.Float64() < 0.1 {
				if err := emit(t, s, true, s.windowErr(r)); err != nil {
					return MessyTruth{}, err
				}
			}
		}
		// A few lines from a sidecar with no service field.
		if r.Float64() < 0.05 {
			if err := enc.Encode(map[string]any{"_time": t.UTC().Format(time.RFC3339Nano), "_msg": "sidecar heartbeat"}); err != nil {
				return MessyTruth{}, err
			}
		}
		if buf.Len() > 4<<20 {
			if err := sink.Logs(ctx, buf.Bytes()); err != nil {
				return MessyTruth{}, err
			}
			buf.Reset()
		}
	}
	if buf.Len() > 0 {
		if err := sink.Logs(ctx, buf.Bytes()); err != nil {
			return MessyTruth{}, err
		}
	}
	return MessyTruth{
		GroundTruth: GroundTruth{
			Scenario:    "messy-logs",
			Description: "Production-shaped logs: mixed level fields, text-only levels, embedded IDs and stack traces. orders starts a new error; billing's errors rise on flat traffic.",
			Since:       faultStart.UTC().Format(time.RFC3339),
			Until:       end.UTC().Format(time.RFC3339),
		},
		Expect: map[string]string{"orders": "new_log_pattern", "billing": "error_volume_up", "renderer": "new_log_pattern"},
		Quiet:  []string{"api-gateway", "search", "catalog", "accounts"},
	}, nil
}

func poissonInt(r *rand.Rand, lambda float64) int {
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}
