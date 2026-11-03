package wms

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"

	"github.com/fogleman/gg"
	"github.com/tobilg/neoserver/internal/conf"
	"github.com/tobilg/neoserver/internal/datasource"
	"github.com/tobilg/neoserver/internal/renderer"
	"github.com/tobilg/neoserver/internal/sld"
	"github.com/tobilg/neoserver/internal/workspace"
)

const remoteSLDUnsupportedMessage = "SLD URL parameter is not supported; use SLD_BODY or a stored style"

func (h *workspaceHandler) handleGetLegendGraphic(w http.ResponseWriter, r *http.Request, ws *workspace.Workspace) {
	ctx := r.Context()

	// Parse request
	maxEnvVariables, maxEnvValueBytes := h.environmentLimits()
	req, err := parseGetLegendGraphicRequestWithLimits(r, h.cfg.WMS.MaxWidth, h.cfg.WMS.MaxHeight, h.cfg.WMS.MaxPixels, maxEnvVariables, maxEnvValueBytes)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if strings.TrimSpace(req.SLD) != "" {
		WriteExceptionWithLocator(w, ExceptionInvalidParameterValue, "SLD", remoteSLDUnsupportedMessage)
		return
	}

	// Find layer and service. A layer the caller may not read is treated as
	// undefined so its existence is not disclosed.
	resource := ws.GetResource(req.Layer)
	role := workspaceRole(r, ws.ID)
	if resource != nil && resource.Kind == workspace.ResourceGroup {
		h.handleGroupGetLegendGraphic(w, r, ws, resource.Group, req, role)
		return
	}
	if resource == nil || resource.Service == nil ||
		(resource.Layer != nil && !resource.Layer.VisibleToRole(workspaceRole(r, ws.ID))) ||
		(resource.Coverage != nil && !resource.Coverage.VisibleToRole(workspaceRole(r, ws.ID))) {
		WriteException(w, ExceptionLayerNotDefined, fmt.Sprintf("Layer not found: %s", req.Layer))
		return
	}
	if resource.Kind == workspace.ResourceCoverage {
		var doc *sld.StyledLayerDescriptor
		if req.SLDBody != "" {
			doc, err = sld.ParseString(req.SLDBody)
			if err != nil {
				WriteException(w, ExceptionStyleNotDefined, "Invalid SLD")
				return
			}
		}
		style, err := h.resolveWMSStyle(ws, resource, doc, req.Style)
		if err != nil {
			WriteException(w, ExceptionStyleNotDefined, err.Error())
			return
		}
		rasterStyle, err := h.resolveRasterLegendStyle(ws, style, req.Environment)
		if err != nil {
			WriteException(w, ExceptionStyleNotDefined, err.Error())
			return
		}

		w.Header().Set("Content-Type", FormatPNG)
		w.WriteHeader(http.StatusOK)
		_ = png.Encode(w, renderer.RasterLegend(rasterStyle, req.Width, req.Height))
		return
	}
	layer, service := resource.Layer, resource.Service

	if service.DataSource == nil {
		WriteException(w, ExceptionLayerNotDefined, fmt.Sprintf("Service not available for layer: %s", req.Layer))
		return
	}

	// Get layer info for geometry type
	layerInfo, err := layer.FeatureInfo(ctx, service.DataSource)
	if err != nil {
		WriteException(w, ExceptionInvalidParameterValue, fmt.Sprintf("Failed to get layer info: %v", err))
		return
	}

	// Get style
	var style *sld.Style
	if req.SLDBody != "" {
		sldDoc, err := sld.ParseString(req.SLDBody)
		if err != nil {
			WriteException(w, ExceptionStyleNotDefined, fmt.Sprintf("Invalid SLD: %v", err))
			return
		}
		style, _ = sldDoc.GetStyle(req.Layer, req.Style)
	}

	if style == nil && req.Style != "" && req.Style != "default" {
		// Try to load style from file
		stylePath, pathErr := safeStylePath(h.cfg.WMS.SLDPath, req.Style)
		if sldFile, err := sld.ParseFile(stylePath); pathErr == nil && err == nil {
			style, _ = sldFile.GetDefaultStyle()
		}
	}

	if style == nil {
		// Use default style
		defaultStyleCfg := h.cfg.WMS.Styles[h.cfg.WMS.DefaultStyle]
		if defaultStyleCfg.FillColor == "" {
			defaultStyleCfg = conf.WMSStyle{
				FillColor:   "#3388ff",
				FillOpacity: 0.5,
				StrokeColor: "#3388ff",
				StrokeWidth: 2.0,
				PointRadius: 5.0,
			}
		}
		style = sld.DefaultStyle(defaultStyleCfg)
	}

	// Generate legend image
	dc := h.generateLegend(style, layerInfo, req.Width, req.Height)

	// Write response
	w.Header().Set("Content-Type", FormatPNG)
	w.WriteHeader(http.StatusOK)
	png.Encode(w, dc.Image())
}

