package fakedaemon

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

func TestTheFakePricesWhatANamespaceHoldsNow(t *testing.T) {
	now := time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC)
	d := New(t, WithKey("k"), WithClock(func() time.Time { return now }),
		WithCostRates(genv1.CostRates{Currency: "EUR", VcpuHour: 0.04, MemoryGiBHour: 0.01, DiskGiBHour: 0.0001}))
	client := typed(t, d, "k")
	ctx := context.Background()
	spec := genv1.SandboxSpec{Image: "alpine", Cpus: new(2), Memory: new("4Gi"), Disk: &genv1.DiskSpec{Volume: new("chat")},
		Labels: &map[string]string{"app": "a"}}
	if created, err := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "k1"}, spec); err != nil || created.JSON202 == nil {
		t.Fatalf("create = %v %s", err, created.Body)
	}
	d.PutVolume("data", 10<<30)

	window := genv1.CostWindow24h
	got, err := client.GetNamespaceCostsWithResponse(ctx, DefaultNamespace, &genv1.GetNamespaceCostsParams{Window: &window})
	if err != nil || got.JSON200 == nil {
		t.Fatalf("costs = %v %s", err, got.Body)
	}
	report := got.JSON200
	if report.Currency != "EUR" || report.Step != genv1.CostStepHour || len(report.Series) != 25 || report.Total.Total != 0 {
		t.Fatalf("the report is %+v", report)
	}
	if math.Abs(report.Running.Sandboxes-(2*0.04+4*0.01)) > 1e-9 || math.Abs(report.Running.Volumes-10*0.0001) > 1e-9 {
		t.Fatalf("running %+v", report.Running)
	}
	if len(report.Resources) != 3 || report.Resources[0].Kind != genv1.CostKindSandbox || !report.Resources[0].Live {
		t.Fatalf("the resources %+v", report.Resources)
	}
	week, err := client.GetNamespaceCostsWithResponse(ctx, DefaultNamespace, nil)
	if err != nil || week.JSON200 == nil || week.JSON200.Window != genv1.CostWindow7d || len(week.JSON200.Series) != 8 {
		t.Fatalf("a week = %v %s", err, week.Body)
	}
	// One workload's: the sandboxes carrying its label, and the volumes named.
	for name, c := range map[string]struct {
		label, volume []string
		want          []string
	}{
		"its label and its disk": {[]string{"app=a"}, []string{"data"}, []string{"sandbox", "data"}},
		"another's label":        {[]string{"app=b"}, []string{"data"}, []string{"data"}},
		"its label alone":        {[]string{"app=a"}, nil, []string{"sandbox"}},
	} {
		params := &genv1.GetNamespaceCostsParams{Window: &window, Label: &c.label}
		if c.volume != nil {
			params.Volume = &c.volume
		}
		got, err := client.GetNamespaceCostsWithResponse(ctx, DefaultNamespace, params)
		if err != nil || got.JSON200 == nil {
			t.Fatalf("%s = %v %s", name, err, got.Body)
		}
		var kinds []string
		for _, x := range got.JSON200.Resources {
			if x.Kind == genv1.CostKindSandbox {
				kinds = append(kinds, "sandbox")
			} else {
				kinds = append(kinds, x.Id)
			}
		}
		if !slices.Equal(kinds, c.want) {
			t.Errorf("%s: %v, want %v", name, kinds, c.want)
		}
	}
	if bad, _ := client.GetNamespaceCostsWithResponse(ctx, DefaultNamespace, &genv1.GetNamespaceCostsParams{Label: &[]string{"app"}}); bad.StatusCode() != 400 {
		t.Fatalf("a label that is not one = %d", bad.StatusCode())
	}
	wrong := genv1.CostWindow("1y")
	if bad, _ := client.GetNamespaceCostsWithResponse(ctx, DefaultNamespace, &genv1.GetNamespaceCostsParams{Window: &wrong}); bad.StatusCode() != 400 {
		t.Fatalf("a window that is not one = %d", bad.StatusCode())
	}
}
