package mgmt

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tobilg/neoserver/internal/datasource"
	"github.com/tobilg/neoserver/internal/identity"
	"github.com/tobilg/neoserver/internal/store"
	"github.com/tobilg/neoserver/internal/tilecache"
	"github.com/tobilg/neoserver/internal/tiles"
)

const (
	batchConnectionTestMaxServices = 100
	batchConnectionTestConcurrency = 8
	connectionTestDefaultTimeout   = 10 * time.Second
	connectionTestMinTimeout       = 100 * time.Millisecond
	connectionTestMaxTimeout       = 30 * time.Second
)

// BatchTestConnectionsRequest is the body for the batch connection test.
type BatchTestConnectionsRequest struct {
	// Services lists stored service names to probe. It is required, holds at
	// most 100 entries, and duplicate names are checked only once (the first
	// occurrence determines the result position).
	Services []string `json:"services"`
	// TimeoutMS bounds every individual check. It defaults to 10000 and must
	// be between 100 and 30000 when provided.
	TimeoutMS *int `json:"timeout_ms,omitempty"`
}

// ServiceConnectionResult is one item of a batch connection-test response.
type ServiceConnectionResult struct {
	ServiceID  string  `json:"service_id"`
	Name       string  `json:"name"`
	OK         bool    `json:"ok"`
	DurationMS float64 `json:"duration_ms"`
	Message    string  `json:"message,omitempty"`
}

