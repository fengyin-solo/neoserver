package wms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tobilg/neoserver/internal/datasource"
	"github.com/tobilg/neoserver/internal/identity"
	"github.com/tobilg/neoserver/internal/sld"
	"github.com/tobilg/neoserver/internal/store"
	"github.com/tobilg/neoserver/internal/workspace"
)

// legendVectorSource is a minimal vector source used by group legend tests.
type legendVectorSource struct {
	geomType string
	fail     bool
}

func (*legendVectorSource) Type() store.ServiceType      { return store.ServiceTypePostGIS }
func (*legendVectorSource) ID() string                   { return "legend-vector" }
func (*legendVectorSource) Health(context.Context) error { return nil }
func (*legendVectorSource) Close() error                 { return nil }
func (*legendVectorSource) DiscoverLayers(context.Context) ([]*datasource.DiscoveredLayer, error) {
	return nil, nil
}
func (*legendVectorSource) Query(context.Context, string, datasource.QueryParams) ([]json.RawMessage, error) {
	return nil, nil
}
func (*legendVectorSource) QueryWKB(context.Context, string, datasource.QueryParams) ([]datasource.RenderFeature, error) {
	return nil, nil
}
func (*legendVectorSource) QueryByID(context.Context, string, string, int) (json.RawMessage, bool, error) {
	return nil, false, nil
}
func (*legendVectorSource) Count(context.Context, string, datasource.QueryParams) (int, error) {
	return 0, nil
}
func (s *legendVectorSource) GetLayerInfo(context.Context, string) (*datasource.LayerInfo, error) {
	if s.fail {
		return nil, errors.New("legend source unavailable")
	}
	return &datasource.LayerInfo{Name: "layer", GeometryType: s.geomType, SRID: 4326}, nil
}

func polygonLegendSLD(hex string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<StyledLayerDescriptor version="1.1.0"><NamedLayer><Name>generic</Name><UserStyle><Name>s</Name><IsDefault>true</IsDefault><FeatureTypeStyle><Rule><PolygonSymbolizer><Fill><CssParameter name="fill">%[1]s</CssParameter><CssParameter name="fill-opacity">1.0</CssParameter></Fill><Stroke><CssParameter name="stroke">%[1]s</CssParameter><CssParameter name="stroke-width">1</CssParameter></Stroke></PolygonSymbolizer></Rule></FeatureTypeStyle></UserStyle></NamedLayer></StyledLayerDescriptor>`, hex)
}

func pointLegendSLD(hex string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<StyledLayerDescriptor version="1.1.0"><NamedLayer><Name>generic</Name><UserStyle><Name>s</Name><IsDefault>true</IsDefault><FeatureTypeStyle><Rule><PointSymbolizer><Graphic><Mark><WellKnownName>circle</WellKnownName><Fill><CssParameter name="fill">%[1]s</CssParameter><CssParameter name="fill-opacity">1.0</CssParameter></Fill></Mark></Graphic></PointSymbolizer></Rule></FeatureTypeStyle></UserStyle></NamedLayer></StyledLayerDescriptor>`, hex)
}