// legendGeometryType classifies a layer for legend symbol rendering using the
// same point/line/polygon rules as map rendering.
func legendGeometryType(layerInfo *datasource.LayerInfo) string {
	geomTypeLower := strings.ToLower(layerInfo.GeometryType)
	switch {
	case strings.Contains(geomTypeLower, "point"):
		return "Point"
	case strings.Contains(geomTypeLower, "line"):
		return "LineString"
	default:
		return "Polygon"
	}
}

// generateLegend generates a legend image for the given style.
func (h *workspaceHandler) generateLegend(style *sld.Style, layerInfo *datasource.LayerInfo, symbolWidth, symbolHeight int) *gg.Context {
	// Determine geometry type for symbol rendering
	geomType := legendGeometryType(layerInfo)

	// Create image with exact requested dimensions (required for WMS 1.3.0 compliance)
	dc := gg.NewContext(symbolWidth, symbolHeight)

	// White background
	dc.SetRGB(1, 1, 1)
	dc.Clear()

	rules := style.Rules
	if len(rules) == 0 {
		return dc
	}

	// Draw the first rule's symbol to fit the requested dimensions
	h.drawLegendSymbol(dc, 0, 0, float64(symbolWidth), float64(symbolHeight), geomType, &rules[0])

	return dc
}

// drawLegendSymbol draws a legend symbol.
func (h *workspaceHandler) drawLegendSymbol(dc *gg.Context, x, y, width, height float64, geomType string, rule *sld.ResolvedRule) {
	switch geomType {
	case "Point", "MultiPoint":
		h.drawPointLegend(dc, x, y, width, height, rule.PointStyle)
	case "LineString", "MultiLineString":
		h.drawLineLegend(dc, x, y, width, height, rule.LineStyle)
	case "Polygon", "MultiPolygon":
		h.drawPolygonLegend(dc, x, y, width, height, rule.PolygonStyle)
	default:
		h.drawPolygonLegend(dc, x, y, width, height, rule.PolygonStyle)
	}
}

// drawPointLegend draws a point legend symbol.
func (h *workspaceHandler) drawPointLegend(dc *gg.Context, x, y, width, height float64, style *sld.PointStyle) {
	if style == nil {
		style = sld.DefaultPointStyle()
	}

	cx := x + width/2
	cy := y + height/2
	radius := minFloat(width, height) / 2 * 0.8

	switch style.Shape {
	case "square":
		dc.DrawRectangle(cx-radius, cy-radius, radius*2, radius*2)
	case "triangle":
		dc.MoveTo(cx, cy-radius)
		dc.LineTo(cx+radius*0.866, cy+radius*0.5)
		dc.LineTo(cx-radius*0.866, cy+radius*0.5)
		dc.ClosePath()
	default: // circle
		dc.DrawCircle(cx, cy, radius)
	}

	setLegendColor(dc, style.FillColor, style.Opacity)
	dc.FillPreserve()
	setLegendColor(dc, style.StrokeColor, 1.0)
	dc.SetLineWidth(style.StrokeWidth)
	dc.Stroke()
}

// drawLineLegend draws a line legend symbol.
func (h *workspaceHandler) drawLineLegend(dc *gg.Context, x, y, width, height float64, style *sld.LineStyle) {
	if style == nil {
		style = sld.DefaultLineStyle()
	}

	// Draw a diagonal line
	dc.MoveTo(x+2, y+height-2)
	dc.LineTo(x+width-2, y+2)

	setLegendColor(dc, style.Color, style.Opacity)
	dc.SetLineWidth(style.Width)

	if len(style.DashArray) > 0 {
		dc.SetDash(style.DashArray...)
	}

	dc.Stroke()
	dc.SetDash()
}

// drawPolygonLegend draws a polygon legend symbol.
func (h *workspaceHandler) drawPolygonLegend(dc *gg.Context, x, y, width, height float64, style *sld.PolygonStyle) {
	if style == nil {
		style = sld.DefaultPolygonStyle()
	}

	// Draw a rectangle
	dc.DrawRectangle(x+2, y+2, width-4, height-4)

	setLegendColor(dc, style.FillColor, style.FillOpacity)
	dc.FillPreserve()

	if style.StrokeWidth > 0 {
		setLegendColor(dc, style.StrokeColor, style.StrokeOpacity)
		dc.SetLineWidth(style.StrokeWidth)
		dc.Stroke()
	} else {
		dc.ClearPath()
	}
}

