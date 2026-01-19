package ui

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jroimartin/gocui"

	"xhark/internal/httpclient"
	"xhark/internal/model"
	"xhark/internal/openapi"
)

type screen int

const (
	screenBaseURL screen = iota
	screenEndpoints
	screenBuilder
	screenResponse
)

type focusPane int

const (
	panePath focusPane = iota
	paneQuery
	paneBody
)

type App struct {
	in  io.Reader
	out io.Writer

	g *gocui.Gui

	scr screen

	baseURL   string
	endpoints []model.Endpoint

	filter   string
	filtered []int
	selected int

	activeEndpoint model.Endpoint
	pathVals       map[string]string
	queryVals      map[string]string
	bodyVals       map[string]string

	pane focusPane

	editing    bool
	editTarget string

	lastReq  httpclient.RequestSpec
	lastRes  httpclient.Result
	errorMsg string
}

func NewApp(in io.Reader, out io.Writer) *App {
	return &App{in: in, out: out, scr: screenBaseURL}
}

func (a *App) Run() error {
	g, err := gocui.NewGui(gocui.OutputNormal)
	if err != nil {
		return err
	}
	defer g.Close()
	a.g = g

	g.Cursor = true
	g.SetManagerFunc(a.layout)

	if err := a.bindKeys(); err != nil {
		return err
	}

	if err := g.MainLoop(); err != nil && err != gocui.ErrQuit {
		return err
	}
	return nil
}

func (a *App) layout(g *gocui.Gui) error {
	maxX, maxY := g.Size()

	if v, err := g.SetView("header", 0, 0, maxX-1, 2); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Frame = false
		fmt.Fprintln(v, "xhark  -  FastAPI OpenAPI TUI")
	}

	if v, err := g.SetView("footer", 0, maxY-2, maxX-1, maxY-1); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Frame = false
	}
	a.renderFooter()

	switch a.scr {
	case screenBaseURL:
		return a.layoutBaseURL(maxX, maxY)
	case screenEndpoints:
		return a.layoutEndpoints(maxX, maxY)
	case screenBuilder:
		return a.layoutBuilder(maxX, maxY)
	case screenResponse:
		return a.layoutResponse(maxX, maxY)
	default:
		return nil
	}
}

func (a *App) layoutBaseURL(maxX, maxY int) error {
	a.clearMainViews([]string{"prompt"})

	if v, err := a.g.SetView("prompt", 0, 2, maxX-1, 6); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Title = "Base URL"
		v.Editable = true
		v.Editor = gocui.DefaultEditor
		fmt.Fprint(v, a.baseURL)
		if _, err := a.g.SetCurrentView("prompt"); err != nil {
			return err
		}
	}

	return nil
}

func (a *App) layoutEndpoints(maxX, maxY int) error {
	a.clearMainViews([]string{"filter", "endpoints"})

	if v, err := a.g.SetView("filter", 0, 2, maxX-1, 4); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Title = "Filter"
		v.Editable = false
	}
	if v, err := a.g.SetView("endpoints", 0, 4, maxX-1, maxY-3); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Title = "Endpoints"
		v.Highlight = true
		v.SelFgColor = gocui.ColorBlack
		v.SelBgColor = gocui.ColorGreen
		v.Autoscroll = false
		if _, err := a.g.SetCurrentView("endpoints"); err != nil {
			return err
		}
		a.recomputeFilter()
		a.renderEndpoints()
	}
	a.renderFilter()
	return nil
}

