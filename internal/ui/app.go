package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
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
	screenEndpoints screen = iota
	screenBuilder
	screenResponse
)

type focusPane int

const (
	panePath focusPane = iota
	paneQuery
	paneBody
)

type authState struct {
	schemeName string
	token      string
	tokenType  string
	acquiredAt time.Time
}

type authMode int

const (
	authModeToken authMode = iota
	authModeUser
	authModePass
	authModeScope
)

type App struct {
	in  io.Reader
	out io.Writer

	g *gocui.Gui

	scr screen

	specURL    string
	baseURL    string
	endpoints  []model.Endpoint
	secSchemes map[string]model.SecurityScheme

	filter   string
	filtered []int
	selected int

	activeEndpoint model.Endpoint
	pathVals       map[string]string
	queryVals      map[string]string
	bodyVals       map[string]string
	bodyRaw        string

	pane focusPane

	editing    bool
	editTarget string

	// Auth dialog state
	authOpen       bool
	authEditing    bool
	authSchemes    []string
	authSelected   int
	authActiveName string
	authMode       authMode
	authToken      string
	authUsername   string
	authPassword   string
	authScope      string
	authError      string
	authStore      map[string]authState

	suspendEditorFile string

	lastReq  httpclient.RequestSpec
	lastRes  httpclient.Result
	errorMsg string
}

func NewApp(in io.Reader, out io.Writer) *App {
	return &App{in: in, out: out, scr: screenEndpoints, authStore: map[string]authState{}}
}

func (a *App) SetSpec(spec string) {
	a.specURL = strings.TrimSpace(spec)
}

func (a *App) SetBaseURL(baseURL string) {
	a.baseURL = normalizeBaseURL(baseURL)
}

// Init loads the OpenAPI spec and prepares the endpoint list.
func (a *App) Init() error {
	if strings.TrimSpace(a.specURL) == "" {
		return fmt.Errorf("spec required (use --spec-url or --spec-file, or set XHARK_SPEC_URL/XHARK_SPEC_FILE)")
	}

	if strings.HasPrefix(a.specURL, "http://") || strings.HasPrefix(a.specURL, "https://") {
		a.baseURL = baseURLFromURLSpec(a.specURL)
	}

	return a.loadEndpoints()
}

// singleLineEditor is an editor that doesn't consume Enter (lets keybinding handle it)
type singleLineEditor struct{}

// passwordEditor masks typed characters but stores real buffer.
// It relies on the view buffer already containing the real value.
// (gocui doesn't support masked inputs natively; this is good-enough for now.)
type passwordEditor struct{}

