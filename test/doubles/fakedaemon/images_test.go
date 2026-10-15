package fakedaemon

import (
	"context"
	"testing"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

func TestTheFakesImagesAreReadCountedPricedAndDeletedUnlessBootedFrom(t *testing.T) {
	d := New(t, WithKey("k"), WithCostRates(genv1.CostRates{Currency: "EUR", DiskGiBHour: 0.001}))
	client := typed(t, d, "k")
	ctx := context.Background()
	d.PutImage("img_a", "ghcr.io/acme/a:1", 2<<30)
	d.PutImage("img_b", "ghcr.io/acme/b:1", 1<<30)
	spec := genv1.SandboxSpec{Image: "ghcr.io/acme/a:1"}
	if created, err := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "k1"}, spec); err != nil || created.JSON202 == nil {
		t.Fatalf("create = %v %s", err, created.Body)
	}

	list, err := client.ListImagesWithResponse(ctx, DefaultNamespace, nil)
	if err != nil || list.JSON200 == nil || len(list.JSON200.Items) != 2 || *list.JSON200.Items[0].Sandboxes != 1 {
		t.Fatalf("list = %v %s", err, list.Body)
	}
	one, err := client.GetImageWithResponse(ctx, DefaultNamespace, "img_b")
	if err != nil || one.JSON200 == nil || one.JSON200.Ref != "ghcr.io/acme/b:1" || *one.JSON200.Sandboxes != 0 {
		t.Fatalf("get = %v %s", err, one.Body)
	}
	counts, err := client.GetCountsWithResponse(ctx, DefaultNamespace)
	if err != nil || counts.JSON200 == nil || *counts.JSON200.Images != 2 {
		t.Fatalf("counts = %v %s", err, counts.Body)
	}
	costs, err := client.GetNamespaceCostsWithResponse(ctx, DefaultNamespace, nil)
	if err != nil || costs.JSON200 == nil || costs.JSON200.Running.Images != 3*0.001 {
		t.Fatalf("costs = %v %s", err, costs.Body)
	}

	if booted, _ := client.DeleteImageWithResponse(ctx, DefaultNamespace, "img_a"); booted.StatusCode() != 409 {
		t.Fatalf("deleting one booted from = %d", booted.StatusCode())
	}
	for range 2 {
		if gone, err := client.DeleteImageWithResponse(ctx, DefaultNamespace, "img_b"); err != nil || gone.StatusCode() != 204 {
			t.Fatalf("delete = %v %d", err, gone.StatusCode())
		}
	}
	if missing, _ := client.GetImageWithResponse(ctx, DefaultNamespace, "img_b"); missing.StatusCode() != 404 {
		t.Fatalf("a deleted image = %d", missing.StatusCode())
	}
}
