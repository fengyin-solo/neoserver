package mgmt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tobilg/neoserver/internal/conf"
	"github.com/tobilg/neoserver/internal/store"
	"github.com/tobilg/neoserver/internal/wfs"
)

const (
	wfsTestWFSNS = `xmlns:wfs="http://www.opengis.net/wfs/2.0"`
	wfsTestFESNS = `xmlns:fes="http://www.opengis.net/fes/2.0"`
)

// newStoredQueryFixture builds a workspace with one enabled service. The
// "cities" layer can be toggled off to simulate an invisible publication.
func newStoredQueryFixture(t *testing.T, citiesEnabled bool) (*handler, *mockStore) {
	t.Helper()
	m := newMockStore()
	m.workspaces["ws-1"] = &store.Workspace{ID: "ws-1", Name: "workspace-1"}
	m.services["svc-1"] = &store.Service{ID: "svc-1", WorkspaceID: "ws-1", Name: "service-1", Type: store.ServiceTypePostGIS, Enabled: true}
	m.layers["layer-cities"] = &store.Layer{ID: "layer-cities", ServiceID: "svc-1", PublicID: "cities", SourceLayer: "public.cities", Enabled: citiesEnabled}
	h := newTestHandlerWithEnforcer(t, m)
	return h, m
}

func validateStoredQueryRequest(t *testing.T, h *handler, body string) WFSStoredQueryValidateResponse {
	t.Helper()
	r := httptest.NewRequest("POST", "/workspaces/ws-1/wfs/stored-queries/validate", strings.NewReader(body))
	r = r.WithContext(withChiContext(r.Context(), map[string]string{"workspace": "ws-1"}))
	w := httptest.NewRecorder()
	h.validateWFSStoredQuery(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp WFSStoredQueryValidateResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v: %s", err, w.Body.String())
	}
	return resp
}

func issueCodes(resp WFSStoredQueryValidateResponse) []string {
	codes := make([]string, 0, len(resp.Issues))
	for _, issue := range resp.Issues {
		codes = append(codes, issue.Code)
	}
	return codes
}

func containsIssue(resp WFSStoredQueryValidateResponse, code string) bool {
	for _, issue := range resp.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func TestWFSStoredQueryValidate_ValidDefinition(t *testing.T) {
	h, m := newStoredQueryFixture(t, true)

	body := `{
	  "id": "urn:example:citiesByName",
	  "title": "Cities by name",
	  "abstract": "looks up a city by name",
	  "parameters": [{"name": "nameParam", "type": "xsd:string"}],
	  "query_expression_text": [{
	    "language": "urn:ogc:def:queryLanguage:OGC-WFS::WFS_QueryExpression",
	    "return_feature_types": ["cities"],
	    "content": "<wfs:Query typeNames=\"cities\" ` + wfsTestWFSNS + `><fes:Filter ` + wfsTestFESNS + `><fes:PropertyIsEqualTo><fes:ValueReference>name</fes:ValueReference><fes:Literal>${nameParam}</fes:Literal></fes:PropertyIsEqualTo></fes:Filter></wfs:Query>"
	  }]
	}`

	resp := validateStoredQueryRequest(t, h, body)
	if !resp.Valid || len(resp.Issues) != 0 {
		t.Fatalf("expected valid definition, got issues: %+v", resp.Issues)
	}
	if m.storedQueryCreates != 0 {
		t.Fatalf("validation created %d stored queries", m.storedQueryCreates)
	}
}

func TestWFSStoredQueryValidate_PlaceholderMatchingIsCaseInsensitive(t *testing.T) {
	h, _ := newStoredQueryFixture(t, true)

	// The declared parameter uses different casing than the expression
	// placeholder, and typeNames itself is parametric.
	body := `{
	  "id": "urn:example:parametric",
	  "parameters": [{"name": "TypeName"}],
	  "query_expression_text": [{
	    "content": "<Query typeNames=\"${typename}\"/>"
	  }]
	}`
	resp := validateStoredQueryRequest(t, h, body)
	if !resp.Valid {
		t.Fatalf("expected valid, got %+v", resp.Issues)
	}
}

func TestWFSStoredQueryValidate_MultipleIssuesAreSortedAndStable(t *testing.T) {
	h, _ := newStoredQueryFixture(t, true)

	body := `{
	  "id": "",
	  "parameters": [
	    {"name": ""},
	    {"name": "dup"},
	    {"name": "dup"}
	  ],
	  "query_expression_text": [{
	    "language": "urn:bogus:language",
	    "content": "<wfs:Query typeNames=\"ghosts\" ` + wfsTestWFSNS + `><fes:Filter ` + wfsTestFESNS + `><fes:PropertyIsEqualTo><fes:ValueReference>n</fes:ValueReference><fes:Literal>${missing}</fes:Literal></fes:PropertyIsEqualTo></fes:Filter></wfs:Query>"
	  }]
	}`

	first := validateStoredQueryRequest(t, h, body)
	second := validateStoredQueryRequest(t, h, body)

	if first.Valid {
		t.Fatal("expected invalid definition")
	}
	wantCodes := []string{
		wfs.StoredQueryIssueIDRequired,
		wfs.StoredQueryIssueParameterNameRequired,
		wfs.StoredQueryIssueDuplicateParameter,
		wfs.StoredQueryIssueUnsupportedLanguage,
		wfs.StoredQueryIssueUndeclaredParameter,
		wfs.StoredQueryIssueUnknownTypeName,
	}
	gotCodes := issueCodes(first)
	if len(gotCodes) != len(wantCodes) {
		t.Fatalf("expected %d issues, got %d: %v", len(wantCodes), len(gotCodes), gotCodes)
	}
	for i, want := range wantCodes {
		if gotCodes[i] != want {
			t.Fatalf("issue %d: want %s, got %s (all: %v)", i, want, gotCodes[i], gotCodes)
		}
	}

	// Identical request must yield byte-identical results.
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("non-deterministic results:\n%s\n%s", firstJSON, secondJSON)
	}

	// Every issue must carry a stable code/locator/message.
	for _, issue := range first.Issues {
		if issue.Code == "" || issue.Locator == "" || issue.Message == "" {
			t.Fatalf("issue missing fields: %+v", issue)
		}
	}
}