// setLegendColor sets the drawing color.
func setLegendColor(dc *gg.Context, c color.RGBA, opacity float64) {
	alpha := float64(c.A) / 255.0 * opacity
	dc.SetRGBA(float64(c.R)/255.0, float64(c.G)/255.0, float64(c.B)/255.0, alpha)
}

// minFloat returns the minimum of two floats.
func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// groupLegendError locates a failure within a layer group so the service
// exception can identify the offending member instead of failing the request
// anonymously.
type groupLegendError struct {
	code    string
	message string
}

func (e *groupLegendError) Error() string { return e.message }

// legendMember is one flattened, visible, de-duplicated group leaf in declared
// top-to-bottom order.
type legendMember struct {
	resource  *workspace.PublishedResource
	styleName string // effective stored/file style name; "" selects the resource default
	path      []string
}

func (h *workspaceHandler) handleGroupGetLegendGraphic(w http.ResponseWriter, r *http.Request, ws *workspace.Workspace, group *workspace.LayerGroup, req *GetLegendGraphicRequest, role string) {
	// A group the caller may not open is undefined, the same as a restricted
	// layer, so private groups are not disclosed.
	if !ws.GroupAccessibleToRole(group, role) {
		WriteException(w, ExceptionLayerNotDefined, fmt.Sprintf("Layer not found: %s", req.Layer))
		return
	}
	var inline *sld.StyledLayerDescriptor
	if req.SLDBody != "" {
		doc, err := sld.ParseString(req.SLDBody)
		if err != nil {
			WriteException(w, ExceptionStyleNotDefined, "Invalid SLD")
			return
		}
		inline = doc
	}
	members, err := h.expandLegendGroup(ws, group, req.Style, role)
	if err != nil {
		WriteExceptionWithLocator(w, ExceptionInvalidParameterValue, "LAYER", err.Error())
		return
	}
	// Every member's data and style is resolved before any response body is
	// written, so a member failure can never produce a partial legend image.
	img, err := h.renderGroupLegend(r.Context(), ws, members, inline, req.Environment, req.Width, req.Height)
	if err != nil {
		if failure, ok := err.(*groupLegendError); ok {
			WriteExceptionWithLocator(w, failure.code, "LAYER", failure.message)
			return
		}
		h.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", FormatPNG)
	w.WriteHeader(http.StatusOK)
	_ = png.Encode(w, img)
}

// expandLegendGroup flattens a group depth-first in declared member order,
// skipping resources the role may not read (including entire inaccessible
// sub-groups) and drawing any given resource+style combination only once even
// when it is reachable through several group paths. Style inheritance follows
// the same rules as GetMap group expansion: a requested style or group default
// overrides member-declared styles.
func (h *workspaceHandler) expandLegendGroup(ws *workspace.Workspace, root *workspace.LayerGroup, requestedStyle, role string) ([]legendMember, error) {
	maxDepth := h.cfg.WMS.MaxGroupDepth
	if maxDepth <= 0 {
		maxDepth = 8
	}
	members := make([]legendMember, 0)
	seen := make(map[string]struct{})
	visiting := make(map[string]bool)

	var expand func(group *workspace.LayerGroup, inheritedStyle string, path []string, depth int) error
	expand = func(group *workspace.LayerGroup, inheritedStyle string, path []string, depth int) error {
		if depth > maxDepth {
			return fmt.Errorf("layer group nesting exceeds %d", maxDepth)
		}
		if visiting[group.PublicID] {
			return fmt.Errorf("layer group cycle includes %q", group.PublicID)
		}
		visiting[group.PublicID] = true
		defer delete(visiting, group.PublicID)

		groupStyle := inheritedStyle
		if groupStyle == "" {
			groupStyle = group.DefaultStyle
		}
		for _, declared := range group.Members {
			memberPath := append(append([]string{}, path...), declared.Resource)
			resource := ws.GetResource(declared.Resource)
			if resource == nil {
				// Disabled or removed after publication: there is nothing visible to show.
				continue
			}
			styleName := declared.Style
			if groupStyle != "" {
				styleName = groupStyle
			}
			if resource.Kind == workspace.ResourceGroup {
				// Skip the whole subtree when the nested group itself is not
				// accessible; none of its member names or symbols may leak.
				if !ws.GroupAccessibleToRole(resource.Group, role) {
					continue
				}
				if err := expand(resource.Group, styleName, memberPath, depth+1); err != nil {
					return err
				}
				continue
			}
			visible := false
			switch resource.Kind {
			case workspace.ResourceFeature:
				visible = resource.Layer.VisibleToRole(role)
			case workspace.ResourceCoverage:
				visible = resource.Coverage.VisibleToRole(role)
			}
			if !visible {
				continue
			}
			key := resource.PublicID() + "\x00" + canonicalLegendStyle(resource, styleName)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			members = append(members, legendMember{resource: resource, styleName: styleName, path: memberPath})
		}
		return nil
	}

	if err := expand(root, requestedStyle, []string{root.PublicID}, 1); err != nil {
		return nil, err
	}
	return members, nil
}

// canonicalLegendStyle identifies the style that would actually be rendered for
// a member so default-style references de-duplicate against explicit ones.
func canonicalLegendStyle(resource *workspace.PublishedResource, styleName string) string {
	if styleName == "" || styleName == "default" {
		if resource.Layer != nil {
			return resource.Layer.DefaultStyle
		}
		if resource.Coverage != nil {
			return resource.Coverage.DefaultStyle
		}
	}
	return styleName
}

type preparedLegendMember struct {
	path     []string
	isRaster bool
	raster   *sld.RasterStyle
	geomType string
	rule     *sld.ResolvedRule
	hasRule  bool
}

// renderGroupLegend resolves every member first and then paints one merged PNG.
// WIDTH/HEIGHT are the bounds of the whole result; members share the height in
// equal vertical slices, in declared top-to-bottom order.
func (h *workspaceHandler) renderGroupLegend(ctx context.Context, ws *workspace.Workspace, members []legendMember, inline *sld.StyledLayerDescriptor, environment map[string]string, width, height int) (image.Image, error) {
	prepared := make([]preparedLegendMember, 0, len(members))
	for _, member := range members {
		ready, err := h.prepareLegendMember(ctx, ws, member, inline, environment)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, ready)
	}

	dc := gg.NewContext(width, height)
	dc.SetRGB(1, 1, 1)
	dc.Clear()
	count := len(prepared)
	for index, member := range prepared {
		top := index * height / count
		bottom := (index + 1) * height / count
		sliceHeight := bottom - top
		if sliceHeight <= 0 {
			continue
		}
		if member.isRaster {
			dc.DrawImage(renderer.RasterLegend(member.raster, width, sliceHeight), 0, top)
			continue
		}
		if member.hasRule {
			h.drawLegendSymbol(dc, 0, float64(top), float64(width), float64(sliceHeight), member.geomType, member.rule)
		}
	}
	return dc.Image(), nil
}

