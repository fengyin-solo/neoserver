package mgmt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tobilg/neoserver/internal/store"
	"github.com/tobilg/neoserver/internal/tilecache"
)

type tileStatsResourceRow struct {
	ResourceID     string  `json:"resource_id"`
	SizeBytes      int64   `json:"size_bytes"`
	EntryCount     int64   `json:"entry_count"`
	QuotaBytes     int64   `json:"quota_bytes"`
	RemainingBytes int64   `json:"remaining_bytes"`
	Utilization    float64 `json:"utilization"`
}

type tileStatsGroupedResponse struct {
	Enabled           bool                   `json:"enabled"`
	Backend           string                 `json:"backend"`
	GroupBy           string                 `json:"group_by"`
	WorkspaceID       string                 `json:"workspace_id"`
	Resources         []tileStatsResourceRow `json:"resources"`
	TotalResources    int                    `json:"total_resources"`
	ReturnedResources int                    `json:"returned_resources"`
	Limit             int                    `json:"limit"`
}

func newTileCacheStatsHandler(t *testing.T, workspaceQuota int64) (*handler, *tilecache.Manager, string, map[string]string) {
	t.Helper()
	catalog := newMockStore()
	catalog.ogcTilesQuota = workspaceQuota
	h := newTestHandlerWithEnforcer(t, catalog, publicationTestFactory)
	ctx := context.Background()
	ws, err := h.registry.CreateWorkspace(ctx, store.CreateWorkspaceInput{Name: "tiles-ws"})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := h.registry.CreateService(ctx, store.CreateServiceInput{
		WorkspaceID: ws.ID, Name: "source", Type: store.ServiceTypePostGIS, Enabled: true,
		ConnectionInfo: json.RawMessage(`{"host":"localhost"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	publicIDs := []string{"roads", "buildings", "water"}
	resourceQuotas := map[string]int64{"roads": 500}
	resourceIDs := map[string]string{}
	for _, publicID := range publicIDs {
		layer, err := h.registry.CreateLayer(ctx, ws.ID, svc.ID, store.CreateLayerInput{
			ServiceID: svc.ID, PublicID: publicID, SourceLayer: "public." + publicID, Enabled: true,
			TileCacheQuotaBytes: resourceQuotas[publicID],
		})
		if err != nil {
			t.Fatal(err)
		}
		resourceIDs[publicID] = layer.ID
	}
	root := t.TempDir()
	backend, err := tilecache.NewFilesystemStore(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := tilecache.NewManager(ctx, tilecache.Config{
		DatabasePath: filepath.Join(root, "tile-cache.duckdb"), EncryptionKey: "abc123",
		MaxBytes: 1 << 30, AccessFlushInterval: time.Hour, MaintenanceInterval: time.Hour,
	}, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	h.tileCache = manager
	return h, manager, ws.ID, resourceIDs
}

func tileStatsRequest(h *handler, workspaceID, rawQuery string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/workspaces/"+workspaceID+"/tile-cache/stats"+rawQuery, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("workspace", workspaceID)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.getPersistentTileCacheStats(w, r)
	return w
}

func putStatsTile(t *testing.T, manager *tilecache.Manager, workspaceID, resourceID string, column int, content []byte) {
	t.Helper()
	identity := tilecache.Identity{
		WorkspaceID: workspaceID, WorkspaceRevision: 1, ResourceID: resourceID,
		ResourceKind: "feature", Generation: 1, TileType: "map", MatrixSet: "WebMercatorQuad",
		Zoom: 2, Column: column, Row: 1, StyleDigest: "default", Format: "image/png",
	}
	stored, err := manager.Put(context.Background(), identity, content, tilecache.Policy{})
	if err != nil || !stored {
		t.Fatalf("Put(%s col=%d) = stored %v, err %v", resourceID, column, stored, err)
	}
}

func TestPersistentTileCacheStatsGroupByResource(t *testing.T) {
	h, manager, wsID, ids := newTileCacheStatsHandler(t, 5000)
	putStatsTile(t, manager, wsID, ids["roads"], 1, []byte("01234567890123456789")) // 20
	putStatsTile(t, manager, wsID, ids["buildings"], 1, []byte("0123456789"))       // 10
	putStatsTile(t, manager, wsID, ids["buildings"], 2, []byte("0123456789"))       // 20
	putStatsTile(t, manager, wsID, ids["water"], 1, []byte("12345"))                // 5

	w := tileStatsRequest(h, wsID, "?group_by=resource")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp tileStatsGroupedResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Enabled || resp.Backend == "" || resp.GroupBy != "resource" || resp.WorkspaceID != wsID {
		t.Fatalf("unexpected envelope: %+v", resp)
	}
	if resp.Limit != tilecache.DefaultResourceUsageLimit || resp.TotalResources != 3 || resp.ReturnedResources != 3 {
		t.Fatalf("counts = total %d returned %d limit %d", resp.TotalResources, resp.ReturnedResources, resp.Limit)
	}
	wantOrder := []string{ids["buildings"], ids["roads"], ids["water"]}
	for idx, want := range wantOrder {
		if resp.Resources[idx].ResourceID != want {
			t.Fatalf("order = %v, want %v", resourceIDsOf(resp.Resources), wantOrder)
		}
	}
	byID := map[string]tileStatsResourceRow{}
	for _, row := range resp.Resources {
		byID[row.ResourceID] = row
	}
	roads := byID[ids["roads"]]
	if roads.QuotaBytes != 500 || roads.RemainingBytes != 480 {
		t.Fatalf("roads quota = %+v", roads)
	}
	buildings := byID[ids["buildings"]]
	if buildings.EntryCount != 2 || buildings.QuotaBytes != 5000 || buildings.RemainingBytes != 4980 {
		t.Fatalf("buildings usage = %+v", buildings)
	}
}

func TestPersistentTileCacheStatsGroupByResourceGlobalQuotaFallback(t *testing.T) {
	h, manager, wsID, ids := newTileCacheStatsHandler(t, 0)
	putStatsTile(t, manager, wsID, ids["water"], 1, []byte("12345"))
	w := tileStatsRequest(h, wsID, "?group_by=resource")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp tileStatsGroupedResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Resources[0].QuotaBytes != 1<<30 || resp.Resources[0].RemainingBytes != 1<<30-5 {
		t.Fatalf("global fallback = %+v", resp.Resources[0])
	}
}

func TestPersistentTileCacheStatsGroupByResourceTruncation(t *testing.T) {
	h, manager, wsID, ids := newTileCacheStatsHandler(t, 0)
	putStatsTile(t, manager, wsID, ids["roads"], 1, []byte("30bytes----largest-payload-xx"))
	putStatsTile(t, manager, wsID, ids["buildings"], 1, []byte("20bytes----payload----"))
	putStatsTile(t, manager, wsID, ids["water"], 1, []byte("10bytes-p"))
	w := tileStatsRequest(h, wsID, "?group_by=resource&limit=2")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp tileStatsGroupedResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.TotalResources != 3 || resp.ReturnedResources != 2 || resp.Limit != 2 {
		t.Fatalf("counts = %+v", resp)
	}
	if resp.Resources[0].ResourceID != ids["roads"] || resp.Resources[1].ResourceID != ids["buildings"] {
		t.Fatalf("truncated order = %v", resourceIDsOf(resp.Resources))
	}
}

func TestPersistentTileCacheStatsGroupByResourceExcludesEmptyResources(t *testing.T) {
	h, manager, wsID, ids := newTileCacheStatsHandler(t, 0)
	putStatsTile(t, manager, wsID, ids["roads"], 1, []byte("cached"))
	// buildings and water are published but hold no cache entries.
	w := tileStatsRequest(h, wsID, "?group_by=resource")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp tileStatsGroupedResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.TotalResources != 1 || resp.ReturnedResources != 1 || resp.Resources[0].ResourceID != ids["roads"] {
		t.Fatalf("empty resources leaked: %+v", resp)
	}
}

func TestPersistentTileCacheStatsInvalidParameters(t *testing.T) {
	h, _, wsID, _ := newTileCacheStatsHandler(t, 0)
	for _, query := range []string{
		"?group_by=layer",
		"?group_by=resource&resource=roads",
		"?group_by=resource&limit=0",
		"?group_by=resource&limit=1001",
		"?group_by=resource&limit=abc",
		"?group_by=resource&limit=-5",
	} {
		w := tileStatsRequest(h, wsID, query)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d (%s)", query, w.Code, w.Body.String())
		}
	}
	for _, query := range []string{"?group_by=resource&limit=1", "?group_by=resource&limit=1000"} {
		if w := tileStatsRequest(h, wsID, query); w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d (%s)", query, w.Code, w.Body.String())
		}
	}
}

func TestPersistentTileCacheStatsBackwardCompatibility(t *testing.T) {
	h, manager, wsID, ids := newTileCacheStatsHandler(t, 7777)
	putStatsTile(t, manager, wsID, ids["roads"], 1, []byte("cached-tile"))

	// Default response keeps the global/workspace envelope and no resources key.
	w := tileStatsRequest(h, wsID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var defaultResp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&defaultResp); err != nil {
		t.Fatal(err)
	}
	if defaultResp["enabled"] != true {
		t.Fatalf("enabled = %v", defaultResp["enabled"])
	}
	if _, ok := defaultResp["resources"]; ok {
		t.Fatal("default response must not carry resources")
	}
	if _, ok := defaultResp["group_by"]; ok {
		t.Fatal("default response must not carry group_by")
	}
	workspace, _ := defaultResp["workspace"].(map[string]any)
	if workspace["entry_count"].(float64) != 1 {
		t.Fatalf("workspace usage = %v", workspace)
	}

	// Single-resource response remains unchanged.
	w = tileStatsRequest(h, wsID, "?resource=roads")
	if w.Code != http.StatusOK {
		t.Fatalf("single resource status = %d, %s", w.Code, w.Body.String())
	}
	var singleResp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&singleResp); err != nil {
		t.Fatal(err)
	}
	resource, _ := singleResp["resource"].(map[string]any)
	// roads carries an explicit resource quota of 500, which takes precedence
	// over the workspace quota in the legacy single-resource response.
	if resource["entry_count"].(float64) != 1 || resource["quota_bytes"].(float64) != 500 {
		t.Fatalf("resource usage = %v", resource)
	}
	if _, ok := singleResp["resources"]; ok {
		t.Fatal("single-resource response must not carry resources")
	}

	// limit without group_by is ignored by the legacy response.
	if w := tileStatsRequest(h, wsID, "?limit=1001"); w.Code != http.StatusOK {
		t.Fatalf("legacy limit handling = %d", w.Code)
	}
}

func TestPersistentTileCacheStatsDisabled(t *testing.T) {
	mockStore := newMockStore()
	h := newTestHandler(t, mockStore)
	h.tileCache = nil
	w := tileStatsRequest(h, "ws-1", "?group_by=resource")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["enabled"] != false {
		t.Fatalf("enabled = %v", resp["enabled"])
	}
}

func resourceIDsOf(rows []tileStatsResourceRow) []string {
	ids := make([]string, len(rows))
	for idx, row := range rows {
		ids[idx] = row.ResourceID
	}
	return ids
}
