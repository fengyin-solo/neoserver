package wfs

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/tobilg/neoserver/internal/workspace"
)

// Stable issue codes returned by the stored-query validator. They are part of
// the management API contract and must not change between releases.
const (
	// StoredQueryIssueIDRequired is reported when the definition has no id.
	StoredQueryIssueIDRequired = "stored_query_id_required"
	// StoredQueryIssueIDReserved is reported when the id overrides a built-in
	// stored query (GetFeatureById).
	StoredQueryIssueIDReserved = "stored_query_id_reserved"
	// StoredQueryIssueIDExists is reported when the id is already in use.
	StoredQueryIssueIDExists = "stored_query_id_exists"
	// StoredQueryIssueExpressionRequired is reported when no QueryExpressionText
	// is supplied or its body is empty.
	StoredQueryIssueExpressionRequired = "query_expression_required"
	// StoredQueryIssueUnsupportedLanguage is reported for an unknown language.
	StoredQueryIssueUnsupportedLanguage = "unsupported_query_language"
	// StoredQueryIssueParameterNameRequired is reported for an unnamed
	// parameter declaration.
	StoredQueryIssueParameterNameRequired = "parameter_name_required"
	// StoredQueryIssueDuplicateParameter is reported when the same parameter
	// name is declared more than once.
	StoredQueryIssueDuplicateParameter = "duplicate_parameter"
	// StoredQueryIssueUndeclaredParameter is reported when the expression uses
	// a ${placeholder} that has no matching Parameter declaration.
	StoredQueryIssueUndeclaredParameter = "undeclared_parameter"
	// StoredQueryIssueInvalidQuery is reported when the expression body is not
	// a parseable wfs:Query element.
	StoredQueryIssueInvalidQuery = "invalid_query_expression"
	// StoredQueryIssueInvalidFilter is reported when the embedded FES Filter
	// cannot be parsed.
	StoredQueryIssueInvalidFilter = "invalid_filter"
	// StoredQueryIssueUnknownTypeName is reported when a typeName does not
	// match any publication in the workspace.
	StoredQueryIssueUnknownTypeName = "unknown_type_name"
	// StoredQueryIssueTypeNameNotVisible is reported when a typeName matches a
	// publication that is currently not served (disabled layer or service).
	StoredQueryIssueTypeNameNotVisible = "type_name_not_visible"
)

// StoredQueryIssue describes a single validation problem. All fields are
// stable strings so results can be compared across repeated requests.
type StoredQueryIssue struct {
	Code    string `json:"code"`
	Locator string `json:"locator"`
	Message string `json:"message"`
}

// StoredQueryParameterSpec is the structured form of a wfs:Parameter
// declaration.
type StoredQueryParameterSpec struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// StoredQueryExpressionSpec is the structured form of a
// wfs:QueryExpressionText element.
type StoredQueryExpressionSpec struct {
	Language           string   `json:"language,omitempty"`
	ReturnFeatureTypes []string `json:"return_feature_types,omitempty"`
	IsPrivate          bool     `json:"is_private,omitempty"`
	// Content holds the inner wfs:Query XML fragment, including
	// ${placeholder} references.
	Content string `json:"content"`
}

// StoredQueryDefinitionSpec is the structured management-API form of a WFS
// StoredQueryDefinition.
type StoredQueryDefinitionSpec struct {
	ID                  string                      `json:"id"`
	Title               string                      `json:"title,omitempty"`
	Abstract            string                      `json:"abstract,omitempty"`
	Parameters          []StoredQueryParameterSpec  `json:"parameters,omitempty"`
	QueryExpressionText []StoredQueryExpressionSpec `json:"query_expression_text,omitempty"`
}

// StoredQueryExistence checks whether a custom stored query with the given id
// already exists in the workspace. It performs read-only storage access.
type StoredQueryExistence func(ctx context.Context, workspaceID, queryID string) bool