func (h *workspaceHandler) prepareLegendMember(ctx context.Context, ws *workspace.Workspace, member legendMember, inline *sld.StyledLayerDescriptor, environment map[string]string) (preparedLegendMember, error) {
	fail := func(code string, cause error) (preparedLegendMember, error) {
		groupName := ""
		location := strings.Join(member.path, "/")
		if len(member.path) > 0 {
			groupName = member.path[0]
		}
		return preparedLegendMember{path: member.path}, &groupLegendError{
			code:    code,
			message: fmt.Sprintf("layer group %q member %q: %v", groupName, location, cause),
		}
	}
	resource := member.resource
	style, err := h.resolveWMSStyle(ws, resource, inline, member.styleName)
	if err != nil {
		return fail(ExceptionStyleNotDefined, err)
	}
	if resource.Kind == workspace.ResourceCoverage {
		raster, err := h.resolveRasterLegendStyle(ws, style, environment)
		if err != nil {
			return fail(ExceptionStyleNotDefined, err)
		}
		return preparedLegendMember{path: member.path, isRaster: true, raster: raster}, nil
	}
	service := resource.Service
	if service == nil || service.DataSource == nil {
		return fail(ExceptionLayerNotDefined, fmt.Errorf("feature source is unavailable"))
	}
	layerInfo, err := resource.Layer.FeatureInfo(ctx, service.DataSource)
	if err != nil {
		return fail(ExceptionInvalidParameterValue, fmt.Errorf("failed to get layer info: %w", err))
	}
	if style == nil {
		defaults := h.cfg.WMS.Styles[h.cfg.WMS.DefaultStyle]
		if defaults.FillColor == "" {
			defaults = conf.WMSStyle{
				FillColor:   "#3388ff",
				FillOpacity: 0.5,
				StrokeColor: "#3388ff",
				StrokeWidth: 2.0,
				PointRadius: 5.0,
			}
		}
		style = sld.DefaultStyle(defaults)
	}
	prepared := preparedLegendMember{path: member.path, geomType: legendGeometryType(layerInfo)}
	if len(style.Rules) > 0 {
		prepared.hasRule = true
		prepared.rule = &style.Rules[0]
	}
	return prepared, nil
}