func (e passwordEditor) Edit(v *gocui.View, key gocui.Key, ch rune, mod gocui.Modifier) {
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
	// We sometimes need to temporarily drop out of the TUI to run an external
	// process (e.g. $EDITOR for JSON body editing). gocui doesn't expose a native
	// suspend/resume API, so we exit the main loop, run the external command, and
	// then re-create the GUI.
	for {
		g, err := gocui.NewGui(gocui.OutputNormal)
		if err != nil {
			return err
		}
		a.g = g

		// Set dark theme colors
		g.BgColor = gocui.ColorBlack
		g.FgColor = gocui.ColorWhite

		g.Cursor = true
		g.InputEsc = true
		g.SetManagerFunc(a.layout)

		if err := a.bindKeys(); err != nil {
			g.Close()
			return err
		}

		err = g.MainLoop()
		g.Close()

		// If we asked to suspend into an external editor, do that and resume.
		if a.suspendEditorFile != "" {
			file := a.suspendEditorFile
			a.suspendEditorFile = ""
			if err := a.runExternalEditor(file); err != nil {
				a.errorMsg = err.Error()
			}
			// regardless of editor success, resume the app
			continue
		}

		if err != nil && err != gocui.ErrQuit {
			return err
		}
		return nil
	}
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
		fmt.Fprintln(v, colorGreen+"xhark"+colorReset+"  -  OpenAPI TUI")
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

	if a.authOpen {
		return a.layoutAuth(maxX, maxY)
	}

	switch a.scr {
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

func (a *App) layoutAuth(maxX, maxY int) error {
	// Centered auth modal
	width := maxX - 10
	if width > 100 {
		width = 100
	}
	height := 14
	if height > maxY-4 {
		height = maxY - 4
	}
	if width < 34 {
		width = 34
	}
	if height < 10 {
		height = 10
	}
	x0 := (maxX - width) / 2
	y0 := (maxY - height) / 2
	x1 := x0 + width
	y1 := y0 + height

	// keep underlying views, but ensure auth views exist and are on top
	if v, err := a.g.SetView("auth-box", x0, y0, x1, y1); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Title = "Authentication"
	}

	// split panels dynamically so small terminals still work
	leftW := 26
	if width < 60 {
		leftW = width / 3
	}
	if leftW < 12 {
		leftW = 12
	}
	// ensure right side has some space
	rightMin := 16
	maxLeft := (x1 - 2) - (x0 + 1) - rightMin - 1
	if leftW > maxLeft {
		leftW = maxLeft
	}
	if leftW < 12 {
		leftW = 12
	}
	schemesX0 := x0 + 1
	schemesX1 := x0 + leftW
	formX0 := schemesX1 + 1
	formX1 := x1 - 2

	if v, err := a.g.SetView("auth-schemes", schemesX0, y0+2, schemesX1, y1-2); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Title = "Schemes"
		v.Highlight = true
		v.SelFgColor = gocui.ColorBlack
		v.SelBgColor = gocui.ColorGreen
		v.Autoscroll = false
	}
	if v, err := a.g.SetView("auth-form", formX0, y0+2, formX1, y1-2); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Title = "Details"
		v.Editable = false
		v.Editor = singleLineEditor{}
	}

	// render
	a.renderAuth()

	// focus
	if a.authEditing {
		if _, err := a.g.SetCurrentView("auth-form"); err != nil {
			return err
		}
	} else {
		if _, err := a.g.SetCurrentView("auth-schemes"); err != nil {
			return err
		}
	}
	// z-order: box at back, then list/form on top
	_, _ = a.g.SetViewOnTop("auth-box")
	_, _ = a.g.SetViewOnTop("auth-schemes")
	_, _ = a.g.SetViewOnTop("auth-form")
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

	for _, n := range []string{"filter", "endpoints", "selected", "path", "query", "body", "edit", "response"} {
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
	// Global auth dialog hotkey (Shift+A)
	if err := g.SetKeybinding("", 'A', gocui.ModNone, a.openAuth); err != nil {
		return err
	}

	// spec url (handled by the prompt's custom Editor)

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
	if err := g.SetKeybinding("body", gocui.KeyEnter, gocui.ModNone, a.bodyEnter); err != nil {
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

	// auth modal keys
	if err := g.SetKeybinding("auth-schemes", gocui.KeyArrowDown, gocui.ModNone, a.moveAuthSel(1)); err != nil {
		return err
	}
	if err := g.SetKeybinding("auth-schemes", gocui.KeyArrowUp, gocui.ModNone, a.moveAuthSel(-1)); err != nil {
		return err
	}
	if err := g.SetKeybinding("auth-schemes", gocui.KeyEnter, gocui.ModNone, a.startAuthEdit); err != nil {
		return err
	}
	if err := g.SetKeybinding("auth-form", gocui.KeyEnter, gocui.ModNone, a.submitAuth); err != nil {
		return err
	}
	// use Ctrl+D to avoid clobbering normal typing (e.g. emails)
	if err := g.SetKeybinding("auth-form", gocui.KeyCtrlD, gocui.ModNone, a.clearAuth); err != nil {
		return err
	}
	// printable input in auth form
	for r := rune(32); r <= rune(126); r++ {
		if err := g.SetKeybinding("auth-form", r, gocui.ModNone, a.authTypeRune(r)); err != nil {
			return err
		}
	}
	if err := g.SetKeybinding("auth-form", gocui.KeyBackspace, gocui.ModNone, a.authBackspace); err != nil {
		return err
	}
	if err := g.SetKeybinding("auth-form", gocui.KeyBackspace2, gocui.ModNone, a.authBackspace); err != nil {
		return err
	}
	if err := g.SetKeybinding("auth-form", gocui.KeyTab, gocui.ModNone, a.authNextField); err != nil {
		return err
	}

	return nil
}

func (a *App) quit(*gocui.Gui, *gocui.View) error { return gocui.ErrQuit }

func (a *App) back(*gocui.Gui, *gocui.View) error {
	if a.authOpen {
		a.closeAuth()
		return nil
	}
	if a.editing {
		return a.closeEdit()
	}
	switch a.scr {
	case screenResponse:
		a.scr = screenBuilder
	case screenBuilder:
		a.scr = screenEndpoints
	case screenEndpoints:
		// no previous screen
	}
	a.errorMsg = ""
	return nil
}

func (a *App) openAuth(*gocui.Gui, *gocui.View) error {
	// If the modal is already open, don't reset state.
	// This also prevents the global hotkey from clobbering input inside the modal.
	if a.authOpen {
		return nil
	}

	// Only usable after OpenAPI has been loaded (we need schemes).
	if len(a.secSchemes) == 0 {
		a.errorMsg = "no security schemes found (load OpenAPI first)"
		return nil
	}

	a.authOpen = true
	a.authEditing = false
	a.authError = ""
	a.authSelected = 0
	a.authMode = authModeToken

	a.authSchemes = a.authSchemes[:0]
	for name := range a.secSchemes {
		a.authSchemes = append(a.authSchemes, name)
	}
	sort.Strings(a.authSchemes)

	// preselect first
	if len(a.authSchemes) > 0 {
		a.authActiveName = a.authSchemes[a.authSelected]
		a.loadAuthFormFromStore()
	}

	// force immediate paint so the modal isn't blank until next layout pass
	if a.g != nil {
		a.g.Update(func(g *gocui.Gui) error {
			_ = g
			a.renderFooter()
			a.renderAuth()
			return nil
		})
	}
	return nil
}

func (a *App) closeAuth() {
	a.authOpen = false
	a.authEditing = false
	a.authError = ""
	a.authMode = authModeToken
	a.authActiveName = ""
	if a.g != nil {
		if v, err := a.g.View("auth-form"); err == nil {
			v.Clear()
			a.g.DeleteView("auth-form")
		}
		if v, err := a.g.View("auth-schemes"); err == nil {
			v.Clear()
			a.g.DeleteView("auth-schemes")
		}
		if v, err := a.g.View("auth-box"); err == nil {
			v.Clear()
			a.g.DeleteView("auth-box")
		}
	}
}

func (a *App) moveAuthSel(delta int) func(*gocui.Gui, *gocui.View) error {
	return func(g *gocui.Gui, v *gocui.View) error {
		_ = g
		_ = v
		if !a.authOpen || a.authEditing || len(a.authSchemes) == 0 {
			return nil
		}
		a.authSelected += delta
		if a.authSelected < 0 {
			a.authSelected = 0
		}
		if a.authSelected >= len(a.authSchemes) {
			a.authSelected = len(a.authSchemes) - 1
		}
		a.authActiveName = a.authSchemes[a.authSelected]
		a.authError = ""
		a.loadAuthFormFromStore()
		if sv, err := a.g.View("auth-schemes"); err == nil {
			sv.SetCursor(0, a.authSelected)
		}
		return nil
	}
}

func (a *App) startAuthEdit(*gocui.Gui, *gocui.View) error {
	if !a.authOpen || len(a.authSchemes) == 0 {
		return nil
	}
	a.authEditing = true
	a.authError = ""
	a.authMode = authModeToken
	if scheme := a.secSchemes[a.authActiveName]; scheme.Type == "oauth2" && scheme.TokenURL != "" {
		a.authMode = authModeUser
	}
	a.renderAuth()
	return nil
}

func (a *App) authTypeRune(r rune) func(*gocui.Gui, *gocui.View) error {
	return func(g *gocui.Gui, v *gocui.View) error {
		_ = g
		_ = v
		if !a.authOpen || !a.authEditing {
			return nil
		}
		switch a.authMode {
		case authModeToken:
			a.authToken += string(r)
		case authModeUser:
			a.authUsername += string(r)
		case authModePass:
			a.authPassword += string(r)
		case authModeScope:
			a.authScope += string(r)
		}
		a.renderAuth()
		return nil
	}
}

func (a *App) authBackspace(*gocui.Gui, *gocui.View) error {
	if !a.authOpen || !a.authEditing {
		return nil
	}
	switch a.authMode {
	case authModeToken:
		if len(a.authToken) > 0 {
			a.authToken = a.authToken[:len(a.authToken)-1]
		}
	case authModeUser:
		if len(a.authUsername) > 0 {
			a.authUsername = a.authUsername[:len(a.authUsername)-1]
		}
	case authModePass:
		if len(a.authPassword) > 0 {
			a.authPassword = a.authPassword[:len(a.authPassword)-1]
		}
	case authModeScope:
		if len(a.authScope) > 0 {
			a.authScope = a.authScope[:len(a.authScope)-1]
		}
	}
	a.renderAuth()
	return nil
}

func (a *App) authNextField(*gocui.Gui, *gocui.View) error {
	if !a.authOpen || !a.authEditing {
		return nil
	}
	// token-only mode stays on token
	scheme := a.secSchemes[a.authActiveName]
	if scheme.Type != "oauth2" || scheme.TokenURL == "" {
		a.authMode = authModeToken
		a.renderAuth()
		return nil
	}

	switch a.authMode {
	case authModeUser:
		a.authMode = authModePass
	case authModePass:
		a.authMode = authModeScope
	case authModeScope:
		a.authMode = authModeUser
	default:
		a.authMode = authModeUser
	}
	a.renderAuth()
	return nil
}

func (a *App) clearAuth(*gocui.Gui, *gocui.View) error {
	if !a.authOpen {
		return nil
	}
	name := a.authActiveName
	delete(a.authStore, name)
	a.authToken = ""
	a.authUsername = ""
	a.authPassword = ""
	a.authScope = ""
	a.authError = ""
	a.authEditing = false
	a.renderAuth()
	return nil
}

func (a *App) submitAuth(*gocui.Gui, *gocui.View) error {
	if !a.authOpen {
		return nil
	}
	name := a.authActiveName
	ss, ok := a.secSchemes[name]
	if !ok {
		return nil
	}
	// Bearer token manual entry
	if ss.Type == "http" && strings.EqualFold(ss.Scheme, "bearer") {
		tok := strings.TrimSpace(a.authToken)
		if tok == "" {
			delete(a.authStore, name)
			a.authEditing = false
			a.renderAuth()
			return nil
		}
		a.authStore[name] = authState{schemeName: name, tokenType: "Bearer", token: tok, acquiredAt: time.Now()}
		a.authEditing = false
		a.authError = ""
		a.renderAuth()
		return nil
	}

	// OAuth2 password flow
	if ss.Type == "oauth2" && ss.TokenURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		accessToken, tokenType, err := httpclient.FetchOAuthPasswordToken(ctx, a.baseURL, ss.TokenURL, a.authUsername, a.authPassword, a.authScope)
		if err != nil {
			a.authError = err.Error()
			a.renderAuth()
			return nil
		}
		if tokenType == "" {
			tokenType = "Bearer"
		}
		a.authStore[name] = authState{schemeName: name, tokenType: tokenType, token: accessToken, acquiredAt: time.Now()}
		a.authEditing = false
		a.authError = ""
		a.renderAuth()
		return nil
	}

	a.authError = "unsupported security scheme"
	a.renderAuth()
	return nil
}