func (a *App) layoutBuilder(maxX, maxY int) error {
	a.clearMainViews([]string{"path", "query", "body"})

	midX := maxX / 2
	bodyTop := 2
	paramsBottom := maxY - 3

	if v, err := a.g.SetView("path", 0, bodyTop, midX-1, (bodyTop+paramsBottom)/2); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Title = "Path Params"
		v.Highlight = true
		v.SelFgColor = gocui.ColorBlack
		v.SelBgColor = gocui.ColorGreen
	}
	if v, err := a.g.SetView("query", 0, (bodyTop+paramsBottom)/2, midX-1, paramsBottom); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Title = "Query Params"
		v.Highlight = true
		v.SelFgColor = gocui.ColorBlack
		v.SelBgColor = gocui.ColorGreen
	}
	if v, err := a.g.SetView("body", midX, bodyTop, maxX-1, paramsBottom); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Title = "Body"
		v.Highlight = true
		v.SelFgColor = gocui.ColorBlack
		v.SelBgColor = gocui.ColorGreen
	}

	a.renderBuilder()
	a.setBuilderFocus()
	return nil
}

func (a *App) layoutResponse(maxX, maxY int) error {
	a.clearMainViews([]string{"response"})

	if v, err := a.g.SetView("response", 0, 2, maxX-1, maxY-3); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Title = "Response"
		v.Wrap = false
		v.Autoscroll = false
	}
	a.renderResponse()
	if _, err := a.g.SetCurrentView("response"); err != nil {
		return err
	}
	return nil
}

func (a *App) clearMainViews(keep []string) {
	keepSet := map[string]bool{"header": true, "footer": true}
	for _, k := range keep {
		keepSet[k] = true
	}

	for _, n := range []string{"prompt", "filter", "endpoints", "path", "query", "body", "edit", "response"} {
		if keepSet[n] {
			continue
		}
		if v, err := a.g.View(n); err == nil {
			v.Clear()
			a.g.DeleteView(n)
		}
	}
}

func (a *App) bindKeys() error {
	g := a.g
	if err := g.SetKeybinding("", 'q', gocui.ModNone, a.quit); err != nil {
		return err
	}
	if err := g.SetKeybinding("", gocui.KeyEsc, gocui.ModNone, a.back); err != nil {
		return err
	}

	// base url
	if err := g.SetKeybinding("prompt", gocui.KeyEnter, gocui.ModNone, a.submitBaseURL); err != nil {
		return err
	}

	// endpoints list
	if err := g.SetKeybinding("endpoints", gocui.KeyArrowDown, gocui.ModNone, a.moveSel(1)); err != nil {
		return err
	}
	if err := g.SetKeybinding("endpoints", gocui.KeyArrowUp, gocui.ModNone, a.moveSel(-1)); err != nil {
		return err
	}
	if err := g.SetKeybinding("endpoints", gocui.KeyEnter, gocui.ModNone, a.openBuilder); err != nil {
		return err
	}
	if err := g.SetKeybinding("endpoints", gocui.KeyBackspace, gocui.ModNone, a.filterBackspace); err != nil {
		return err
	}
	if err := g.SetKeybinding("endpoints", gocui.KeyBackspace2, gocui.ModNone, a.filterBackspace); err != nil {
		return err
	}

	// builder
	if err := g.SetKeybinding("", gocui.KeyTab, gocui.ModNone, a.tabPane); err != nil {
		return err
	}
	if err := g.SetKeybinding("path", gocui.KeyArrowDown, gocui.ModNone, a.moveRow("path", 1)); err != nil {
		return err
	}
	if err := g.SetKeybinding("path", gocui.KeyArrowUp, gocui.ModNone, a.moveRow("path", -1)); err != nil {
		return err
	}
	if err := g.SetKeybinding("query", gocui.KeyArrowDown, gocui.ModNone, a.moveRow("query", 1)); err != nil {
		return err
	}
	if err := g.SetKeybinding("query", gocui.KeyArrowUp, gocui.ModNone, a.moveRow("query", -1)); err != nil {
		return err
	}
	if err := g.SetKeybinding("body", gocui.KeyArrowDown, gocui.ModNone, a.moveRow("body", 1)); err != nil {
		return err
	}
	if err := g.SetKeybinding("body", gocui.KeyArrowUp, gocui.ModNone, a.moveRow("body", -1)); err != nil {
		return err
	}
	if err := g.SetKeybinding("path", gocui.KeyEnter, gocui.ModNone, a.beginEdit("path")); err != nil {
		return err
	}
	if err := g.SetKeybinding("query", gocui.KeyEnter, gocui.ModNone, a.beginEdit("query")); err != nil {
		return err
	}
	if err := g.SetKeybinding("body", gocui.KeyEnter, gocui.ModNone, a.beginEdit("body")); err != nil {
		return err
	}
	if err := g.SetKeybinding("", gocui.KeyCtrlR, gocui.ModNone, a.executeRequest); err != nil {
		return err
	}

	// edit modal
	if err := g.SetKeybinding("edit", gocui.KeyEnter, gocui.ModNone, a.confirmEdit); err != nil {
		return err
	}

	// response
	if err := g.SetKeybinding("response", gocui.KeyArrowDown, gocui.ModNone, a.scrollResponse(1)); err != nil {
		return err
	}
	if err := g.SetKeybinding("response", gocui.KeyArrowUp, gocui.ModNone, a.scrollResponse(-1)); err != nil {
		return err
	}
	if err := g.SetKeybinding("response", 'r', gocui.ModNone, a.rerun); err != nil {
		return err
	}

	// global typing for endpoint filter (bind printable ASCII)
	for r := rune(32); r <= rune(126); r++ {
		if err := g.SetKeybinding("endpoints", r, gocui.ModNone, a.appendFilterRune(r)); err != nil {
			return err
		}
	}

	return nil
}