func lineLegendSLD(hex string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<StyledLayerDescriptor version="1.1.0"><NamedLayer><Name>generic</Name><UserStyle><Name>s</Name><IsDefault>true</IsDefault><FeatureTypeStyle><Rule><LineSymbolizer><Stroke><CssParameter name="stroke">%[1]s</CssParameter><CssParameter name="stroke-width">2</CssParameter><CssParameter name="stroke-opacity">1.0</CssParameter></Stroke></LineSymbolizer></Rule></FeatureTypeStyle></UserStyle></NamedLayer></StyledLayerDescriptor>`, hex)
}

// groupLegendFixture builds a workspace with polygon/point/line layers, a
// raster coverage, solid-colour stored styles and an invalid style.
func groupLegendFixture(t *testing.T) (*workspaceHandler, *workspace.Workspace) {
	t.Helper()
	h, ws := rasterWMSFixture()
	ws.Groups = map[string]*workspace.LayerGroup{}

	addStyle := func(name, body string) {
		doc, err := sld.ParseString(body)
		if err != nil {
			t.Fatalf("style %s: %v", name, err)
		}
		ws.Styles[name] = &workspace.Style{Name: name, Format: sld.FormatSLD110, Document: doc, Valid: true}
	}
	addStyle("red", polygonLegendSLD("#ff0000"))
	addStyle("green", polygonLegendSLD("#00ff00"))
	addStyle("blue", polygonLegendSLD("#0000ff"))
	addStyle("point-red", pointLegendSLD("#ff0000"))
	addStyle("line-green", lineLegendSLD("#00ff00"))
	ws.Styles["broken-style"] = &workspace.Style{Name: "broken-style", Format: sld.FormatSLD110, Valid: false, ValidationErrors: []string{"style is invalid"}}

	addVector := func(publicID, geom, defaultStyle string, public bool, roles []string, fail bool) {
		source := &legendVectorSource{geomType: geom, fail: fail}
		layer := &workspace.Layer{
			ID: publicID, PublicID: publicID, SourceLayer: publicID, Title: publicID,
			Enabled: true, Public: public, AllowedRoles: roles, DefaultStyle: defaultStyle, CRSDefault: 4326,
		}
		service := &workspace.Service{ID: "svc-" + publicID, Enabled: true, DataSource: source, Layers: map[string]*workspace.Layer{publicID: layer}}
		ws.Services["svc-"+publicID] = service
	}
	addVector("parks", "Polygon", "red", true, nil, false)
	addVector("woods", "Polygon", "green", true, nil, false)
	addVector("lakes", "Polygon", "blue", true, nil, false)
	addVector("cities", "Point", "point-red", true, nil, false)
	addVector("roads", "LineString", "line-green", true, nil, false)
	addVector("secret", "Polygon", "green", false, []string{"admin"}, false)
	addVector("broken", "Polygon", "red", true, nil, true)
	return h, ws
}

func groupMember(resource string, style ...string) store.LayerGroupMember {
	member := store.LayerGroupMember{Resource: resource}
	if len(style) > 0 {
		member.Style = style[0]
	}
	return member
}

func legendGroup(publicID string, public bool, roles []string, members ...store.LayerGroupMember) *workspace.LayerGroup {
	return &workspace.LayerGroup{
		ID: publicID, PublicID: publicID, Enabled: true, Public: public,
		AllowedRoles: roles, Members: members,
	}
}

func requestGroupLegend(h *workspaceHandler, ws *workspace.Workspace, layer string, roles map[string]string, extra string) *httptest.ResponseRecorder {
	if extra != "" {
		extra = "&" + extra
	}
	request := httptest.NewRequest(http.MethodGet,
		"/wms?SERVICE=WMS&VERSION=1.3.0&REQUEST=GetLegendGraphic&FORMAT=image/png&LAYER="+url.QueryEscape(layer)+extra, nil)
	if roles != nil {
		ctx := identity.WithIdentity(request.Context(), &identity.Identity{Roles: roles})
		request = request.WithContext(ctx)
	}
	recorder := httptest.NewRecorder()
	h.handleGetLegendGraphic(recorder, request, ws)
	return recorder
}

func decodeLegendPNG(t *testing.T, recorder *httptest.ResponseRecorder) image.Image {
	t.Helper()
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != FormatPNG {
		t.Fatalf("expected a PNG legend, got %d %s: %s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
	img, err := png.Decode(recorder.Body)
	if err != nil {
		t.Fatalf("decoding legend PNG: %v", err)
	}
	return img
}

func rgbAt(img image.Image, x, y int) (int, int, int) {
	r, g, b, _ := img.At(x, y).RGBA()
	return int(r >> 8), int(g >> 8), int(b >> 8)
}

func expectColor(t *testing.T, img image.Image, x, y int, want [3]int, label string) {
	t.Helper()
	expectColorTolerance(t, img, x, y, want, 24, label)
}

func expectColorTolerance(t *testing.T, img image.Image, x, y int, want [3]int, tolerance int, label string) {
	t.Helper()
	r, g, b := rgbAt(img, x, y)
	if absInt(r-want[0]) > tolerance || absInt(g-want[1]) > tolerance || absInt(b-want[2]) > tolerance {
		t.Errorf("%s at (%d,%d) = RGB(%d,%d,%d), want %v", label, x, y, r, g, b, want)
	}
}

func imageHasColor(img image.Image, want [3]int, tolerance int) bool {
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b := rgbAt(img, x, y)
			if absInt(r-want[0]) <= tolerance && absInt(g-want[1]) <= tolerance && absInt(b-want[2]) <= tolerance {
				return true
			}
		}
	}
	return false
}

var (
	redRGB   = [3]int{255, 0, 0}
	greenRGB = [3]int{0, 255, 0}
	blueRGB  = [3]int{0, 0, 255}
)

// A public group returns one merged PNG with one equal-height vertical slice
// per member, in declared top-to-bottom order.
func TestGetLegendGraphicPublicGroupMergesMembers(t *testing.T) {
	h, ws := groupLegendFixture(t)
	ws.Groups["stack"] = legendGroup("stack", true, nil,
		groupMember("parks"), groupMember("woods"), groupMember("lakes"))

	img := decodeLegendPNG(t, requestGroupLegend(h, ws, "stack", nil, "WIDTH=30&HEIGHT=30"))
	bounds := img.Bounds()
	if bounds.Dx() != 30 || bounds.Dy() != 30 {
		t.Fatalf("legend bounds = %v, want 30x30", bounds)
	}
	// Declared order is parks(red), woods(green), lakes(blue); alphabetical
	// order would be blue/red/green, so this also pins the ordering rule.
	expectColor(t, img, 5, 3, redRGB, "member 1 slice")
	expectColor(t, img, 5, 13, greenRGB, "member 2 slice")
	expectColor(t, img, 5, 23, blueRGB, "member 3 slice")
}

// A group the caller may not open stays undefined and neither the group type
// nor member names are disclosed; an authorized role gets the merged legend.
func TestGetLegendGraphicPrivateGroupStaysUndefined(t *testing.T) {
	h, ws := groupLegendFixture(t)
	ws.Groups["classified"] = legendGroup("classified", false, []string{"admin"}, groupMember("parks"))

	denied := requestGroupLegend(h, ws, "classified", nil, "WIDTH=30&HEIGHT=30")
	if denied.Code != http.StatusNotFound || !strings.Contains(denied.Body.String(), `code="LayerNotDefined"`) {
		t.Fatalf("anonymous private group response = %d %s", denied.Code, denied.Body.String())
	}
	if body := denied.Body.String(); strings.Contains(body, "layer group") || strings.Contains(body, "parks") {
		t.Fatalf("private group disclosed details: %s", body)
	}

	viewer := requestGroupLegend(h, ws, "classified", map[string]string{ws.ID: "viewer"}, "WIDTH=30&HEIGHT=30")
	if viewer.Code != http.StatusNotFound {
		t.Fatalf("unrelated role got %d: %s", viewer.Code, viewer.Body.String())
	}

	admin := requestGroupLegend(h, ws, "classified", map[string]string{ws.ID: "admin"}, "WIDTH=30&HEIGHT=30")
	img := decodeLegendPNG(t, admin)
	expectColor(t, img, 5, 15, redRGB, "admin member slice")
}

// Members the role cannot read are simply omitted: their symbols must not
// appear anywhere in the merged image.
func TestGetLegendGraphicGroupFiltersInvisibleMembers(t *testing.T) {
	h, ws := groupLegendFixture(t)
	ws.Groups["mixed"] = legendGroup("mixed", true, nil,
		groupMember("parks"), groupMember("secret"))

	anonymous := requestGroupLegend(h, ws, "mixed", nil, "WIDTH=30&HEIGHT=30")
	img := decodeLegendPNG(t, anonymous)
	// Only parks is visible: its slice covers the whole image.
	expectColor(t, img, 15, 15, redRGB, "visible member")
	if imageHasColor(img, greenRGB, 24) {
		t.Fatal("restricted member's green symbol leaked into the anonymous legend")
	}

	admin := requestGroupLegend(h, ws, "mixed", map[string]string{ws.ID: "admin"}, "WIDTH=30&HEIGHT=30")
	img = decodeLegendPNG(t, admin)
	expectColor(t, img, 5, 7, redRGB, "admin first slice")
	expectColor(t, img, 5, 22, greenRGB, "admin second slice")

	// A public group whose members are all hidden returns a blank graphic
	// rather than disclosing anything.
	ws.Groups["all-hidden"] = legendGroup("all-hidden", true, nil, groupMember("secret"))
	blank := decodeLegendPNG(t, requestGroupLegend(h, ws, "all-hidden", nil, "WIDTH=30&HEIGHT=30"))
	if imageHasColor(blank, greenRGB, 24) || imageHasColor(blank, redRGB, 24) || imageHasColor(blank, blueRGB, 24) {
		t.Fatal("blank group legend leaked member symbols")
	}
}

// Nested groups expand in place, depth-first, following declared member order.
func TestGetLegendGraphicNestedGroupExpandsInOrder(t *testing.T) {
	h, ws := groupLegendFixture(t)
	ws.Groups["inner"] = legendGroup("inner", true, nil,
		groupMember("woods"), groupMember("lakes"))
	ws.Groups["outer"] = legendGroup("outer", true, nil,
		groupMember("parks"), groupMember("inner"))

	img := decodeLegendPNG(t, requestGroupLegend(h, ws, "outer", nil, "WIDTH=30&HEIGHT=30"))
	expectColor(t, img, 5, 3, redRGB, "outer first member")
	expectColor(t, img, 5, 13, greenRGB, "nested first member")
	expectColor(t, img, 5, 23, blueRGB, "nested second member")
}

// An inaccessible nested group is skipped together with its whole subtree.
func TestGetLegendGraphicSkipsInaccessibleNestedGroup(t *testing.T) {
	h, ws := groupLegendFixture(t)
	ws.Groups["hidden-inner"] = legendGroup("hidden-inner", false, []string{"admin"},
		groupMember("woods"), groupMember("lakes"))
	ws.Groups["outer"] = legendGroup("outer", true, nil,
		groupMember("parks"), groupMember("hidden-inner"))

	img := decodeLegendPNG(t, requestGroupLegend(h, ws, "outer", nil, "WIDTH=30&HEIGHT=30"))
	// Anonymous sees only parks: one equal slice over the whole image.
	expectColor(t, img, 15, 15, redRGB, "visible outer member")
	if imageHasColor(img, greenRGB, 24) || imageHasColor(img, blueRGB, 24) {
		t.Fatal("members of a hidden nested group leaked into the legend")
	}

	admin := requestGroupLegend(h, ws, "outer", map[string]string{ws.ID: "admin"}, "WIDTH=30&HEIGHT=30")
	img = decodeLegendPNG(t, admin)
	expectColor(t, img, 5, 3, redRGB, "admin outer slice")
	expectColor(t, img, 5, 13, greenRGB, "admin nested slice")
}

// The same resource+style reached through several group paths is drawn once;
// the same resource with different styles is drawn for each style.
func TestGetLegendGraphicDeduplicatesRepeatedResourceStyle(t *testing.T) {
	h, ws := groupLegendFixture(t)
	ws.Groups["shared"] = legendGroup("shared", true, nil, groupMember("parks"))
	ws.Groups["dup"] = legendGroup("dup", true, nil,
		groupMember("parks"), groupMember("shared"), groupMember("parks"))

	img := decodeLegendPNG(t, requestGroupLegend(h, ws, "dup", nil, "WIDTH=30&HEIGHT=30"))
	// parks/red occurs three times but must render once: the full image is red.
	expectColor(t, img, 5, 3, redRGB, "deduplicated slice top")
	expectColor(t, img, 5, 15, redRGB, "deduplicated slice middle")
	expectColor(t, img, 5, 27, redRGB, "deduplicated slice bottom")
	if imageHasColor(img, greenRGB, 24) || imageHasColor(img, blueRGB, 24) {
		t.Fatal("unexpected symbols from de-duplicated members")
	}

	// Same resource with two distinct styles is rendered twice, in order.
	ws.Groups["two-styles"] = legendGroup("two-styles", true, nil,
		groupMember("parks", "red"), groupMember("parks", "blue"))
	img = decodeLegendPNG(t, requestGroupLegend(h, ws, "two-styles", nil, "WIDTH=30&HEIGHT=30"))
	expectColor(t, img, 5, 7, redRGB, "red style slice")
	expectColor(t, img, 5, 22, blueRGB, "blue style slice")
}

// WIDTH/HEIGHT remain bounds for the whole merged image and the server's
// maximum pixel and maximum dimension limits keep applying.
func TestGetLegendGraphicGroupEnforcesImageLimits(t *testing.T) {
	h, ws := groupLegendFixture(t)
	ws.Groups["stack"] = legendGroup("stack", true, nil,
		groupMember("parks"), groupMember("woods"))

	// Dimension codes pre-date the group legend and map to 500, like single
	// layer legends; the rejection itself is what must keep working.
	for _, tc := range []struct {
		name, query, code string
	}{
		{"width", "WIDTH=2048&HEIGHT=10", `code="InvalidWidth"`},
		{"height", "WIDTH=10&HEIGHT=2048", `code="InvalidHeight"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := requestGroupLegend(h, ws, "stack", nil, tc.query)
			if rec.Header().Get("Content-Type") != "text/xml" || !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("limit response = %d %s", rec.Code, rec.Body.String())
			}
		})
	}

	// Dimensions exactly at the configured limit are accepted.
	rec := requestGroupLegend(h, ws, "stack", nil, "WIDTH=1024&HEIGHT=1024")
	img := decodeLegendPNG(t, rec)
	if img.Bounds().Dx() != 1024 || img.Bounds().Dy() != 1024 {
		t.Fatalf("bounds = %v", img.Bounds())
	}

	// The configured maximum pixel count still bounds the whole result.
	limited, limitedWS := groupLegendFixture(t)
	limited.cfg.WMS.MaxPixels = 100
	limitedWS.Groups["stack"] = legendGroup("stack", true, nil,
		groupMember("parks"), groupMember("woods"))
	over := requestGroupLegend(limited, limitedWS, "stack", nil, "WIDTH=20&HEIGHT=20")
	if over.Code != http.StatusBadRequest || !strings.Contains(over.Body.String(), "maximum pixel count") {
		t.Fatalf("pixel limit response = %d %s", over.Code, over.Body.String())
	}
}