func (a *App) loadAuthFormFromStore() {
	name := a.authActiveName
	ss, ok := a.secSchemes[name]
	if !ok {
		return
	}
	if st, ok := a.authStore[name]; ok {
		a.authToken = st.token
		_ = ss
	} else {
		a.authToken = ""
	}
	// keep username/pass empty by default
	if ss.Type != "oauth2" {
		a.authUsername = ""
		a.authPassword = ""
		a.authScope = ""
	}
}

func (a *App) renderAuth() {
	// schemes list
	if v, err := a.g.View("auth-schemes"); err == nil {
		v.Clear()
		for i, name := range a.authSchemes {
			ss := a.secSchemes[name]
			status := "[unset]"
			if _, ok := a.authStore[name]; ok {
				status = "[set]"
			}
			desc := strings.TrimSpace(ss.Description)
			if desc != "" {
				desc = " - " + desc
			}
			fmt.Fprintf(v, "%s %s%s\n", status, name, desc)
			if i == a.authSelected {
				v.SetCursor(0, a.authSelected)
			}
		}
	}

	// form
	if v, err := a.g.View("auth-form"); err == nil {
		v.Clear()
		name := a.authActiveName
		ss := a.secSchemes[name]

		if name == "" {
			fmt.Fprintln(v, "No security schemes.")
			return
		}

		if a.authError != "" {
			fmt.Fprintf(v, "error: %s\n\n", a.authError)
		}

		fmt.Fprintf(v, "scheme: %s\n", name)
		fmt.Fprintf(v, "type:   %s\n\n", ss.Type)

		if ss.Type == "http" && strings.EqualFold(ss.Scheme, "bearer") {
			fmt.Fprintln(v, "Bearer token:")
			fmt.Fprintf(v, "%s\n\n", a.authToken)
			fmt.Fprintln(v, "enter: save   tab: (n/a)   ctrl+d: clear   esc: close")
			return
		}

		if ss.Type == "oauth2" {
			if strings.TrimSpace(ss.TokenURL) == "" {
				fmt.Fprintln(v, "OAuth2 scheme detected but no password-flow tokenUrl found in the spec.")
				fmt.Fprintln(v, "This app currently supports only OAuth2 password flow (flows.password.tokenUrl).")
				return
			}
			fmt.Fprintln(v, "OAuth2 password flow")
			fmt.Fprintf(v, "tokenUrl: %s\n\n", ss.TokenURL)
			fmt.Fprintf(v, "username: %s%s\n", fieldMarker(a.authMode == authModeUser), a.authUsername)
			fmt.Fprintf(v, "password: %s%s\n", fieldMarker(a.authMode == authModePass), mask(a.authPassword))
			fmt.Fprintf(v, "scope:    %s%s\n\n", fieldMarker(a.authMode == authModeScope), a.authScope)
			fmt.Fprintln(v, "tab: next field   enter: fetch token   ctrl+d: clear   esc: close")
			return
		}

		fmt.Fprintln(v, "(unsupported scheme in MVP)")
	}
}

