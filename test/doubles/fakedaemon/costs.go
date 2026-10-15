package fakedaemon

import (
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// What a namespace costs, as much of it as a program reading it needs: what
// it holds now — its running sandboxes, its volumes — priced at the rates the
// fake is given, and no history, which a fake that never meters has none of.

// WithCostRates are what an hour of each thing costs, in USD: what
// GET /v1/namespaces/{namespace}/costs prices what is held now at. Without
// it, everything costs nothing.
func WithCostRates(rates genv1.CostRates) Option { return func(d *Daemon) { d.costRates = rates } }

// costWindows is how far back each window goes, and in what steps.
var costWindows = map[genv1.CostWindow]struct {
	back time.Duration
	step time.Duration
}{
	genv1.CostWindow24h: {24 * time.Hour, time.Hour},
	genv1.CostWindow7d:  {7 * 24 * time.Hour, 24 * time.Hour},
	genv1.CostWindow30d: {30 * 24 * time.Hour, 24 * time.Hour},
	genv1.CostWindow90d: {90 * 24 * time.Hour, 24 * time.Hour},
}

const gib = float64(1 << 30)

func (d *Daemon) getNamespaceCosts(w http.ResponseWriter, r *http.Request) {
	window := genv1.CostWindow7d
	if asked := r.URL.Query().Get("window"); asked != "" {
		window = genv1.CostWindow(asked)
	}
	span, ok := costWindows[window]
	if !ok {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "window is one of 24h, 7d, 30d, 90d")
		return
	}
	namespace := r.PathValue("namespace")
	// Narrowed to one workload, as the daemon narrows it: the sandboxes
	// carrying every label asked for, and the volumes named.
	wanted := map[string]string{}
	for _, pair := range r.URL.Query()["label"] {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "a label filter is written `key=value`")
			return
		}
		wanted[key] = value
	}
	volumes := r.URL.Query()["volume"]
	selected := len(wanted) > 0 || len(volumes) > 0
	d.mu.Lock()
	defer d.mu.Unlock()
	rates := d.costRates
	if rates.Currency == "" {
		rates.Currency = "USD"
	}
	to := d.now().UTC()
	from := to.Add(-span.back).Truncate(span.step)
	step := genv1.CostStepDay
	if span.step == time.Hour {
		step = genv1.CostStepHour
	}
	out := genv1.CostReport{Currency: rates.Currency, Rates: rates, Window: window, From: from, To: to, Step: step,
		Series: []genv1.CostBucket{}, Resources: []genv1.CostResource{}, Namespaces: []genv1.NamespaceCost{}}
	for at := from; at.Before(to); at = at.Add(span.step) {
		out.Series = append(out.Series, genv1.CostBucket{At: at})
	}
	for _, s := range d.sandboxes {
		if valueOf(s.record.Namespace) != namespace || !isRunning(s.record.State) {
			continue
		}
		cpus := 0
		if s.record.Spec.Cpus != nil {
			cpus = *s.record.Spec.Cpus
		}
		var memory int64
		if s.record.Spec.Memory != nil {
			memory = sizeOf(*s.record.Spec.Memory)
		}
		perHour := float64(cpus)*rates.VcpuHour + float64(memory)/gib*rates.MemoryGiBHour
		labels := map[string]string{}
		if s.record.Labels != nil {
			labels = *s.record.Labels
		}
		if selected && (len(wanted) == 0 || !carries(labels, wanted)) {
			continue
		}
		out.Running.Sandboxes += perHour
		out.Resources = append(out.Resources, genv1.CostResource{Namespace: namespace, Kind: genv1.CostKindSandbox, Id: s.record.Id.String(),
			Labels: labels, Cpus: &cpus, MemoryBytes: &memory, Live: true, PerHour: perHour, FirstSeenAt: to, LastSeenAt: to})
	}
	for key, v := range d.volumes {
		if key.namespace != namespace || selected && !slices.Contains(volumes, key.name) {
			continue
		}
		bytes, name := v.bytes, key.name
		perHour := float64(bytes) / gib * rates.DiskGiBHour
		out.Running.Volumes += perHour
		out.Resources = append(out.Resources, genv1.CostResource{Namespace: namespace, Kind: genv1.CostKindVolume, Id: name, Name: &name,
			Labels: map[string]string{}, DiskBytes: &bytes, Live: true, PerHour: perHour, FirstSeenAt: to, LastSeenAt: to})
	}
	// The namespace's images, which are no workload's.
	for key, image := range d.images {
		if key.namespace != namespace || selected {
			continue
		}
		size, ref := *image.Size, image.Ref
		perHour := float64(size) / gib * rates.DiskGiBHour
		out.Running.Images += perHour
		out.Resources = append(out.Resources, genv1.CostResource{Namespace: namespace, Kind: genv1.CostKindImage, Id: key.name, Name: &ref,
			Labels: map[string]string{}, DiskBytes: &size, Live: true, PerHour: perHour, FirstSeenAt: to, LastSeenAt: to})
	}
	sort.Slice(out.Resources, func(i, j int) bool {
		if out.Resources[i].PerHour != out.Resources[j].PerHour {
			return out.Resources[i].PerHour > out.Resources[j].PerHour
		}
		return out.Resources[i].Id < out.Resources[j].Id
	})
	out.Running.Total = out.Running.Sandboxes + out.Running.Volumes + out.Running.Images
	out.Namespaces = append(out.Namespaces, genv1.NamespaceCost{Namespace: namespace, PerHour: out.Running.Total})
	reply(w, http.StatusOK, out)
}