func (a *App) quit(*gocui.Gui, *gocui.View) error { return gocui.ErrQuit }

func (a *App) back(*gocui.Gui, *gocui.View) error {
	if a.editing {
		return a.closeEdit()
	}
	switch a.scr {
	case screenResponse:
		a.scr = screenBuilder
	case screenBuilder:
		a.scr = screenEndpoints
	case screenEndpoints:
		a.scr = screenBaseURL
	}
	a.errorMsg = ""
	return nil
}

func (a *App) submitBaseURL(g *gocui.Gui, v *gocui.View) error {
	raw := strings.TrimSpace(viewText(v))
	if raw == "" {
		a.errorMsg = "base URL required"
		return nil
	}
	norm := normalizeBaseURL(raw)
	if _, err := url.ParseRequestURI(norm); err != nil {
		a.errorMsg = "invalid base URL"
		return nil
	}

	a.baseURL = norm
	a.errorMsg = "loading openapi..."
	a.renderFooter()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	doc, err := openapi.Load(ctx, a.baseURL)
	if err != nil {
		a.errorMsg = err.Error()
		return nil
	}
	endpoints := openapi.ExtractEndpoints(doc)
	a.endpoints = endpoints
	a.filter = ""
	a.selected = 0
	a.recomputeFilter()

	a.scr = screenEndpoints
	a.errorMsg = ""
	return nil
}

func normalizeBaseURL(in string) string {
	if strings.HasPrefix(in, "http://") || strings.HasPrefix(in, "https://") {
		return in
	}
	return "http://" + in
}

func (a *App) captureRune(g *gocui.Gui, v *gocui.View) error {
	if a.scr != screenEndpoints {
		return nil
	}
	if a.editing {
		return nil
	}
	_ = g
	_ = v
	return nil
}

func (a *App) appendFilterRune(r rune) func(*gocui.Gui, *gocui.View) error {
	return func(g *gocui.Gui, v *gocui.View) error {
		if a.scr != screenEndpoints || a.editing {
			return nil
		}
		if v != nil && v.Name() == "prompt" {
			return nil
		}
		a.filter += string(r)
		a.recomputeFilter()
		a.renderFilter()
		a.renderEndpoints()
		return nil
	}
}

