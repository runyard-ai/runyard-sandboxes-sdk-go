package fakedaemon

import (
	"context"
	"math"
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
	spec := genv1.SandboxSpec{Image: "alpine", Cpus: new(2), Memory: new("4Gi"), Disk: &genv1.DiskSpec{Volume: new("chat")}}
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
	wrong := genv1.CostWindow("1y")
	if bad, _ := client.GetNamespaceCostsWithResponse(ctx, DefaultNamespace, &genv1.GetNamespaceCostsParams{Window: &wrong}); bad.StatusCode() != 400 {
		t.Fatalf("a window that is not one = %d", bad.StatusCode())
	}
}
