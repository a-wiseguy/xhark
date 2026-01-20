package ui

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jroimartin/gocui"

	"xhark/internal/httpclient"
	"xhark/internal/model"
	"xhark/internal/openapi"
)

var debugLog *log.Logger

func init() {
	// enable debug logging with XHARK_DEBUG=1
	if os.Getenv("XHARK_DEBUG") == "1" {
		f, err := os.OpenFile("/tmp/xhark.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err == nil {
			debugLog = log.New(f, "", log.LstdFlags)
			return
		}
	}
	debugLog = log.New(io.Discard, "", 0)
}

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

// singleLineEditor is an editor that doesn't consume Enter (lets keybinding handle it)
type singleLineEditor struct{}

func (e singleLineEditor) Edit(v *gocui.View, key gocui.Key, ch rune, mod gocui.Modifier) {
	switch {
	case key == gocui.KeyBackspace || key == gocui.KeyBackspace2:
		v.EditDelete(true)
	case key == gocui.KeyDelete:
		v.EditDelete(false)
	case key == gocui.KeyArrowLeft:
		v.MoveCursor(-1, 0, false)
	case key == gocui.KeyArrowRight:
		v.MoveCursor(1, 0, false)
	case key == gocui.KeyHome || key == gocui.KeyCtrlA:
		v.SetCursor(0, 0)
	case key == gocui.KeyEnd || key == gocui.KeyCtrlE:
		line := v.Buffer()
		v.SetCursor(len(line)-1, 0)
	case key == gocui.KeyEnter:
		// don't handle - let keybinding process it
	case ch != 0 && mod == 0:
		v.EditWrite(ch)
	}
}

func (a *App) Run() error {
	g, err := gocui.NewGui(gocui.OutputNormal)
	if err != nil {
		return err
	}
	defer g.Close()
	a.g = g

	// Set dark theme colors
	g.BgColor = gocui.ColorBlack
	g.FgColor = gocui.ColorWhite

	// check env for base url - skip prompt if set
	if envURL := os.Getenv("XHARK_BASE_URL"); envURL != "" {
		a.baseURL = envURL
		if !strings.HasPrefix(a.baseURL, "http://") && !strings.HasPrefix(a.baseURL, "https://") {
			a.baseURL = "http://" + a.baseURL
		}
		// load endpoints immediately
		if err := a.loadEndpoints(); err != nil {
			a.errorMsg = "error: " + err.Error()
		} else {
			a.scr = screenEndpoints
		}
	}

	g.Cursor = true
	g.InputEsc = true
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
		v.BgColor = gocui.ColorBlack
		v.FgColor = gocui.ColorWhite
		fmt.Fprintln(v, colorGreen+"xhark"+colorReset+"  -  FastAPI OpenAPI TUI")
	}

	if v, err := g.SetView("footer", 0, maxY-2, maxX-1, maxY); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Frame = false
		v.BgColor = gocui.ColorBlack
		v.FgColor = gocui.ColorWhite
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
		v.Editable = false
	}
	a.renderPrompt()
	if _, err := a.g.SetCurrentView("prompt"); err != nil {
		return err
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
	}
	a.renderFilter()
	a.renderEndpoints()
	if _, err := a.g.SetCurrentView("endpoints"); err != nil {
		return err
	}
	return nil
}

func (a *App) layoutBuilder(maxX, maxY int) error {
	// determine which panels to show
	hasPath := len(a.activeEndpoint.PathParams) > 0
	hasQuery := len(a.activeEndpoint.QueryParams) > 0
	hasBody := a.activeEndpoint.Body != nil

	// build list of panels to display
	var panels []string
	if hasPath {
		panels = append(panels, "path")
	}
	if hasQuery {
		panels = append(panels, "query")
	}
	if hasBody {
		panels = append(panels, "body")
	}

	// fallback: show at least path panel with "(none)" message
	if len(panels) == 0 {
		panels = []string{"path"}
		hasPath = true
	}

	// clear views that won't be shown, but keep edit modal if active
	keepViews := make([]string, len(panels))
	copy(keepViews, panels)
	keepViews = append(keepViews, "selected")
	if a.editing {
		keepViews = append(keepViews, "edit")
	}
	a.clearMainViews(keepViews)

	// ensure current pane is valid
	a.ensureValidPane(hasPath, hasQuery, hasBody)

	// add selected endpoint panel at top
	if v, err := a.g.SetView("selected", 0, 2, maxX-1, 4); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Title = "Selected endpoint"
	}

	bodyTop := 4
	paramsBottom := maxY - 3
	panelHeight := (paramsBottom - bodyTop) / len(panels)

	for i, panel := range panels {
		y0 := bodyTop + i*panelHeight
		y1 := bodyTop + (i+1)*panelHeight
		if i == len(panels)-1 {
			y1 = paramsBottom
		}

		if v, err := a.g.SetView(panel, 0, y0, maxX-1, y1); err != nil {
			if err != gocui.ErrUnknownView {
				return err
			}
			v.Highlight = true
		}
	}

	a.renderBuilder()
	a.updatePanelColors()

	// if editing, ensure edit view is on top and focused
	if a.editing {
		if v, err := a.g.View("edit"); err == nil {
			a.g.SetViewOnTop("edit")
			a.g.SetCurrentView("edit")
			_ = v // avoid unused warning
		}
	} else {
		a.setBuilderFocus()
	}
	return nil
}

