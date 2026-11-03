package tilecache

import (
	"context"
	"math"
	"testing"
)

func putUsageTile(t *testing.T, manager *Manager, workspaceID, resourceID string, column int, content []byte) {
	t.Helper()
	identity := Identity{
		WorkspaceID: workspaceID, WorkspaceRevision: 1, ResourceID: resourceID,
		ResourceKind: "feature", Generation: 1, TileType: "map", MatrixSet: "WebMercatorQuad",
		Zoom: 2, Column: column, Row: 1, StyleDigest: "default", Format: "image/png",
	}
	stored, err := manager.Put(context.Background(), identity, content, Policy{})
	if err != nil || !stored {
		t.Fatalf("Put(%s/%s col=%d) = stored %v, err %v", workspaceID, resourceID, column, stored, err)
	}
}

func TestManagerResourceUsageSortsAndTruncates(t *testing.T) {
	manager := newTestManager(t, t.TempDir(), 1<<30)
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()

	putUsageTile(t, manager, "ws", "alpha", 1, []byte("0123456789")) // 10 bytes
	putUsageTile(t, manager, "ws", "beta", 1, []byte("0123456789"))  // 20 bytes
	putUsageTile(t, manager, "ws", "beta", 2, []byte("0123456789"))
	putUsageTile(t, manager, "ws", "gamma", 1, []byte("01234567890123456789")) // 20 bytes
	putUsageTile(t, manager, "ws", "delta", 1, []byte("12345"))                // 5 bytes
	putUsageTile(t, manager, "ws", "empty", 1, []byte("ephemeral"))
	if _, err := manager.DeleteIdentity(ctx, Identity{
		WorkspaceID: "ws", WorkspaceRevision: 1, ResourceID: "empty",
		ResourceKind: "feature", Generation: 1, TileType: "map", MatrixSet: "WebMercatorQuad",
		Zoom: 2, Column: 1, Row: 1, StyleDigest: "default", Format: "image/png",
	}); err != nil {
		t.Fatal(err)
	}
	// A different workspace must never leak into the aggregation; the colon in
	// the scope prefix also guards against prefix collisions.
	putUsageTile(t, manager, "wsx", "foreign", 1, []byte("foreign-tile"))

	// The zeroed cache_usage row remains but carries no entries.
	if lingering, err := manager.index.usage(ctx, "resource", "ws:empty"); err != nil || lingering.EntryCount != 0 || lingering.SizeBytes != 0 {
		t.Fatalf("lingering zero usage = %+v, %v", lingering, err)
	}

	report, err := manager.ResourceUsage(ctx, "ws", 0, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalResources != 4 || report.ReturnedResources != 4 || report.Limit != 100 {
		t.Fatalf("counts = total %d returned %d limit %d", report.TotalResources, report.ReturnedResources, report.Limit)
	}
	got := make([]string, len(report.Resources))
	for idx, item := range report.Resources {
		got[idx] = item.ResourceID
	}
	want := []string{"beta", "gamma", "alpha", "delta"}
	if len(got) != len(want) {
		t.Fatalf("resources = %v, want %v", got, want)
	}
	for idx := range want {
		if got[idx] != want[idx] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	byResource := map[string]ResourceUsage{}
	for _, item := range report.Resources {
		byResource[item.ResourceID] = item
	}
	if byResource["beta"].EntryCount != 2 || byResource["beta"].SizeBytes != 20 {
		t.Fatalf("beta usage = %+v", byResource["beta"])
	}
	if byResource["gamma"].EntryCount != 1 || byResource["gamma"].SizeBytes != 20 {
		t.Fatalf("gamma usage = %+v", byResource["gamma"])
	}
	if _, ok := byResource["empty"]; ok {
		t.Fatal("zero-entry resource must not be reported")
	}
	// No quotas configured: everything inherits the global quota.
	for _, item := range report.Resources {
		if item.QuotaBytes != 1<<30 || item.RemainingBytes != 1<<30-item.SizeBytes {
			t.Fatalf("global quota inheritance failed for %s: %+v", item.ResourceID, item.Usage)
		}
	}

	for _, limit := range []int{1, 2} {
		truncated, err := manager.ResourceUsage(ctx, "ws", 0, nil, limit)
		if err != nil {
			t.Fatal(err)
		}
		if truncated.TotalResources != 4 || truncated.ReturnedResources != limit {
			t.Fatalf("limit %d counts = total %d returned %d", limit, truncated.TotalResources, truncated.ReturnedResources)
		}
		if truncated.Resources[0].ResourceID != "beta" {
			t.Fatalf("limit %d starts with %s, want beta", limit, truncated.Resources[0].ResourceID)
		}
	}

	empty, err := manager.ResourceUsage(ctx, "vacant", 0, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if empty.TotalResources != 0 || empty.ReturnedResources != 0 || len(empty.Resources) != 0 {
		t.Fatalf("vacant workspace report = %+v", empty)
	}
}

func TestManagerResourceUsageQuotaInheritance(t *testing.T) {
	const globalQuota int64 = 1000
	manager := newTestManager(t, t.TempDir(), globalQuota)
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	putUsageTile(t, manager, "ws", "layer-a", 1, []byte("0123456789012345678901234")) // 25 bytes
	putUsageTile(t, manager, "ws", "layer-b", 1, []byte("0123456789012345678901234"))

	resourceQuota, err := manager.ResourceUsage(ctx, "ws", 100, map[string]int64{"layer-a": 40}, 100)
	if err != nil {
		t.Fatal(err)
	}
	a := resourceQuota.Resources[0]
	if a.ResourceID != "layer-a" || a.QuotaBytes != 40 || a.RemainingBytes != 15 {
		t.Fatalf("resource quota usage = %+v", a)
	}
	if math.Abs(a.Utilization-0.625) > 1e-9 {
		t.Fatalf("utilization = %v, want 0.625", a.Utilization)
	}

	workspaceQuota, err := manager.ResourceUsage(ctx, "ws", 100, map[string]int64{"layer-a": 40}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var b ResourceUsage
	for _, item := range workspaceQuota.Resources {
		if item.ResourceID == "layer-b" {
			b = item
		}
	}
	if b.QuotaBytes != 100 || b.RemainingBytes != 75 {
		t.Fatalf("workspace quota inheritance = %+v", b)
	}

	global, err := manager.ResourceUsage(ctx, "ws", 0, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range global.Resources {
		if item.QuotaBytes != globalQuota || item.RemainingBytes != globalQuota-item.SizeBytes {
			t.Fatalf("global quota inheritance for %s = %+v", item.ResourceID, item.Usage)
		}
	}
}

func TestManagerResourceUsageValidatesLimits(t *testing.T) {
	manager := newTestManager(t, t.TempDir(), 1024)
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	for _, limit := range []int{0, -1, MaxResourceUsageLimit + 1} {
		if _, err := manager.ResourceUsage(ctx, "ws", 0, nil, limit); err == nil {
			t.Fatalf("limit %d should be rejected", limit)
		}
	}
	if _, err := manager.ResourceUsage(ctx, "", 0, nil, 100); err == nil {
		t.Fatal("empty workspace id should be rejected")
	}
	if _, err := manager.ResourceUsage(ctx, "ws", 0, nil, MaxResourceUsageLimit); err != nil {
		t.Fatalf("max limit rejected: %v", err)
	}
}

func TestManagerResourceUsageIsReadOnly(t *testing.T) {
	manager := newTestManager(t, t.TempDir(), 1<<20)
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	putUsageTile(t, manager, "ws", "layer", 1, []byte("tile payload"))
	before, err := manager.Stats(ctx, "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.ResourceUsage(ctx, "ws", 0, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.ResourceUsage(ctx, "ws", 0, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Resources) != len(second.Resources) || first.Resources[0] != second.Resources[0] {
		t.Fatalf("repeated reads differ: %+v vs %+v", first.Resources, second.Resources)
	}
	after, err := manager.Stats(ctx, "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if before.Global != after.Global {
		t.Fatalf("usage changed after read-only aggregation: before=%+v after=%+v", before.Global, after.Global)
	}
	identity := Identity{WorkspaceID: "ws", WorkspaceRevision: 1, ResourceID: "layer", ResourceKind: "feature",
		Generation: 1, TileType: "map", MatrixSet: "WebMercatorQuad", Zoom: 2, Column: 1, Row: 1,
		StyleDigest: "default", Format: "image/png"}
	data, _, found, err := manager.Get(ctx, identity)
	if err != nil || !found || string(data) != "tile payload" {
		t.Fatalf("cached tile unavailable after aggregation: found=%v err=%v", found, err)
	}
}
