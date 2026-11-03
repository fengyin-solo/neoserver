package mgmt

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/tobilg/neoserver/internal/store"
	"github.com/tobilg/neoserver/internal/wfs"
)

// WFSStoredQueryValidateRequest is the structured StoredQueryDefinition used
// by the WFS stored-query validation endpoint.
type WFSStoredQueryValidateRequest = wfs.StoredQueryDefinitionSpec

// WFSStoredQueryIssue describes one validation finding.
type WFSStoredQueryIssue = wfs.StoredQueryIssue

// WFSStoredQueryValidateResponse is the validation result. The endpoint never
// creates or modifies stored queries.
type WFSStoredQueryValidateResponse struct {
	Valid  bool                   `json:"valid"`
	Issues []wfs.StoredQueryIssue `json:"issues"`
}

// validateWFSStoredQuery handles POST
// /workspaces/{workspace}/wfs/stored-queries/validate.
//
// It validates a structured StoredQueryDefinition and reports every detected
// issue. It is strictly read-only: no stored query is created, updated, or
// deleted, and validation does not populate caches.
func (h *handler) validateWFSStoredQuery(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceIdentifier := chi.URLParam(r, "workspace")

	workspaceID, err := h.resolveWorkspaceIDForSettings(ctx, workspaceIdentifier)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "Not Found", "workspace not found")
			return
		}
		h.logger.Error("failed to resolve workspace", "workspace", workspaceIdentifier, "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Error", "failed to resolve workspace")
		return
	}

	var req WFSStoredQueryValidateRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "invalid JSON")
		return
	}

	ws, release, ok := h.registry.AcquireByID(workspaceID)
	if !ok {
		writeError(w, http.StatusNotFound, "Not Found", "workspace not found in registry")
		return
	}
	defer release()

	// Resolve the id collision question once up front so unexpected storage
	// failures surface as 5xx instead of being swallowed in an issue list.
	idExists := false
	if req.ID != "" && !wfs.IsGetFeatureByIdQuery(req.ID) {
		existing, err := h.store.GetWFSStoredQuery(ctx, workspaceID, req.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			h.logger.Error("failed to look up stored query", "workspace", workspaceID, "id", req.ID, "error", err)
			writeError(w, http.StatusInternalServerError, "Internal Error", "failed to validate stored query")
			return
		}
		idExists = existing != nil
	}

	issues := wfs.ValidateStoredQueryDefinition(ctx, ws, h.cfg.WFS.AppNamespacePrefix, req,
		func(_ context.Context, _, _ string) bool { return idExists })

	writeJSON(w, http.StatusOK, WFSStoredQueryValidateResponse{
		Valid:  len(issues) == 0,
		Issues: issues,
	})
}
