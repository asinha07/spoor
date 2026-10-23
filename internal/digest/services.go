package digest

import (
	"context"
	"sort"

	"github.com/asinha07/spoor/internal/window"
)

// ServiceInfo names one service and the signals that have data for it.
type ServiceInfo struct {
	Name    string   `json:"name"`
	Signals []string `json:"signals"`
}

// ServiceList is the list_services response.
type ServiceList struct {
	Window   string            `json:"window"`
	Services []ServiceInfo     `json:"services"`
	Coverage map[string]string `json:"coverage"`
}

// ListServices returns every service seen in any backend during the window.
func (e *Engine) ListServices(ctx context.Context, since, until string) (ServiceList, error) {
	spec, err := window.Parse(e.now(), since, until, "", 2)
	if err != nil {
		return ServiceList{}, err
	}
	return e.listIn(ctx, spec.Window), nil
}

// listIn lists services seen anywhere in r.
func (e *Engine) listIn(ctx context.Context, r window.Range) ServiceList {
	found := map[string]map[string]bool{}
	add := func(sig string, names []string) {
		for _, n := range names {
			if n == "" {
				continue
			}
			if found[n] == nil {
				found[n] = map[string]bool{}
			}
			found[n][sig] = true
		}
	}
	cov := map[string]string{}
	note := func(sig string, err error) {
		if err != nil {
			cov[sig] = "error: " + truncate(err.Error(), 160)
		} else {
			cov[sig] = "ok"
		}
	}
	if e.metrics != nil {
		names, err := e.metrics.LabelValues(ctx, e.cfg.Metrics.ServiceLabel, r.Start, r.End)
		note(SignalMetrics, err)
		add(SignalMetrics, names)
	} else {
		cov[SignalMetrics] = "not configured"
	}
	if e.logs != nil {
		names, err := e.logs.Services(ctx, r)
		note(SignalLogs, err)
		add(SignalLogs, names)
	} else {
		cov[SignalLogs] = "not configured"
	}
	if e.traces != nil {
		names, err := e.traces.Services(ctx, r)
		note(SignalTraces, err)
		add(SignalTraces, names)
	} else {
		cov[SignalTraces] = "not configured"
	}
	out := ServiceList{Window: rangeString(r), Coverage: cov, Services: []ServiceInfo{}}
	for name, sigs := range found {
		info := ServiceInfo{Name: name}
		for _, s := range []string{SignalMetrics, SignalLogs, SignalTraces} {
			if sigs[s] {
				info.Signals = append(info.Signals, s)
			}
		}
		out.Services = append(out.Services, info)
	}
	sort.Slice(out.Services, func(i, j int) bool { return out.Services[i].Name < out.Services[j].Name })
	return out
}