func fieldMarker(active bool) string {
	if active {
		return "> "
	}
	return "  "
}

func mask(s string) string {
	if s == "" {
		return ""
	}
	return strings.Repeat("*", len(s))
}

func (a *App) loadEndpoints() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	doc, err := openapi.Load(ctx, a.specURL)
	if err != nil {
		return err
	}
	a.endpoints = openapi.ExtractEndpoints(doc)
	a.secSchemes = openapi.ExtractSecuritySchemes(doc)
	if a.baseURL == "" {
		a.baseURL = baseURLFromOpenAPI(doc)
	}
	a.filter = ""
	a.selected = 0
	a.recomputeFilter()
	return nil
}

func normalizeBaseURL(in string) string {
	in = strings.TrimSpace(in)
	if in == "" {
		return ""
	}
	if strings.HasPrefix(in, "http://") || strings.HasPrefix(in, "https://") {
		return in
	}
	return "http://" + in
}

func baseURLFromURLSpec(specURL string) string {
	specURL = strings.TrimSpace(specURL)
	if specURL == "" {
		return ""
	}
	u, err := url.Parse(specURL)
	if err != nil {
		return ""
	}
	u.Fragment = ""
	u.RawQuery = ""
	u.Path = path.Dir(u.Path)
	if u.Path == "." {
		u.Path = ""
	}
	return strings.TrimRight(u.String(), "/")
}