func (a *App) filterBackspace(*gocui.Gui, *gocui.View) error {
	if a.scr != screenEndpoints || a.editing {
		return nil
	}
	if len(a.filter) == 0 {
		return nil
	}
	a.filter = a.filter[:len(a.filter)-1]
	a.recomputeFilter()
	a.renderFilter()
	a.renderEndpoints()
	return nil
}

func (a *App) moveSel(delta int) func(*gocui.Gui, *gocui.View) error {
	return func(g *gocui.Gui, v *gocui.View) error {
		if a.scr != screenEndpoints {
			return nil
		}
		if len(a.filtered) == 0 {
			return nil
		}
		a.selected += delta
		if a.selected < 0 {
			a.selected = 0
		}
		if a.selected >= len(a.filtered) {
			a.selected = len(a.filtered) - 1
		}
		if ev, err := a.g.View("endpoints"); err == nil {
			ev.SetCursor(0, a.selected)
		}
		return nil
	}
}

func (a *App) openBuilder(*gocui.Gui, *gocui.View) error {
	if a.scr != screenEndpoints {
		return nil
	}
	if len(a.filtered) == 0 {
		return nil
	}
	idx := a.filtered[a.selected]
	a.activeEndpoint = a.endpoints[idx]
	a.pathVals = map[string]string{}
	a.queryVals = map[string]string{}
	a.bodyVals = map[string]string{}
	a.pane = panePath
	a.scr = screenBuilder
	a.errorMsg = ""
	return nil
}

func (a *App) tabPane(*gocui.Gui, *gocui.View) error {
	if a.scr != screenBuilder || a.editing {
		return nil
	}
	if a.pane == panePath {
		a.pane = paneQuery
	} else if a.pane == paneQuery {
		a.pane = paneBody
	} else {
		a.pane = panePath
	}
	a.setBuilderFocus()
	return nil
}

func (a *App) setBuilderFocus() {
	if a.scr != screenBuilder {
		return
	}
	name := "path"
	switch a.pane {
	case panePath:
		name = "path"
	case paneQuery:
		name = "query"
	case paneBody:
		name = "body"
	}
	_, _ = a.g.SetCurrentView(name)
}

func (a *App) moveRow(viewName string, delta int) func(*gocui.Gui, *gocui.View) error {
	return func(g *gocui.Gui, v *gocui.View) error {
		if a.scr != screenBuilder || a.editing {
			return nil
		}
		if v == nil {
			return nil
		}
		ox, oy := v.Origin()
		cx, cy := v.Cursor()
		newY := cy + delta
		if newY < 0 {
			if oy > 0 {
				v.SetOrigin(ox, oy-1)
			}
			return nil
		}

		lines := viewLines(v)
		absY := oy + newY
		if absY >= len(lines) {
			return nil
		}
		v.SetCursor(cx, newY)
		return nil
	}
}

func (a *App) beginEdit(viewName string) func(*gocui.Gui, *gocui.View) error {
	return func(g *gocui.Gui, v *gocui.View) error {
		if a.scr != screenBuilder || a.editing {
			return nil
		}
		if v == nil {
			return nil
		}

		key := a.selectedKey(viewName, v)
		if key == "" {
			return nil
		}

		a.editing = true
		a.editTarget = viewName + ":" + key

		maxX, maxY := g.Size()
		x0, y0 := maxX/6, maxY/3
		x1, y1 := maxX*5/6, maxY/3+4
		if ev, err := g.SetView("edit", x0, y0, x1, y1); err != nil {
			if err != gocui.ErrUnknownView {
				return err
			}
			ev.Title = "Edit"
			ev.Editable = true
			ev.Editor = gocui.DefaultEditor
			ev.Clear()
			fmt.Fprint(ev, a.currentValueFor(key, viewName))
			_, _ = g.SetCurrentView("edit")
		}
		return nil
	}
}

func (a *App) closeEdit() error {
	if !a.editing {
		return nil
	}
	if v, err := a.g.View("edit"); err == nil {
		v.Clear()
		a.g.DeleteView("edit")
	}
	a.editing = false
	a.editTarget = ""
	a.setBuilderFocus()
	return nil
}