// A failing member data source or an unresolvable member style produces a
// service exception locating the member (also through nested group paths), and
// never a partial image.
func TestGetLegendGraphicGroupMemberFailures(t *testing.T) {
	h, ws := groupLegendFixture(t)
	ws.Groups["risky"] = legendGroup("risky", true, nil,
		groupMember("parks"), groupMember("broken"))

	rec := requestGroupLegend(h, ws, "risky", nil, "WIDTH=30&HEIGHT=30")
	body := html.UnescapeString(rec.Body.String())
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Content-Type") != "text/xml" ||
		!strings.Contains(body, `code="InvalidParameterValue"`) ||
		!strings.Contains(body, `layer group "risky" member "risky/broken"`) ||
		!strings.Contains(body, "legend source unavailable") {
		t.Fatalf("data-source failure response = %d %s", rec.Code, body)
	}
	if strings.Contains(rec.Header().Get("Content-Type"), "image/png") {
		t.Fatal("member failure must not return image bytes")
	}

	// Style failure for a member, located through a nested group.
	ws.Groups["inner-fail"] = legendGroup("inner-fail", true, nil,
		groupMember("parks", "broken-style"))
	ws.Groups["outer-fail"] = legendGroup("outer-fail", true, nil,
		groupMember("lakes"), groupMember("inner-fail"))
	rec = requestGroupLegend(h, ws, "outer-fail", nil, "WIDTH=30&HEIGHT=30")
	body = html.UnescapeString(rec.Body.String())
	if rec.Code != http.StatusNotFound || !strings.Contains(body, `code="StyleNotDefined"`) ||
		!strings.Contains(body, `layer group "outer-fail" member "outer-fail/inner-fail/parks"`) {
		t.Fatalf("style failure response = %d %s", rec.Code, body)
	}
}