func baseURLFromOpenAPI(doc *openapi3.T) string {
	if doc == nil {
		return ""
	}
	if len(doc.Servers) == 0 {
		return ""
	}
	u := strings.TrimSpace(doc.Servers[0].URL)
	// For now, we only support concrete URLs (no {vars}).
	if u == "" || strings.Contains(u, "{") {
		return ""
	}
	if p, err := url.Parse(u); err == nil {
		p.Fragment = ""
		p.RawQuery = ""
		return strings.TrimRight(p.String(), "/")
	}
	return ""
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
	a.bodyRaw = ""
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
		a.bodyRaw = ""
	}
	a.renderBuilder()
	return nil
}

func (a *App) bodyEnter(g *gocui.Gui, v *gocui.View) error {
	if a.scr != screenBuilder || a.editing {
		return nil
	}
	if a.activeEndpoint.Body == nil {
		return nil
	}
	// Always drop into $EDITOR for JSON body editing.
	return a.editBodyInEditor(g, v)
}

func (a *App) editBodyInEditor(*gocui.Gui, *gocui.View) error {
	if a.scr != screenBuilder || a.editing {
		return nil
	}
	if a.activeEndpoint.Body == nil {
		return nil
	}

	// Seed with existing raw JSON, otherwise generate a "swagger-like" starter
	// object using defaults/examples when available.
	seed := strings.TrimSpace(a.bodyRaw)
	if seed == "" {
		obj := map[string]any{}
		if a.activeEndpoint.Body != nil {
			for _, f := range a.activeEndpoint.Body.Fields {
				// Prefer explicit default, then example.
				val := strings.TrimSpace(f.Default)
				if val == "" {
					val = strings.TrimSpace(f.Example)
				}

				// If the user has previously used the old field-based editor, use that
				// as the last-resort seed.
				if val == "" {
					val = strings.TrimSpace(a.bodyVals[f.Name])
				}

				if val == "" {
					continue
				}

				obj[f.Name] = coerceJSONScalar(f.Type, val)
			}
		}
		if len(obj) > 0 {
			if b, err := json.MarshalIndent(obj, "", "  "); err == nil {
				seed = string(b)
			}
		}
	}
	if seed == "" {
		seed = "{}\n"
	} else if !strings.HasSuffix(seed, "\n") {
		seed += "\n"
	}

	// Write to temp file and request that Run() suspends into $EDITOR.
	f, err := os.CreateTemp("", "xhark-body-*.json")
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := io.Copy(f, bytes.NewBufferString(seed)); err != nil {
		return nil
	}
	a.suspendEditorFile = f.Name()
	return gocui.ErrQuit
}