func TestWFSStoredQueryValidate_IDConflicts(t *testing.T) {
	h, m := newStoredQueryFixture(t, true)
	if _, err := m.CreateWFSStoredQuery(context.Background(), store.CreateWFSStoredQueryInput{
		WorkspaceID:     "ws-1",
		QueryID:         "urn:example:existing",
		QueryExpression: "<Query typeNames=\"cities\"/>",
	}); err != nil {
		t.Fatal(err)
	}

	existing := `{
	  "id": "urn:example:existing",
	  "query_expression_text": [{"content": "<Query typeNames=\"cities\"/>"}]
	}`
	resp := validateStoredQueryRequest(t, h, existing)
	if !containsIssue(resp, wfs.StoredQueryIssueIDExists) {
		t.Fatalf("expected %s, got %+v", wfs.StoredQueryIssueIDExists, resp.Issues)
	}

	for _, builtinID := range []string{
		"urn:ogc:def:query:OGC-WFS::GetFeatureById",
		"http://www.opengis.net/def/query/OGC-WFS/0/GetFeatureById",
	} {
		body := `{
		  "id": "` + builtinID + `",
		  "query_expression_text": [{"content": "<Query typeNames=\"cities\"/>"}]
		}`
		resp := validateStoredQueryRequest(t, h, body)
		if !containsIssue(resp, wfs.StoredQueryIssueIDReserved) {
			t.Fatalf("id %s: expected %s, got %+v", builtinID, wfs.StoredQueryIssueIDReserved, resp.Issues)
		}
	}
}

func TestWFSStoredQueryValidate_InvisibleAndUnknownTypeNames(t *testing.T) {
	hDisabled, _ := newStoredQueryFixture(t, false)
	invisible := validateStoredQueryRequest(t, hDisabled, `{
	  "id": "urn:example:hidden",
	  "query_expression_text": [{"content": "<Query typeNames=\"cities\"/>"}]
	}`)
	if !containsIssue(invisible, wfs.StoredQueryIssueTypeNameNotVisible) {
		t.Fatalf("expected %s, got %+v", wfs.StoredQueryIssueTypeNameNotVisible, invisible.Issues)
	}
	if containsIssue(invisible, wfs.StoredQueryIssueUnknownTypeName) {
		t.Fatal("disabled publication must not be reported as unknown")
	}

	hEnabled, _ := newStoredQueryFixture(t, true)
	unknown := validateStoredQueryRequest(t, hEnabled, `{
	  "id": "urn:example:ghost",
	  "query_expression_text": [{"content": "<Query typeNames=\"ghosts\"/>"}]
	}`)
	if !containsIssue(unknown, wfs.StoredQueryIssueUnknownTypeName) {
		t.Fatalf("expected %s, got %+v", wfs.StoredQueryIssueUnknownTypeName, unknown.Issues)
	}
	if containsIssue(unknown, wfs.StoredQueryIssueTypeNameNotVisible) {
		t.Fatal("unknown type must not be reported as invisible")
	}
}