func (a *App) confirmEdit(g *gocui.Gui, v *gocui.View) error {
	if !a.editing {
		return nil
	}
	val := strings.TrimSpace(viewText(v))
	parts := strings.SplitN(a.editTarget, ":", 2)
	if len(parts) != 2 {
		return a.closeEdit()
	}
	pane := parts[0]
	key := parts[1]

	switch pane {
	case "path":
		a.pathVals[key] = val
	case "query":
		a.queryVals[key] = val
	case "body":
		a.bodyVals[key] = val
	}

	a.closeEdit()
	a.renderBuilder()
	return nil
}

func (a *App) executeRequest(*gocui.Gui, *gocui.View) error {
	if a.scr != screenBuilder || a.editing {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := httpclient.BuildRequest(a.baseURL, a.activeEndpoint, a.pathVals, a.queryVals, a.bodyVals)
	if err != nil {
		a.errorMsg = err.Error()
		return nil
	}
	res, err := httpclient.Execute(ctx, req)
	if err != nil {
		a.errorMsg = err.Error()
		return nil
	}
	_ = req

	a.lastReq = req
	a.lastRes = res
	a.scr = screenResponse
	a.errorMsg = ""
	return nil
}

func (a *App) rerun(*gocui.Gui, *gocui.View) error {
	if a.scr != screenResponse {
		return nil
	}
	if a.lastReq.URL == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := httpclient.Execute(ctx, a.lastReq)
	if err != nil {
		a.errorMsg = err.Error()
		return nil
	}
	a.lastRes = res
	a.renderResponse()
	return nil
}

func (a *App) scrollResponse(delta int) func(*gocui.Gui, *gocui.View) error {
	return func(g *gocui.Gui, v *gocui.View) error {
		if a.scr != screenResponse || v == nil {
			return nil
		}
		ox, oy := v.Origin()
		if delta > 0 {
			v.SetOrigin(ox, oy+1)
		} else if oy > 0 {
			v.SetOrigin(ox, oy-1)
		}
		return nil
	}
}

func (a *App) renderFooter() {
	if v, err := a.g.View("footer"); err == nil {
		v.Clear()
		msg := a.errorMsg
		if msg == "" {
			switch a.scr {
			case screenBaseURL:
				msg = "enter: load openapi   q: quit"
			case screenEndpoints:
				msg = "type: filter   enter: select   esc: back   q: quit"
			case screenBuilder:
				msg = "tab: switch pane   enter: edit   ctrl+r: run   esc: back"
			case screenResponse:
				msg = "up/down: scroll   r: rerun   esc: back"
			}
		}
		fmt.Fprint(v, msg)
	}
}

func (a *App) renderFilter() {
	v, err := a.g.View("filter")
	if err != nil {
		return
	}
	v.Clear()
	fmt.Fprintf(v, "%s", a.filter)
}

func (a *App) recomputeFilter() {
	needle := strings.TrimSpace(a.filter)
	if needle == "" {
		a.filtered = a.filtered[:0]
		for i := range a.endpoints {
			a.filtered = append(a.filtered, i)
		}
		return
	}

	var scored []scoredIdx
	for i, ep := range a.endpoints {
		cand := strings.ToLower(ep.Method + " " + ep.Path + " " + firstNonEmpty(ep.Summary, ep.OperationID))
		if s, ok := fuzzyMatchScore(needle, cand); ok {
			scored = append(scored, scoredIdx{idx: i, score: s})
		}
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score == scored[j].score {
			return scored[i].idx < scored[j].idx
		}
		return scored[i].score < scored[j].score
	})
	a.filtered = a.filtered[:0]
	for _, s := range scored {
		a.filtered = append(a.filtered, s.idx)
	}
	if a.selected >= len(a.filtered) {
		a.selected = 0
	}
}

