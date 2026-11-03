package mgmt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tobilg/neoserver/internal/conf"
	"github.com/tobilg/neoserver/internal/datasource"
	"github.com/tobilg/neoserver/internal/identity"
	"github.com/tobilg/neoserver/internal/rbac"
	"github.com/tobilg/neoserver/internal/store"
	"github.com/tobilg/neoserver/internal/workspace"
)

// batchTestServiceType is a test-only data source registered with the global
// datasource factory so the real Prepare/CreateFromService/Health path runs.
const batchTestServiceType store.ServiceType = "batchtest"

type batchTestSource struct {
	datasource.DataSource
	id    string
	delay time.Duration
	fail  string
}

func (s *batchTestSource) Type() store.ServiceType { return batchTestServiceType }
func (s *batchTestSource) ID() string              { return s.id }
func (s *batchTestSource) Close() error            { return nil }

var (
	batchTestActive    atomic.Int64
	batchTestMaxActive atomic.Int64
)

func resetBatchTestConcurrency() {
	batchTestActive.Store(0)
	batchTestMaxActive.Store(0)
}

func (s *batchTestSource) Health(ctx context.Context) error {
	active := batchTestActive.Add(1)
	for {
		peak := batchTestMaxActive.Load()
		if active <= peak || batchTestMaxActive.CompareAndSwap(peak, active) {
			break
		}
	}
	defer batchTestActive.Add(-1)
	if s.delay > 0 {
		timer := time.NewTimer(s.delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	if s.fail != "" {
		return errors.New(s.fail)
	}
	return nil
}

func batchTestFactory(svc *store.Service) (datasource.DataSource, error) {
	var info struct {
		DelayMS int    `json:"delay_ms"`
		Fail    string `json:"fail"`
	}
	_ = json.Unmarshal(svc.ConnectionInfo, &info)
	return &batchTestSource{
		id:    svc.ID,
		delay: time.Duration(info.DelayMS) * time.Millisecond,
		fail:  info.Fail,
	}, nil
}

func init() {
	datasource.Register(batchTestServiceType, batchTestFactory)
}

func batchHandlerFixture(t *testing.T, services ...*store.Service) (*handler, *mockStore) {
	t.Helper()
	catalog := newMockStore()
	catalog.workspaces["ws-1"] = &store.Workspace{ID: "ws-1", Name: "workspace-1"}
	for _, svc := range services {
		catalog.services[svc.ID] = svc
	}
	return newTestHandlerWithEnforcer(t, catalog), catalog
}

func batchService(id, name string, connectionInfo string) *store.Service {
	return &store.Service{
		ID:             id,
		WorkspaceID:    "ws-1",
		Name:           name,
		Type:           batchTestServiceType,
		ConnectionInfo: json.RawMessage(connectionInfo),
		Enabled:        true,
	}
}

func newBatchRequest(ctx context.Context, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	ctx = withChiContext(ctx, map[string]string{"workspace": "ws-1"})
	return r.WithContext(ctx)
}

func decodeBatchResults(t *testing.T, w *httptest.ResponseRecorder) []ServiceConnectionResult {
	t.Helper()
	var response struct {
		Results []ServiceConnectionResult `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v: %s", err, w.Body.String())
	}
	return response.Results
}

func TestBatchTestConnectionsOrderDedupMixedAndUnknown(t *testing.T) {
	resetBatchTestConcurrency()
	h, _ := batchHandlerFixture(t,
		batchService("svc-ok", "healthy", `{}`),
		// Credentials live in the stored connection info but must never appear
		// in the batch response.
		batchService("svc-bad", "broken", `{"fail":"boom","password":"s3cr3t"}`),
	)

	body := `{"services":["healthy","broken","healthy","ghost","broken"]}`
	w := httptest.NewRecorder()
	h.testServiceConnections(w, newBatchRequest(context.Background(), body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	results := decodeBatchResults(t, w)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3 (de-duplicated)", len(results))
	}
	if results[0].Name != "healthy" || !results[0].OK || results[0].ServiceID != "svc-ok" {
		t.Fatalf("unexpected first result: %+v", results[0])
	}
	if results[0].Message != "" {
		t.Fatalf("successful result must not carry a message: %+v", results[0])
	}
	if results[1].Name != "broken" || results[1].OK || results[1].ServiceID != "svc-bad" {
		t.Fatalf("unexpected second result: %+v", results[1])
	}
	if results[1].Message != "boom" {
		t.Fatalf("failure message = %q, want %q", results[1].Message, "boom")
	}
	if results[2].Name != "ghost" || results[2].OK || results[2].ServiceID != "" {
		t.Fatalf("unknown service must be a failed item without an id: %+v", results[2])
	}
	if results[2].Message != "service not found" {
		t.Fatalf("unknown message = %q", results[2].Message)
	}
	if strings.Contains(w.Body.String(), "password") || strings.Contains(w.Body.String(), "s3cr3t") {
		t.Fatalf("response leaked connection credentials: %s", w.Body.String())
	}
}

func TestBatchTestConnectionsConcurrencyLimit(t *testing.T) {
	resetBatchTestConcurrency()
	services := make([]*store.Service, 20)
	names := make([]string, 20)
	for i := range services {
		name := "svc-" + itoa(i)
		names[i] = name
		services[i] = batchService("id-"+itoa(i), name, `{"delay_ms":50}`)
	}
	h, _ := batchHandlerFixture(t, services...)

	payload, _ := json.Marshal(BatchTestConnectionsRequest{Services: names})
	started := time.Now()
	w := httptest.NewRecorder()
	h.testServiceConnections(w, newBatchRequest(context.Background(), string(payload)))
	elapsed := time.Since(started)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	results := decodeBatchResults(t, w)
	if len(results) != 20 {
		t.Fatalf("got %d results, want 20", len(results))
	}
	for i, result := range results {
		if result.Name != names[i] {
			t.Fatalf("result %d out of order: %q != %q", i, result.Name, names[i])
		}
		if !result.OK {
			t.Fatalf("result %s unexpectedly failed: %s", result.Name, result.Message)
		}
	}
	if peak := batchTestMaxActive.Load(); peak != int64(batchConnectionTestConcurrency) {
		t.Fatalf("peak concurrency = %d, want %d", peak, batchConnectionTestConcurrency)
	}
	// Twenty checks at ~100ms each would take ~2s sequentially; bounded
	// concurrency must finish much sooner.
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("batch took %s, checks were not run concurrently", elapsed)
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func TestBatchTestConnectionsPerItemTimeout(t *testing.T) {
	resetBatchTestConcurrency()
	h, _ := batchHandlerFixture(t,
		batchService("svc-slow", "slow", `{"delay_ms":1000}`),
	)

	w := httptest.NewRecorder()
	h.testServiceConnections(w, newBatchRequest(context.Background(), `{"services":["slow"],"timeout_ms":100}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	results := decodeBatchResults(t, w)
	if len(results) != 1 || results[0].OK {
		t.Fatalf("expected one failed result, got %+v", results)
	}
	if !strings.Contains(results[0].Message, "deadline exceeded") {
		t.Fatalf("message = %q, want a deadline error", results[0].Message)
	}
	if results[0].DurationMS > 700 {
		t.Fatalf("duration_ms = %v, timeout of 100ms was not honored", results[0].DurationMS)
	}
}

func TestBatchTestConnectionsTimeoutValidation(t *testing.T) {
	h, _ := batchHandlerFixture(t)
	for _, body := range []string{
		`{"services":["x"],"timeout_ms":99}`,
		`{"services":["x"],"timeout_ms":30001}`,
		`{"services":["x"],"timeout_ms":0}`,
		`{"services":["x"],"timeout_ms":-5}`,
	} {
		w := httptest.NewRecorder()
		h.testServiceConnections(w, newBatchRequest(context.Background(), body))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, w.Code)
		}
	}
	// Boundary values are accepted; unknown names return quickly as items.
	for _, timeout := range []int{100, 30000} {
		body, _ := json.Marshal(BatchTestConnectionsRequest{Services: []string{"missing"}, TimeoutMS: &timeout})
		w := httptest.NewRecorder()
		h.testServiceConnections(w, newBatchRequest(context.Background(), string(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("timeout_ms %d: status = %d, want 200", timeout, w.Code)
		}
	}
}

func TestBatchTestConnectionsRequestLimits(t *testing.T) {
	h, _ := batchHandlerFixture(t)

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"invalid json", `{not json`, http.StatusBadRequest},
		{"missing services", `{}`, http.StatusBadRequest},
		{"empty services", `{"services":[]}`, http.StatusBadRequest},
		{"too many services", `{"services":[` + strings.Repeat(`"a",`, 100) + `"b"]}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.testServiceConnections(w, newBatchRequest(context.Background(), tc.body))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}

	// Exactly the limit is accepted; every unknown name still yields an item.
	names := make([]string, 100)
	for i := range names {
		names[i] = "missing-" + itoa(i)
	}
	body, _ := json.Marshal(BatchTestConnectionsRequest{Services: names})
	w := httptest.NewRecorder()
	h.testServiceConnections(w, newBatchRequest(context.Background(), string(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if results := decodeBatchResults(t, w); len(results) != 100 {
		t.Fatalf("got %d results, want 100", len(results))
	}
}

func TestBatchTestConnectionsUnknownWorkspace(t *testing.T) {
	h, _ := batchHandlerFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"services":["x"]}`))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(withChiContext(r.Context(), map[string]string{"workspace": "nope"}))
	w := httptest.NewRecorder()
	h.testServiceConnections(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestBatchTestConnectionsCanceledBeforeStart(t *testing.T) {
	resetBatchTestConcurrency()
	services := make([]*store.Service, 16)
	names := make([]string, 16)
	for i := range services {
		name := "slow-" + itoa(i)
		names[i] = name
		services[i] = batchService("id-"+name, name, `{"delay_ms":500}`)
	}
	h, _ := batchHandlerFixture(t, services...)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	w := httptest.NewRecorder()
	h.testServiceConnections(w, newBatchRequest(ctx, `{"services":`+marshalNames(names)+`}`))
	elapsed := time.Since(started)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if elapsed > time.Second {
		t.Fatalf("canceled batch took %s; queued checks did not stop promptly", elapsed)
	}
	for _, result := range decodeBatchResults(t, w) {
		if result.OK {
			t.Fatalf("result %s succeeded after cancellation", result.Name)
		}
	}
}

func TestBatchTestConnectionsCanceledMidFlight(t *testing.T) {
	resetBatchTestConcurrency()
	services := make([]*store.Service, 16)
	names := make([]string, 16)
	for i := range services {
		name := "slow-" + itoa(i)
		names[i] = name
		services[i] = batchService("id-"+name, name, `{"delay_ms":400}`)
	}
	h, _ := batchHandlerFixture(t, services...)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	w := httptest.NewRecorder()
	h.testServiceConnections(w, newBatchRequest(ctx, `{"services":`+marshalNames(names)+`}`))
	elapsed := time.Since(started)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("canceling mid-flight took %s", elapsed)
	}
	for _, result := range decodeBatchResults(t, w) {
		if result.OK {
			t.Fatalf("result %s succeeded after cancellation", result.Name)
		}
	}
}

func marshalNames(names []string) string {
	data, _ := json.Marshal(names)
	return string(data)
}

// TestSingleTestConnectionBehaviorUnchanged pins the existing endpoint's
// status codes and wire shape while the batch endpoint reuses its core.
func TestSingleTestConnectionBehaviorUnchanged(t *testing.T) {
	h, _ := batchHandlerFixture(t,
		batchService("svc-ok", "healthy", `{}`),
		batchService("svc-bad", "broken", `{"fail":"boom"}`),
	)
	for _, tc := range []struct {
		service string
		ok      bool
		message string
	}{
		{"healthy", true, ""},
		{"broken", false, "boom"},
	} {
		t.Run(tc.service, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			r = r.WithContext(withChiContext(r.Context(), map[string]string{"workspace": "ws-1", "service": tc.service}))
			w := httptest.NewRecorder()
			h.testExistingServiceConnection(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["ok"] != tc.ok {
				t.Fatalf("ok = %v, want %v: %s", body["ok"], tc.ok, w.Body.String())
			}
			if _, present := body["duration_ms"]; !present {
				t.Fatal("duration_ms missing")
			}
			if tc.ok {
				if _, present := body["message"]; present {
					t.Fatalf("successful response must omit message: %s", w.Body.String())
				}
			} else if body["message"] != tc.message {
				t.Fatalf("message = %v, want %q", body["message"], tc.message)
			}
		})
	}
}

// TestBatchTestConnectionsPermissions exercises the full router: only
// workspace administrators may run batch connection tests.
func TestBatchTestConnectionsPermissions(t *testing.T) {
	ctx := context.Background()
	catalog, _, err := store.Init(store.Config{Path: filepath.Join(t.TempDir(), "catalog.db"), EncryptionKey: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	ws, err := catalog.CreateWorkspace(ctx, store.CreateWorkspaceInput{Name: "Perm"})
	if err != nil {
		t.Fatal(err)
	}
	enforcer, err := rbac.NewEnforcerWithDefaults(rbac.NewStoreAdapter(catalog))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		roles map[string]string
		want  int
	}{
		{"workspace admin", map[string]string{ws.ID: "admin"}, http.StatusOK},
		{"global viewer", map[string]string{"*": rbac.RoleViewer}, http.StatusForbidden},
		{"unrelated workspace admin", map[string]string{"other": "admin"}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := chi.NewRouter()
			router.Use(identity.Middleware(identity.MiddlewareConfig{Store: catalog}))
			router.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(w, r.WithContext(identity.WithIdentity(r.Context(), &identity.Identity{Subject: "test", Roles: tc.roles})))
				})
			})
			RegisterRoutes(router, Dependencies{
				Store: catalog, Enforcer: enforcer, Registry: workspace.NewRegistry(catalog, nil),
				Config: conf.Config{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			request := httptest.NewRequest(http.MethodPost, "/workspaces/"+ws.Name+"/services/test-connections",
				strings.NewReader(`{"services":["missing"]}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
			if tc.want == http.StatusOK {
				var body struct {
					Results []ServiceConnectionResult `json:"results"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if len(body.Results) != 1 || body.Results[0].OK || body.Results[0].Name != "missing" {
					t.Fatalf("unexpected results: %+v", body.Results)
				}
			}
		})
	}
}