// ValidateStoredQueryDefinition validates a structured StoredQueryDefinition
// without creating or modifying any stored query. It returns every detected
// issue sorted by (code, locator, message), so identical inputs always yield
// identical results. The definition is valid exactly when the returned slice
// is empty.
func ValidateStoredQueryDefinition(ctx context.Context, ws *workspace.Workspace, appNamespacePrefix string, def StoredQueryDefinitionSpec, exists StoredQueryExistence) []StoredQueryIssue {
	var issues []StoredQueryIssue

	// --- Identifier -------------------------------------------------------
	if strings.TrimSpace(def.ID) == "" {
		issues = append(issues, StoredQueryIssue{
			Code:    StoredQueryIssueIDRequired,
			Locator: "id",
			Message: "StoredQueryDefinition requires a non-empty id",
		})
	} else {
		if IsGetFeatureByIdQuery(def.ID) {
			issues = append(issues, StoredQueryIssue{
				Code:    StoredQueryIssueIDReserved,
				Locator: "id",
				Message: fmt.Sprintf("stored query id %q conflicts with the built-in GetFeatureById query", def.ID),
			})
		}
		if ws != nil && exists != nil && exists(ctx, ws.ID, def.ID) {
			issues = append(issues, StoredQueryIssue{
				Code:    StoredQueryIssueIDExists,
				Locator: "id",
				Message: fmt.Sprintf("stored query with id %q already exists", def.ID),
			})
		}
	}

	// --- Parameters -------------------------------------------------------
	declared := make(map[string]int)
	for i, param := range def.Parameters {
		locator := fmt.Sprintf("parameters[%d].name", i)
		name := strings.TrimSpace(param.Name)
		if name == "" {
			issues = append(issues, StoredQueryIssue{
				Code:    StoredQueryIssueParameterNameRequired,
				Locator: locator,
				Message: "parameter declaration requires a name",
			})
			continue
		}
		key := strings.ToLower(name)
		if first, dup := declared[key]; dup {
			issues = append(issues, StoredQueryIssue{
				Code:    StoredQueryIssueDuplicateParameter,
				Locator: locator,
				Message: fmt.Sprintf("parameter %q duplicates parameters[%d]", param.Name, first),
			})
			continue
		}
		declared[key] = i
	}

	// --- Query expressions ------------------------------------------------
	if len(def.QueryExpressionText) == 0 {
		issues = append(issues, StoredQueryIssue{
			Code:    StoredQueryIssueExpressionRequired,
			Locator: "query_expression_text",
			Message: "StoredQueryDefinition requires at least one QueryExpressionText",
		})
	}
	for i, expr := range def.QueryExpressionText {
		locator := fmt.Sprintf("query_expression_text[%d]", i)
		if expr.Language != "" && expr.Language != QueryLanguageWFS && expr.Language != QueryLanguageWFSAlt {
			issues = append(issues, StoredQueryIssue{
				Code:    StoredQueryIssueUnsupportedLanguage,
				Locator: locator + ".language",
				Message: fmt.Sprintf("unsupported query language %q; supported language is %s", expr.Language, QueryLanguageWFS),
			})
		}

		for _, name := range undeclaredPlaceholders(expr.Content, declared) {
			issues = append(issues, StoredQueryIssue{
				Code:    StoredQueryIssueUndeclaredParameter,
				Locator: locator,
				Message: fmt.Sprintf("query expression references placeholder ${%s} that is not declared as a parameter", name),
			})
		}

		if strings.TrimSpace(expr.Content) == "" {
			issues = append(issues, StoredQueryIssue{
				Code:    StoredQueryIssueExpressionRequired,
				Locator: locator + ".content",
				Message: "QueryExpressionText requires a wfs:Query expression",
			})
			continue
		}

		typeNames, parseErr := parseStoredQueryExpression(expr.Content)
		if parseErr != nil {
			issues = append(issues, StoredQueryIssue{
				Code:    StoredQueryIssueInvalidQuery,
				Locator: locator,
				Message: "cannot parse query expression as wfs:Query: " + parseErr.Error(),
			})
			continue
		}

		if _, filterErr := extractFilterFromXML(expr.Content); filterErr != nil {
			issues = append(issues, StoredQueryIssue{
				Code:    StoredQueryIssueInvalidFilter,
				Locator: locator + ".filter",
				Message: filterErr.Error(),
			})
		}

		for _, typeName := range typeNames {
			// Type names supplied via placeholders (e.g. typeNames="${type}")
			// cannot be resolved statically; the placeholder declaration is
			// checked separately.
			if strings.Contains(typeName, "${") {
				continue
			}
			if ws == nil {
				issues = append(issues, StoredQueryIssue{
					Code:    StoredQueryIssueUnknownTypeName,
					Locator: locator + ".typeNames",
					Message: fmt.Sprintf("unknown type name: %s", typeName),
				})
				continue
			}
			if layer, _ := resolveFeatureLayer(ws, typeName, appNamespacePrefix); layer != nil {
				continue
			}
			if layer := resolveCatalogFeatureLayer(ws, typeName, appNamespacePrefix); layer != nil {
				issues = append(issues, StoredQueryIssue{
					Code:    StoredQueryIssueTypeNameNotVisible,
					Locator: locator + ".typeNames",
					Message: fmt.Sprintf("type name %s matches a publication that is not currently visible (layer or service is disabled)", typeName),
				})
			} else {
				issues = append(issues, StoredQueryIssue{
					Code:    StoredQueryIssueUnknownTypeName,
					Locator: locator + ".typeNames",
					Message: fmt.Sprintf("unknown type name: %s", typeName),
				})
			}
		}
	}

	slices.SortFunc(issues, func(a, b StoredQueryIssue) int {
		if a.Code != b.Code {
			return strings.Compare(a.Code, b.Code)
		}
		if a.Locator != b.Locator {
			return strings.Compare(a.Locator, b.Locator)
		}
		return strings.Compare(a.Message, b.Message)
	})
	return slices.CompactFunc(issues, func(a, b StoredQueryIssue) bool {
		return a.Code == b.Code && a.Locator == b.Locator && a.Message == b.Message
	})
}