func (a *App) runExternalEditor(file string) error {
	editor := strings.TrimSpace(os.Getenv("XHARK_EDITOR"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		editor = "vi"
	}

	args := splitCommand(editor)
	cmdName := args[0]
	cmdArgs := append(args[1:], file)
	cmd := exec.Command(cmdName, cmdArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return err
	}

	b, err := os.ReadFile(file)
	_ = os.Remove(file)
	if err != nil {
		return err
	}

	raw := strings.TrimSpace(string(b))
	if raw == "" {
		a.bodyRaw = ""
		return nil
	}

	// Validate JSON and normalize it.
	var v any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("invalid json body: %w", err)
	}
	// Reject trailing junk (second JSON value)
	if dec.More() {
		return fmt.Errorf("invalid json body: multiple json values")
	}
	norm, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	a.bodyRaw = string(norm)
	return nil
}

func splitCommand(s string) []string {
	// Minimal shell-like splitting: whitespace, no quotes/escapes.
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return []string{"vi"}
	}
	return fields
}

func coerceJSONScalar(t model.ParamType, raw string) any {
	raw = strings.TrimSpace(raw)
	switch t {
	case model.TypeBoolean:
		if b, err := strconv.ParseBool(raw); err == nil {
			return b
		}
	case model.TypeInteger:
		if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return i
		}
	case model.TypeNumber:
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return f
		}
	}
	return raw
}