func (a *App) ensureValidPane(hasPath, hasQuery, hasBody bool) {
	// if current pane doesn't exist, switch to first valid one
	switch a.pane {
	case panePath:
		if !hasPath {
			if hasQuery {
				a.pane = paneQuery
			} else if hasBody {
				a.pane = paneBody
			}
		}
	case paneQuery:
		if !hasQuery {
			if hasPath {
				a.pane = panePath
			} else if hasBody {
				a.pane = paneBody
			}
		}
	case paneBody:
		if !hasBody {
			if hasPath {
				a.pane = panePath
			} else if hasQuery {
				a.pane = paneQuery
			}
		}
	}
}

func (a *App) updatePanelColors() {
	// set colors: focused pane gets green highlight, others get muted
	panels := []struct {
		name string
		pane focusPane
	}{
		{"path", panePath},
		{"query", paneQuery},
		{"body", paneBody},
	}

	for _, p := range panels {
		v, err := a.g.View(p.name)
		if err != nil {
			continue
		}
		if a.pane == p.pane && !a.editing {
			v.SelBgColor = gocui.ColorGreen
			v.SelFgColor = gocui.ColorBlack
			v.FgColor = gocui.ColorWhite
		} else {
			v.SelBgColor = gocui.ColorDefault
			v.SelFgColor = gocui.ColorDefault
			v.FgColor = gocui.ColorDefault
		}
	}
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

	for _, n := range []string{"prompt", "filter", "endpoints", "selected", "path", "query", "body", "edit", "response"} {
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

	// base url (handled by the prompt's custom Editor)

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
	// number shortcuts 1-5 for quick endpoint selection
	for i := 1; i <= 5; i++ {
		if err := g.SetKeybinding("endpoints", rune('0'+i), gocui.ModNone, a.selectEndpointByNumber(i)); err != nil {
			return err
		}
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
	if err := g.SetKeybinding("path", 'd', gocui.ModNone, a.resetParam); err != nil {
		return err
	}
	if err := g.SetKeybinding("query", 'd', gocui.ModNone, a.resetParam); err != nil {
		return err
	}
	if err := g.SetKeybinding("body", 'd', gocui.ModNone, a.resetParam); err != nil {
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
	if err := g.SetKeybinding("response", gocui.KeyEnter, gocui.ModNone, a.responseToEndpoints); err != nil {
		return err
	}

	// global typing for endpoint filter (bind printable ASCII)
	for r := rune(32); r <= rune(126); r++ {
		if err := g.SetKeybinding("endpoints", r, gocui.ModNone, a.appendFilterRune(r)); err != nil {
			return err
		}
	}

	// base URL input: bind printable ASCII and Enter
	for r := rune(32); r <= rune(126); r++ {
		if err := g.SetKeybinding("prompt", r, gocui.ModNone, a.appendToBaseURL(r)); err != nil {
			return err
		}
	}
	if err := g.SetKeybinding("prompt", gocui.KeyBackspace, gocui.ModNone, a.backspaceBaseURL); err != nil {
		return err
	}
	if err := g.SetKeybinding("prompt", gocui.KeyBackspace2, gocui.ModNone, a.backspaceBaseURL); err != nil {
		return err
	}
	if err := g.SetKeybinding("prompt", gocui.KeyEnter, gocui.ModNone, a.submitBaseURL); err != nil {
		return err
	}
	if err := g.SetKeybinding("prompt", gocui.KeyCtrlL, gocui.ModNone, a.submitBaseURL); err != nil {
		return err
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
	raw := strings.TrimSpace(a.baseURL)
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

	if err := a.loadEndpoints(); err != nil {
		a.errorMsg = err.Error()
		return nil
	}

	a.scr = screenEndpoints
	a.errorMsg = ""
	return nil
}

func (a *App) loadEndpoints() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	doc, err := openapi.Load(ctx, a.baseURL)
	if err != nil {
		return err
	}
	a.endpoints = openapi.ExtractEndpoints(doc)
	a.filter = ""
	a.selected = 0
	a.recomputeFilter()
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

func (a *App) selectEndpointByNumber(num int) func(*gocui.Gui, *gocui.View) error {
	return func(g *gocui.Gui, v *gocui.View) error {
		if a.scr != screenEndpoints {
			return nil
		}
		idx := num - 1 // convert 1-based to 0-based
		if idx < 0 || idx >= len(a.filtered) {
			return nil
		}
		a.selected = idx
		return a.openBuilder(g, v)
	}
}

func (a *App) responseToEndpoints(*gocui.Gui, *gocui.View) error {
	if a.scr != screenResponse {
		return nil
	}
	a.scr = screenEndpoints
	a.errorMsg = ""
	return nil
}

func (a *App) tabPane(*gocui.Gui, *gocui.View) error {
	if a.scr != screenBuilder || a.editing {
		return nil
	}

	// get available panes
	hasPath := len(a.activeEndpoint.PathParams) > 0
	hasQuery := len(a.activeEndpoint.QueryParams) > 0
	hasBody := a.activeEndpoint.Body != nil

	// cycle to next available pane
	for i := 0; i < 3; i++ {
		switch a.pane {
		case panePath:
			a.pane = paneQuery
		case paneQuery:
			a.pane = paneBody
		case paneBody:
			a.pane = panePath
		}

		// check if new pane is valid
		switch a.pane {
		case panePath:
			if hasPath {
				goto done
			}
		case paneQuery:
			if hasQuery {
				goto done
			}
		case paneBody:
			if hasBody {
				goto done
			}
		}
	}
done:
	a.updatePanelColors()
	a.setBuilderFocus()
	return nil
}

func (a *App) setBuilderFocus() {
	if a.scr != screenBuilder || a.editing {
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
	a.g.SetCurrentView(name)
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

func (a *App) resetParam(*gocui.Gui, *gocui.View) error {
	if a.scr != screenBuilder || a.editing {
		return nil
	}
	paneName := ""
	switch a.pane {
	case panePath:
		paneName = "path"
	case paneQuery:
		paneName = "query"
	case paneBody:
		paneName = "body"
	}
	if paneName == "" {
		return nil
	}
	v, err := a.g.View(paneName)
	if err != nil {
		return nil
	}
	key := a.selectedKey(paneName, v)
	if key == "" {
		return nil
	}
	switch a.pane {
	case panePath:
		delete(a.pathVals, key)
	case paneQuery:
		delete(a.queryVals, key)
	case paneBody:
		delete(a.bodyVals, key)
	}
	a.renderBuilder()
	return nil
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

		// centered modal dialog
		maxX, maxY := g.Size()
		width := 60
		if width > maxX-4 {
			width = maxX - 4
		}
		height := 3
		x0 := (maxX - width) / 2
		y0 := (maxY - height) / 2
		x1 := x0 + width
		y1 := y0 + height

		if ev, err := g.SetView("edit", x0, y0, x1, y1); err != nil {
			if err != gocui.ErrUnknownView {
				return err
			}
			ev.Title = fmt.Sprintf(" %s (enter=ok, esc=cancel) ", key)
			ev.Editable = true
			ev.Editor = singleLineEditor{}
			ev.BgColor = gocui.ColorBlack
			ev.FgColor = gocui.ColorWhite
		}
		// always update content and focus (in case view already existed)
		if ev, err := g.View("edit"); err == nil {
			ev.Clear()
			currentVal := a.currentValueFor(key, viewName)
			fmt.Fprint(ev, currentVal)
			ev.SetCursor(len(currentVal), 0)
		}
		g.SetCurrentView("edit")
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
				msg = "type: filter   1-5: quick select   enter: select   esc: back   q: quit"
			case screenBuilder:
				msg = "tab: switch pane   enter: edit   d: reset param   ctrl+r: run   esc: back"
			case screenResponse:
				msg = "up/down: scroll   r: rerun   enter: back to endpoints   esc: back"
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

func (a *App) renderPrompt() {
	v, err := a.g.View("prompt")
	if err != nil {
		return
	}
	v.Clear()
	fmt.Fprintf(v, "%s", a.baseURL)
	// position cursor at end
	v.SetCursor(len(a.baseURL), 0)
}

func (a *App) appendToBaseURL(r rune) func(*gocui.Gui, *gocui.View) error {
	return func(g *gocui.Gui, v *gocui.View) error {
		a.baseURL += string(r)
		a.renderPrompt()
		return nil
	}
}

func (a *App) backspaceBaseURL(*gocui.Gui, *gocui.View) error {
	if len(a.baseURL) > 0 {
		a.baseURL = a.baseURL[:len(a.baseURL)-1]
		a.renderPrompt()
	}
	return nil
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

	for i, idx := range a.filtered {
		ep := a.endpoints[idx]
		label := firstNonEmpty(ep.Summary, ep.OperationID)
		if label != "" {
			label = " - " + label
		}
		// show number prefix for top 5 results
		prefix := "  "
		if i < 5 {
			prefix = fmt.Sprintf("%d ", i+1)
		}
		fmt.Fprintf(v, "%s%s  %s%s\n", prefix, colorizeMethod(ep.Method), highlightPathParams(ep.Path), label)
	}
	v.SetCursor(0, a.selected)
}

// ansi colors
const (
	colorDim     = "\033[90m" // gray for placeholder examples
	colorReset   = "\033[0m"
	colorRed     = "\033[31m"
	colorGreen   = "\033[32m"
	colorYellow  = "\033[33m"
	colorBlue    = "\033[34m"
	colorMagenta = "\033[35m"
	colorCyan    = "\033[36m"
	colorWhite   = "\033[37m"
)

func (a *App) renderBuilder() {
	a.renderFooter()

	if v, err := a.g.View("selected"); err == nil {
		v.Clear()
		label := firstNonEmpty(a.activeEndpoint.Summary, a.activeEndpoint.OperationID)
		if label != "" {
			label = " - " + label
		}
		fmt.Fprintf(v, "%s  %s%s\n", colorizeMethod(a.activeEndpoint.Method), highlightPathParams(a.activeEndpoint.Path), label)
	}

	if v, err := a.g.View("path"); err == nil {
		v.Title = "Path Params"
		v.Clear()
		for _, p := range a.activeEndpoint.PathParams {
			val := a.pathVals[p.Name]
			req := ""
			if p.Required {
				req = "*"
			}
			if val == "" && p.Example != "" {
				// show example as placeholder
				fmt.Fprintf(v, "%s%s = %s%s%s\n", req, p.Name, colorDim, p.Example, colorReset)
			} else {
				fmt.Fprintf(v, "%s%s = %s\n", req, p.Name, val)
			}
		}
		if len(a.activeEndpoint.PathParams) == 0 {
			fmt.Fprintln(v, "(none)")
		}
	}

	if v, err := a.g.View("query"); err == nil {
		v.Title = "Query Params"
		v.Clear()
		for _, p := range a.activeEndpoint.QueryParams {
			val := a.queryVals[p.Name]
			req := ""
			if p.Required {
				req = "*"
			}
			var display string
			var color string
			if val != "" {
				display = val
				color = colorGreen
			} else {
				var hint string
				var parts []string
				if len(p.Enum) > 0 {
					parts = append(parts, strings.Join(p.Enum, "|"))
				}
				if p.Default != "" {
					parts = append(parts, "default: "+p.Default)
				}
				if p.Description != "" {
					parts = append(parts, p.Description)
				}
				hint = strings.Join(parts, ", ")
				if hint != "" {
					display = hint
				} else if p.Example != "" {
					display = p.Example
				}
				color = colorCyan
			}
			if display != "" {
				fmt.Fprintf(v, "%s%s = %s%s%s\n", req, p.Name, color, display, colorReset)
			} else {
				fmt.Fprintf(v, "%s%s = \n", req, p.Name)
			}
		}
		if len(a.activeEndpoint.QueryParams) == 0 {
			fmt.Fprintln(v, "(none)")
		}
	}

	if v, err := a.g.View("body"); err == nil {
		v.Title = "Body"
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
			if val == "" && f.Example != "" {
				fmt.Fprintf(v, "%s%s = %s%s%s\n", req, f.Name, colorDim, f.Example, colorReset)
			} else {
				fmt.Fprintf(v, "%s%s = %s\n", req, f.Name, val)
			}
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
	fmt.Fprintf(v, "%s\n", colorizeStatus(r.Status))
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

func colorizeMethod(method string) string {
	var color string
	switch strings.ToUpper(method) {
	case "GET":
		color = colorBlue
	case "POST":
		color = colorGreen
	case "PUT":
		color = colorYellow
	case "DELETE":
		color = colorRed
	case "PATCH":
		color = colorCyan
	case "HEAD":
		color = colorMagenta
	default:
		color = colorReset
	}
	return color + padRight(method, 6) + colorReset
}

func colorizeStatus(status string) string {
	parts := strings.Fields(status)
	if len(parts) == 0 {
		return status
	}
	codeStr := parts[0]
	code, err := strconv.Atoi(codeStr)
	if err != nil {
		return status
	}
	var color string
	if code >= 200 && code < 300 {
		color = colorGreen
	} else if code >= 400 && code < 500 {
		color = colorYellow
	} else if code >= 500 {
		color = colorRed
	} else {
		color = colorReset
	}
	return color + status + colorReset
}

func highlightPathParams(path string) string {
	re := regexp.MustCompile(`\{([^}]+)\}`)
	return re.ReplaceAllString(path, colorCyan+"{$1}"+colorReset)
}