var placeholderPattern = regexp.MustCompile(`\$\{([^{}]*)\}`)

// undeclaredPlaceholders returns the distinct placeholder names used in rawXML
// that have no case-insensitive match in declared parameters, in first-use
// order.
func undeclaredPlaceholders(rawXML string, declared map[string]int) []string {
	seen := make(map[string]bool)
	var missing []string
	for _, match := range placeholderPattern.FindAllStringSubmatch(rawXML, -1) {
		name := match[1]
		key := strings.ToLower(strings.TrimSpace(name))
		if _, ok := declared[key]; ok {
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		missing = append(missing, name)
	}
	return missing
}

// parseStoredQueryExpression verifies that rawXML is a single, well-formed
// wfs:Query element and returns its space-separated typeNames.
func parseStoredQueryExpression(rawXML string) ([]string, error) {
	wrapped := `<root xmlns:wfs="` + NSWfs + `" xmlns:fes="` + NSFes + `" xmlns:gml="` + NSGml + `">` + rawXML + `</root>`
	d := xml.NewDecoder(strings.NewReader(wrapped))
	depth := 0
	topLevel := 0
	var typeNames string
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := token.(type) {
		case xml.StartElement:
			if t.Name.Local == "root" {
				depth = 0
				continue
			}
			if depth == 0 {
				topLevel++
				if topLevel > 1 {
					return nil, fmt.Errorf("expected a single Query element")
				}
				if t.Name.Local != "Query" {
					return nil, fmt.Errorf("expected element type <Query> but have <%s>", t.Name.Local)
				}
				if t.Name.Space != "" && t.Name.Space != NSWfs {
					return nil, fmt.Errorf("Query must be in the WFS namespace %s", NSWfs)
				}
				for _, attr := range t.Attr {
					if attr.Name.Local == "typeNames" {
						typeNames = attr.Value
					}
				}
			}
			depth++
		case xml.EndElement:
			if t.Name.Local == "root" {
				continue
			}
			depth--
		}
	}
	if topLevel == 0 {
		return nil, fmt.Errorf("QueryExpressionText must contain a Query element")
	}
	if strings.TrimSpace(typeNames) == "" {
		return nil, fmt.Errorf("Query requires a typeNames attribute")
	}
	return strings.Fields(typeNames), nil
}

// resolveCatalogFeatureLayer finds a publication for typeName regardless of
// whether the layer or its service is enabled. It is used to distinguish an
// unknown type name from one that exists but is not currently served.
// Ambiguous matches are not resolved.
func resolveCatalogFeatureLayer(ws *workspace.Workspace, typeName, appPrefix string) *workspace.Layer {
	if ws == nil || typeName == "" {
		return nil
	}
	layers := ws.GetCatalogLayers()
	slices.SortFunc(layers, func(a, b *workspace.Layer) int { return strings.Compare(a.PublicID, b.PublicID) })

	var matched *workspace.Layer
	for _, candidate := range layers {
		published := publishedFeatureTypeName(candidate.PublicID, appPrefix)
		qn := ParseQName(published)
		sanitized := appPrefix + ":" + sanitizeXMLName(candidate.PublicID)
		if candidate.PublicID != typeName && published != typeName && sanitized != typeName && qn.LocalPart != typeName {
			continue
		}
		if matched != nil && matched.PublicID != candidate.PublicID {
			return nil
		}
		matched = candidate
	}
	return matched
}