func (a *App) renderEndpoints() {
	v, err := a.g.View("endpoints")
	if err != nil {
		return
	}
	v.Clear()

	for _, idx := range a.filtered {
		ep := a.endpoints[idx]
		label := firstNonEmpty(ep.Summary, ep.OperationID)
		if label != "" {
			label = " - " + label
		}
		fmt.Fprintf(v, "%s  %s%s\n", padRight(ep.Method, 6), ep.Path, label)
	}
	v.SetCursor(0, a.selected)
}

func (a *App) renderBuilder() {
	a.renderFooter()

	if v, err := a.g.View("path"); err == nil {
		v.Clear()
		for _, p := range a.activeEndpoint.PathParams {
			val := a.pathVals[p.Name]
			req := ""
			if p.Required {
				req = "*"
			}
			fmt.Fprintf(v, "%s%s = %s\n", req, p.Name, val)
		}
		if len(a.activeEndpoint.PathParams) == 0 {
			fmt.Fprintln(v, "(none)")
		}
	}

	if v, err := a.g.View("query"); err == nil {
		v.Clear()
		for _, p := range a.activeEndpoint.QueryParams {
			val := a.queryVals[p.Name]
			req := ""
			if p.Required {
				req = "*"
			}
			fmt.Fprintf(v, "%s%s = %s\n", req, p.Name, val)
		}
		if len(a.activeEndpoint.QueryParams) == 0 {
			fmt.Fprintln(v, "(none)")
		}
	}

	if v, err := a.g.View("body"); err == nil {
		v.Clear()
		if a.activeEndpoint.Body == nil {
			fmt.Fprintln(v, "(no body)")
			return
		}
		if !a.activeEndpoint.Body.Supported {
			fmt.Fprintln(v, "(body schema unsupported in MVP)")
			return
		}
		for _, f := range a.activeEndpoint.Body.Fields {
			val := a.bodyVals[f.Name]
			req := ""
			if f.Required {
				req = "*"
			}
			fmt.Fprintf(v, "%s%s = %s\n", req, f.Name, val)
		}
		if len(a.activeEndpoint.Body.Fields) == 0 {
			fmt.Fprintln(v, "(empty schema)")
		}
	}
}

func (a *App) renderResponse() {
	a.renderFooter()
	v, err := a.g.View("response")
	if err != nil {
		return
	}
	v.Clear()

	r := a.lastRes
	fmt.Fprintf(v, "%s\n", r.Status)
	fmt.Fprintf(v, "elapsed: %s\n", r.Elapsed)
	if ct, ok := r.Headers["content-type"]; ok {
		fmt.Fprintf(v, "content-type: %s\n", ct)
	}
	fmt.Fprintln(v, "")
	fmt.Fprintln(v, r.Body)
}

func (a *App) selectedKey(viewName string, v *gocui.View) string {
	lines := viewLines(v)
	_, cy := v.Cursor()
	_, oy := v.Origin()
	i := oy + cy
	if i < 0 || i >= len(lines) {
		return ""
	}
	line := strings.TrimSpace(lines[i])
	if line == "" || strings.HasPrefix(line, "(") {
		return ""
	}
	// line format: "*name = value" or "name = value"
	line = strings.TrimPrefix(line, "*")
	parts := strings.SplitN(line, "=", 2)
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func (a *App) currentValueFor(key, pane string) string {
	switch pane {
	case "path":
		return a.pathVals[key]
	case "query":
		return a.queryVals[key]
	case "body":
		return a.bodyVals[key]
	default:
		return ""
	}
}

func viewText(v *gocui.View) string {
	b := v.Buffer()
	// gocui includes a trailing newline
	return strings.TrimSuffix(b, "\n")
}

func viewLines(v *gocui.View) []string {
	buf := strings.TrimSuffix(v.Buffer(), "\n")
	if buf == "" {
		return nil
	}
	return strings.Split(buf, "\n")
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