func TestWFSStoredQueryValidate_ExpressionAndFilterProblems(t *testing.T) {
	h, _ := newStoredQueryFixture(t, true)

	cases := []struct {
		name string
		body string
		code string
	}{
		{
			name: "missing expression text",
			body: `{"id": "urn:example:noexpr", "query_expression_text": []}`,
			code: wfs.StoredQueryIssueExpressionRequired,
		},
		{
			name: "empty expression content",
			body: `{"id": "urn:example:empty", "query_expression_text": [{"content": "   "]}`,
			code: wfs.StoredQueryIssueExpressionRequired,
		},
		{
			name: "root is not Query",
			body: `{"id": "urn:example:wrongroot", "query_expression_text": [{"content": "<SomethingElse typeNames=\"cities\"/>"}]}`,
			code: wfs.StoredQueryIssueInvalidQuery,
		},
		{
			name: "missing typeNames",
			body: `{"id": "urn:example:notypenames", "query_expression_text": [{"content": "<Query/>"}]}`,
			code: wfs.StoredQueryIssueInvalidQuery,
		},
		{
			name: "malformed XML",
			body: `{"id": "urn:example:malformed", "query_expression_text": [{"content": "<Query typeNames=\"cities\"><Filter>"}]}`,
			code: wfs.StoredQueryIssueInvalidQuery,
		},
		{
			name: "undeclared placeholder",
			body: `{"id": "urn:example:undeclared", "query_expression_text": [{"content": "<Query typeNames=\"cities\"><Filter><PropertyIsEqualTo><ValueReference>n</ValueReference><Literal>${who}</Literal></PropertyIsEqualTo></Filter></Query>"}]}`,
			code: wfs.StoredQueryIssueUndeclaredParameter,
		},
		{
			name: "duplicate parameter",
			body: `{"id": "urn:example:dup", "parameters": [{"name": "p"}, {"name": "P"}], "query_expression_text": [{"content": "<Query typeNames=\"cities\"/>"}]}`,
			code: wfs.StoredQueryIssueDuplicateParameter,
		},
		{
			name: "invalid filter",
			body: `{"id": "urn:example:badfilter", "query_expression_text": [{"content": "<Query typeNames=\"cities\" ` + wfsTestWFSNS + `><fes:Filter ` + wfsTestFESNS + `><fes:PropertyIsEqualTo><fes:ValueReference>name</fes:ValueReference></fes:PropertyIsEqualTo></fes:Filter></wfs:Query>"}]}`,
			code: wfs.StoredQueryIssueInvalidFilter,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := validateStoredQueryRequest(t, h, tc.body)
			if !containsIssue(resp, tc.code) {
				t.Fatalf("expected issue %s, got %+v", tc.code, resp.Issues)
			}
		})
	}
}

func TestWFSStoredQueryValidate_HasNoSideEffects(t *testing.T) {
	h, m := newStoredQueryFixture(t, true)

	bodies := []string{
		// Valid definition.
		`{"id": "urn:example:ok", "parameters": [{"name": "p"}], "query_expression_text": [{"content": "<Query typeNames=\"cities\"><Filter><PropertyIsEqualTo><ValueReference>n</ValueReference><Literal>${p}</Literal></PropertyIsEqualTo></Filter></Query>"}]}`,
		// Invalid definition with multiple problems.
		`{"id": "", "query_expression_text": [{"language": "urn:bad", "content": "<Nope/>"}]}`,
		// Conflicts with a query that already exists.
		`{"id": "urn:example:ok", "query_expression_text": [{"content": "<Query typeNames=\"cities\"/>"}]}`,
	}
	for _, body := range bodies {
		validateStoredQueryRequest(t, h, body)
	}
	if m.storedQueryCreates != 0 {
		t.Fatalf("validation invoked CreateWFSStoredQuery %d times", m.storedQueryCreates)
	}
	if queries, _ := m.ListWFSStoredQueries(context.Background(), "ws-1"); len(queries) != 0 {
		t.Fatalf("validation left stored queries behind: %+v", queries)
	}
}

func TestWFSStoredQueryValidate_InvalidJSONRejected(t *testing.T) {
	h, _ := newStoredQueryFixture(t, true)
	r := httptest.NewRequest("POST", "/workspaces/ws-1/wfs/stored-queries/validate", strings.NewReader("{not json"))
	r = r.WithContext(withChiContext(r.Context(), map[string]string{"workspace": "ws-1"}))
	w := httptest.NewRecorder()
	h.validateWFSStoredQuery(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestWFSStoredQueryValidate_OpenAPIDocumented(t *testing.T) {
	doc := BuildOpenAPI(conf.Config{})
	item := doc.Paths.Value("/workspaces/{workspace}/wfs/stored-queries/validate")
	if item == nil || item.Post == nil {
		t.Fatal("validate operation missing from OpenAPI")
	}
	if item.Post.OperationID != "validateWFSStoredQuery" {
		t.Fatalf("unexpected operation id: %s", item.Post.OperationID)
	}
	bodySchema := item.Post.RequestBody.Value.Content["application/json"].Schema
	if bodySchema.Ref != "#/components/schemas/WFSStoredQueryDefinition" {
		t.Fatalf("request body schema: %s", bodySchema.Ref)
	}
	okSchema := item.Post.Responses.Status(200).Value.Content["application/json"].Schema
	if okSchema.Ref != "#/components/schemas/WFSStoredQueryValidationResponse" {
		t.Fatalf("response schema: %s", okSchema.Ref)
	}
	for _, name := range []string{"WFSStoredQueryDefinition", "WFSStoredQueryParameter", "WFSQueryExpressionText", "WFSStoredQueryIssue", "WFSStoredQueryValidationResponse"} {
		if doc.Components.Schemas[name] == nil {
			t.Errorf("schema %s not registered", name)
		}
	}
	issue := doc.Components.Schemas["WFSStoredQueryIssue"].Value
	for _, field := range []string{"code", "locator", "message"} {
		if _, ok := issue.Properties[field]; !ok {
			t.Errorf("issue schema missing %s", field)
		}
	}
}