func (a *App) authHeadersForEndpoint(ep model.Endpoint) map[string]string {
	// No security requirements: nothing to inject.
	if len(ep.Security) == 0 {
		return nil
	}

	// Swagger semantics: SecurityRequirements is OR-of-requirements.
	// Pick the first requirement that is fully satisfied by our authStore.
	for _, req := range ep.Security {
		ok := true
		headers := map[string]string{}
		for schemeName := range req {
			st, has := a.authStore[schemeName]
			if !has || strings.TrimSpace(st.token) == "" {
				ok = false
				break
			}
			// MVP: only Bearer-ish schemes -> Authorization header.
			headers["Authorization"] = strings.TrimSpace(st.tokenType) + " " + strings.TrimSpace(st.token)
		}
		if ok {
			return headers
		}
	}
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
	if strings.TrimSpace(a.baseURL) == "" {
		a.errorMsg = "base URL unknown (spec missing servers); set XHARK_BASE_URL, or load spec from an http(s) URL"
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	headers := a.authHeadersForEndpoint(a.activeEndpoint)
	req, err := httpclient.BuildRequest(a.baseURL, a.activeEndpoint, a.pathVals, a.queryVals, a.bodyVals, a.bodyRaw)
	if err != nil {
		a.errorMsg = err.Error()
		return nil
	}
	if len(headers) > 0 {
		if req.Headers == nil {
			req.Headers = map[string]string{}
		}
		for k, v := range headers {
			req.Headers[k] = v
		}
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
			if a.authOpen {
				msg = "auth: enter=edit/save   tab=next field   ctrl+d=clear   esc=close"
			} else {
				switch a.scr {
				case screenEndpoints:
					msg = "type: filter   1-5: quick select   enter: select   esc: back   A: auth   q: quit"
				case screenBuilder:
					msg = "tab: switch pane   enter: edit   d: reset param   ctrl+r: run   A: auth   esc: back"
					if a.pane == paneBody && a.activeEndpoint.Body != nil {
						msg = "tab: switch pane   enter: edit json ($EDITOR)   d: reset param   ctrl+r: run   A: auth   esc: back"
					}
				case screenResponse:
					msg = "up/down: scroll   r: rerun   enter: back to endpoints   A: auth   esc: back"
				}
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
		if strings.TrimSpace(a.bodyRaw) != "" {
			fmt.Fprintf(v, "%sbody: raw json set%s\n", colorCyan, colorReset)
		}
		if len(a.activeEndpoint.Security) > 0 {
			if a.authHeadersForEndpoint(a.activeEndpoint) != nil {
				fmt.Fprintf(v, "%sauth: set%s\n", colorCyan, colorReset)
			} else {
				fmt.Fprintf(v, "%sauth: required (press A)%s\n", colorYellow, colorReset)
			}
		}
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