func (h *handler) workspaceSummary(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := h.resolveWorkspaceIDForSettings(r.Context(), chi.URLParam(r, "workspace"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Not Found", "workspace not found")
		return
	}
	counts := store.WorkspaceCounts{}
	if catalog, ok := h.store.(store.WorkspaceCountStore); ok {
		all, countErr := catalog.ListWorkspaceCounts(r.Context())
		if countErr == nil {
			counts = all[workspaceID]
		}
	}
	protocols := map[string]bool{}
	if catalog, ok := h.store.(store.STACSettingsStore); ok {
		if settings, err := catalog.GetSTACSettings(r.Context(), workspaceID); err == nil {
			protocols["stac"] = settings.Enabled
		}
	}
	if value, getErr := h.store.GetWMSSettings(r.Context(), workspaceID); getErr == nil {
		protocols["wms"] = value.Enabled
	}
	if value, getErr := h.store.GetWFSSettings(r.Context(), workspaceID); getErr == nil {
		protocols["wfs"] = value.Enabled
	}
	if value, getErr := h.store.GetOGCAPISettings(r.Context(), workspaceID); getErr == nil {
		protocols["ogcapi"] = value.Enabled
	}
	if value, getErr := h.store.GetOGCTilesAPISettings(r.Context(), workspaceID); getErr == nil {
		protocols["ogc_tiles"] = value.Enabled
	}
	if catalog, ok := h.store.(store.CoverageStore); ok {
		if value, getErr := catalog.GetWCSSettings(r.Context(), workspaceID); getErr == nil {
			protocols["wcs"] = value.Enabled
		}
	}
	if catalog, ok := h.store.(store.WMTSStore); ok {
		if value, getErr := catalog.GetWMTSSettings(r.Context(), workspaceID); getErr == nil {
			protocols["wmts"] = value.Enabled
		}
	}
	activeImports := 0
	if imports, ok := h.store.(store.ImportStore); ok {
		if jobs, listErr := imports.ListImportJobs(r.Context(), workspaceID, 1000); listErr == nil {
			for _, job := range jobs {
				if job.CompletedAt == nil && job.Status != store.ImportCancelled && job.Status != store.ImportFailed {
					activeImports++
				}
			}
		}
	}
	activeTileJobs := 0
	if h.tileJobs != nil {
		if jobs, listErr := h.tileJobs.List(r.Context(), workspaceID, 1000); listErr == nil {
			for _, job := range jobs {
				if job.Status == tilecache.JobQueued || job.Status == tilecache.JobRunning || job.Status == tilecache.JobCancelling {
					activeTileJobs++
				}
			}
		}
	}
	response := map[string]any{"workspace_id": workspaceID, "counts": counts, "protocols": protocols,
		"active_jobs": map[string]int{"imports": activeImports, "tile_cache": activeTileJobs}}
	if h.cache != nil {
		response["cache"] = h.cache.GetMetrics()
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *handler) listWorkspaceRoles(w http.ResponseWriter, r *http.Request) {
	principal, _ := identity.FromContext(r.Context())
	roles, err := h.store.ListRoles(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Error", "failed to list roles")
		return
	}
	result := make([]RoleResponse, 0, len(roles))
	for _, role := range roles {
		if role.ID == "super_admin" && !principal.IsSuperAdmin() {
			continue
		}
		result = append(result, RoleResponse{ID: role.ID, Name: role.Name, Description: role.Description,
			IsSystem: role.IsSystem, CreatedAt: role.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": result})
}

func (h *handler) listWorkspaceTileMatrixSets(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := h.resolveWorkspaceIDForSettings(r.Context(), chi.URLParam(r, "workspace"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Not Found", "workspace not found")
		return
	}
	settings, err := h.store.GetOGCTilesAPISettings(r.Context(), workspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Error", "failed to load tile settings")
		return
	}
	definitions := make([]*tiles.TileMatrixSetDefinition, 0, len(settings.Settings.TileMatrixSets))
	for _, id := range settings.Settings.TileMatrixSets {
		if definition, getErr := tiles.GetTileMatrixSetDefinition(id); getErr == nil {
			definitions = append(definitions, definition)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tile_matrix_sets": definitions})
}

func (h *handler) testNewServiceConnection(w http.ResponseWriter, r *http.Request) {
	var request CreateServiceRequest
	if readJSON(r, &request) != nil || request.Type == "" {
		writeError(w, http.StatusBadRequest, "Bad Request", "type and connection_info are required")
		return
	}
	if !validManagedBinding(nil, request.ConnectionInfo) {
		writeError(w, http.StatusBadRequest, "Bad Request", "managed import bindings are server-owned")
		return
	}
	if !h.authorizeServiceEndpoint(w, r, store.ServiceType(request.Type), nil, request.ConnectionInfo) {
		return
	}
	h.runConnectionTest(w, r, &store.Service{ID: "connection-test", Name: request.Name,
		Type: store.ServiceType(request.Type), ConnectionInfo: request.ConnectionInfo, Enabled: true})
}

func (h *handler) testExistingServiceConnection(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := h.resolveWorkspaceID(r.Context(), chi.URLParam(r, "workspace"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Not Found", "workspace not found")
		return
	}
	serviceID, err := h.resolveServiceID(r.Context(), workspaceID, chi.URLParam(r, "service"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Not Found", "service not found")
		return
	}
	existing, err := h.store.GetService(r.Context(), serviceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Not Found", "service not found")
		return
	}
	var request UpdateServiceRequest
	if readJSON(r, &request) != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "invalid connection test request")
		return
	}
	if request.ConnectionInfo != nil {
		merged := mergeConnectionSecrets(existing.ConnectionInfo, request.ConnectionInfo)
		if !validManagedBinding(existing.ConnectionInfo, merged) {
			writeError(w, http.StatusBadRequest, "Bad Request", "managed import bindings cannot be changed")
			return
		}
		if !h.authorizeServiceEndpoint(w, r, existing.Type, existing.ConnectionInfo, merged) {
			return
		}
		existing.ConnectionInfo = merged
	}
	h.runConnectionTest(w, r, existing)
}

func (h *handler) runConnectionTest(w http.ResponseWriter, r *http.Request, service *store.Service) {
	outcome := h.testServiceConnection(r.Context(), service, connectionTestDefaultTimeout)
	response := map[string]any{"ok": outcome.OK, "duration_ms": outcome.DurationMS}
	if outcome.Message != "" {
		response["message"] = outcome.Message
	}
	writeJSON(w, http.StatusOK, response)
}

// connectionTestOutcome is the transport-neutral result of one check.
type connectionTestOutcome struct {
	OK         bool
	DurationMS float64
	Message    string
}

// testServiceConnection runs the same connection preparation and health check
// for both the single and batch endpoints. The per-check timeout bounds the
// whole attempt, including the initial health check performed by
// datasource.Prepare; no connection credentials are included in the outcome.
func (h *handler) testServiceConnection(ctx context.Context, service *store.Service, timeout time.Duration) connectionTestOutcome {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	source, err := datasource.Prepare(ctx, func() (datasource.DataSource, error) { return datasource.CreateFromService(service) })
	if err == nil {
		defer source.Close()
		err = source.Health(ctx)
	}
	outcome := connectionTestOutcome{OK: err == nil, DurationMS: float64(time.Since(started).Microseconds()) / 1000}
	if err != nil {
		outcome.Message = err.Error()
	}
	return outcome
}

// testServiceConnections handles POST /workspaces/{workspace}/services/test-connections.
// It checks stored services concurrently (at most batchConnectionTestConcurrency
// at a time), keeps the de-duplicated request order in the response, and never
// lets one failing service stop the others.
func (h *handler) testServiceConnections(w http.ResponseWriter, r *http.Request) {
	var request BatchTestConnectionsRequest
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "invalid JSON")
		return
	}
	if len(request.Services) == 0 {
		writeError(w, http.StatusBadRequest, "Bad Request", "services must contain at least one service name")
		return
	}
	if len(request.Services) > batchConnectionTestMaxServices {
		writeError(w, http.StatusBadRequest, "Bad Request", "at most 100 services can be checked per request")
		return
	}
	timeout := connectionTestDefaultTimeout
	if request.TimeoutMS != nil {
		value := *request.TimeoutMS
		if value < int(connectionTestMinTimeout/time.Millisecond) || value > int(connectionTestMaxTimeout/time.Millisecond) {
			writeError(w, http.StatusBadRequest, "Bad Request", "timeout_ms must be between 100 and 30000 milliseconds")
			return
		}
		timeout = time.Duration(value) * time.Millisecond
	}

	workspaceID, err := h.resolveWorkspaceID(r.Context(), chi.URLParam(r, "workspace"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Not Found", "workspace not found")
		return
	}

	// Resolve the catalog once so the batch does not issue one lookup per name.
	storedServices, err := h.store.ListServices(r.Context(), workspaceID)
	if err != nil {
		h.logger.Error("failed to list services for batch connection test", "workspace", workspaceID, "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Error", "failed to list services")
		return
	}
	servicesByName := make(map[string]*store.Service, len(storedServices))
	for _, service := range storedServices {
		servicesByName[service.Name] = service
	}

	// De-duplicate by first occurrence; positions in names drive the order of
	// the response even though checks run concurrently.
	names := make([]string, 0, len(request.Services))
	seen := make(map[string]struct{}, len(request.Services))
	for _, name := range request.Services {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}

	results := make([]ServiceConnectionResult, len(names))
	slots := make(chan struct{}, batchConnectionTestConcurrency)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(index int, serviceName string) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-r.Context().Done():
				// Request canceled before this check started: end immediately
				// instead of waiting for a concurrency slot.
				results[index] = ServiceConnectionResult{Name: serviceName, Message: r.Context().Err().Error()}
				return
			}
			results[index] = h.checkStoredServiceConnection(r.Context(), servicesByName[serviceName], serviceName, timeout)
		}(i, name)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// checkStoredServiceConnection runs one batch item. A nil service means the
// requested name is unknown; it is reported as a failed item rather than
// dropping it. All failures are returned in the item, never as HTTP errors.
func (h *handler) checkStoredServiceConnection(ctx context.Context, service *store.Service, name string, timeout time.Duration) (result ServiceConnectionResult) {
	result.Name = name
	started := time.Now()
	defer func() { result.DurationMS = float64(time.Since(started).Microseconds()) / 1000 }()

	if service == nil {
		result.Message = "service not found"
		return result
	}
	result.ServiceID = service.ID
	// Reuse the same safeguards as single tests: a managed import binding is
	// server-owned and the stored PostGIS endpoint must remain authorized.
	if !validManagedBinding(service.ConnectionInfo, service.ConnectionInfo) {
		result.Message = "managed import bindings are server-owned"
		return result
	}
	if !h.serviceEndpointAllowed(ctx, service.Type, service.ConnectionInfo, service.ConnectionInfo) {
		result.Message = "PostGIS host is not listed in Datasource.DatabaseHosts"
		return result
	}
	outcome := h.testServiceConnection(ctx, service, timeout)
	result.OK = outcome.OK
	result.Message = outcome.Message
	return result
}

func sanitizeConnectionInfo(raw json.RawMessage) (json.RawMessage, []string) {
	if len(raw) == 0 {
		return raw, nil
	}
	var values map[string]any
	if json.Unmarshal(raw, &values) != nil {
		return nil, nil
	}
	configured := make([]string, 0)
	for _, key := range []string{"password"} {
		if value, ok := values[key]; ok {
			if text, stringValue := value.(string); stringValue && text != "" {
				configured = append(configured, key)
			}
			delete(values, key)
		}
	}
	encoded, _ := json.Marshal(values)
	return encoded, configured
}

func mergeConnectionSecrets(existing, update json.RawMessage) json.RawMessage {
	var oldValues, newValues map[string]any
	if json.Unmarshal(existing, &oldValues) != nil || json.Unmarshal(update, &newValues) != nil {
		return update
	}
	for _, key := range []string{"password"} {
		value, present := newValues[key]
		if !present || value == "" {
			if old, exists := oldValues[key]; exists {
				newValues[key] = old
			}
		}
	}
	encoded, _ := json.Marshal(newValues)
	return encoded
}