// Point, line and polygon members keep the existing per-geometry symbol rules
// inside the merged image; raster members render their colour strip as a slice.
func TestGetLegendGraphicGroupPreservesSymbolRulesAndRaster(t *testing.T) {
	h, ws := groupLegendFixture(t)
	ws.Groups["shapes"] = legendGroup("shapes", true, nil,
		groupMember("cities"), groupMember("roads"), groupMember("lakes"))

	img := decodeLegendPNG(t, requestGroupLegend(h, ws, "shapes", nil, "WIDTH=60&HEIGHT=60"))
	// Point circle is centred in its 20px slice.
	expectColor(t, img, 30, 10, redRGB, "point symbol")
	// Diagonal line crosses an integer pixel in the second slice; the
	// rasterised stroke edge is anti-aliased.
	expectColorTolerance(t, img, 16, 34, greenRGB, 64, "line symbol")
	// Polygon fills its slice.
	expectColor(t, img, 5, 45, blueRGB, "polygon symbol")

	// A raster member renders into its own slice below a feature member.
	ws.Groups["terrain"] = legendGroup("terrain", true, nil,
		groupMember("parks"), groupMember("elevation"))
	img = decodeLegendPNG(t, requestGroupLegend(h, ws, "terrain", nil, "WIDTH=40&HEIGHT=40"))
	expectColor(t, img, 5, 5, redRGB, "feature slice above raster")
	// The raster strip starts black at its left edge and is not the white
	// background nor the feature member's red.
	r, g, b := rgbAt(img, 2, 30)
	if !(r < 80 && g < 80 && b < 80) {
		t.Fatalf("raster slice start = RGB(%d,%d,%d), want a dark colour strip", r, g, b)
	}
}
