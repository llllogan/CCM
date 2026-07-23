package api

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/control"
	"github.com/loganjanssen/ccm/internal/deploy"
	"github.com/loganjanssen/ccm/internal/disk"
	"github.com/loganjanssen/ccm/internal/dockermaint"
	"github.com/loganjanssen/ccm/internal/inventory"
	"github.com/loganjanssen/ccm/internal/logs"
	"github.com/loganjanssen/ccm/internal/model"
	"github.com/loganjanssen/ccm/internal/network"
	"github.com/loganjanssen/ccm/internal/restart"
	"github.com/loganjanssen/ccm/internal/runner"
	"github.com/loganjanssen/ccm/internal/script"
	ccmstatus "github.com/loganjanssen/ccm/internal/status"
	"github.com/loganjanssen/ccm/internal/update"
	"github.com/loganjanssen/ccm/internal/util"
)

//go:embed static/*
var staticFS embed.FS

const defaultTheme = "win98"

type Router struct {
	cfg     *config.Config
	inv     *inventory.Service
	deploy  *deploy.Service
	control *control.Service
	runners *runner.Service
	docker  *dockermaint.Service
	disk    *disk.Service
	network *network.Service
	logs    *logs.Service
	restart *restart.Service
	scripts *script.Service
	status  *ccmstatus.Service
	updates *update.Service
	index   *template.Template
	tpls    map[string]*template.Template
	rawLogs []byte
	themes  map[string]struct{}
}

func NewRouter(cfg *config.Config, inv *inventory.Service, d *deploy.Service, c *control.Service, rr *runner.Service, dm *dockermaint.Service, ds *disk.Service, ns *network.Service, l *logs.Service, rs *restart.Service, ss *script.Service, us *update.Service) http.Handler {
	root, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("static root missing")
	}
	index, err := template.ParseFS(root, "index.html")
	if err != nil {
		panic("index template parse failed")
	}
	rawLogs, err := fs.ReadFile(root, "raw-logs.html")
	if err != nil {
		panic("raw logs page missing")
	}
	assetsFS, err := fs.Sub(root, "assets")
	if err != nil {
		panic("assets missing")
	}
	themes, err := loadThemes(assetsFS)
	if err != nil {
		panic("themes missing")
	}
	if _, ok := themes[defaultTheme]; !ok {
		panic("default theme missing")
	}
	tpls, err := loadThemeTemplates(root, themes)
	if err != nil {
		panic("theme templates invalid")
	}

	r := &Router{
		cfg:     cfg,
		inv:     inv,
		deploy:  d,
		control: c,
		runners: rr,
		docker:  dm,
		disk:    ds,
		network: ns,
		logs:    l,
		restart: rs,
		scripts: ss,
		status:  ccmstatus.NewService(cfg, inv, rs, ss),
		updates: us,
		index:   index,
		tpls:    tpls,
		rawLogs: rawLogs,
		themes:  themes,
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", r.health)
	mux.HandleFunc("/v1/stacks", r.stacks)
	mux.HandleFunc("/v1/inventory", r.inventory)
	mux.HandleFunc("/v1/summary", r.summary)
	mux.HandleFunc("/v1/updates/ccm", r.ccmUpdate)
	mux.HandleFunc("/v1/items/", r.itemChildren)
	mux.HandleFunc("/v1/targets/", r.targetRoute)
	mux.HandleFunc("/v1/containers/", r.containerRoute)
	mux.HandleFunc("/v1/runners/", r.runnerRoute)
	mux.HandleFunc("/v1/compose/", r.composeRoute)
	mux.HandleFunc("/v1/deploy", r.deployRoute)
	mux.HandleFunc("/v1/restarts/tracking", r.restartTracking)
	mux.HandleFunc("/v1/scripts/", r.scriptRoute)

	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assetsFS))))
	mux.HandleFunc("/raw-logs.html", r.rawLogsPage)
	mux.HandleFunc("/", r.uiRoute)

	return mux
}

func (r *Router) rawLogsPage(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		util.WriteErr(w, 405, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(r.rawLogs)
}

func (r *Router) uiRoute(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		util.WriteErr(w, 404, "not found")
		return
	}

	theme := defaultTheme
	if req.URL.Path != "/" {
		path := strings.TrimPrefix(req.URL.Path, "/")
		if path == "" || strings.Contains(path, "/") {
			util.WriteErr(w, 404, "not found")
			return
		}
		if _, ok := r.themes[path]; !ok {
			util.WriteErr(w, 404, "not found")
			return
		}
		theme = path
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tpl := r.index
	if themed, ok := r.tpls[theme]; ok {
		tpl = themed
	}
	if err := tpl.Execute(w, map[string]string{"Theme": theme, "ThemeColor": themeColor(theme)}); err != nil {
		util.WriteErr(w, 500, "template render failed")
		return
	}
}

func themeColor(theme string) string {
	switch theme {
	case "vista":
		return "#7899bc"
	case "leopard":
		return "#8fa1b9"
	case "bloomi":
		return "#000000"
	case "nasa":
		return "#171815"
	default:
		return "#008080"
	}
}

func loadThemes(assetsFS fs.FS) (map[string]struct{}, error) {
	entries, err := fs.ReadDir(assetsFS, "themes")
	if err != nil {
		return nil, err
	}
	themes := map[string]struct{}{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".css") {
			continue
		}
		theme := strings.TrimSuffix(name, ".css")
		if strings.TrimSpace(theme) == "" {
			continue
		}
		themes[theme] = struct{}{}
	}
	return themes, nil
}

func loadThemeTemplates(root fs.FS, themes map[string]struct{}) (map[string]*template.Template, error) {
	tpls := map[string]*template.Template{}
	for theme := range themes {
		path := fmt.Sprintf("themes/%s.html", theme)
		if _, err := fs.Stat(root, path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		tpl, err := template.ParseFS(root, path)
		if err != nil {
			return nil, err
		}
		tpls[theme] = tpl
	}
	return tpls, nil
}

func (r *Router) health(w http.ResponseWriter, _ *http.Request) {
	util.WriteJSON(w, 200, map[string]string{"status": "ok"})
}

func (r *Router) stacks(w http.ResponseWriter, _ *http.Request) {
	rows := make([]map[string]any, 0, len(r.cfg.Stacks))
	for id, s := range r.cfg.Stacks {
		restart := r.resolveStackRestart(id)
		rows = append(rows, map[string]any{
			"id":          id,
			"target_id":   s.TargetID,
			"deploy_path": s.Target.DeployRoot + "/" + s.DeploySubdir,
			"flags":       s.Flags,
			"restart":     restart,
		})
	}
	util.WriteJSON(w, 200, rows)
}

func (r *Router) inventory(w http.ResponseWriter, req *http.Request) {
	rows, _, projects := r.inv.Global(req.Context())
	util.WriteJSON(w, 200, map[string]any{
		"items":    rows,
		"projects": projects,
	})
}

func (r *Router) summary(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		util.WriteErr(w, 405, "method not allowed")
		return
	}
	util.WriteJSON(w, 200, r.status.Summary(req.Context()))
}

func (r *Router) ccmUpdate(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		util.WriteErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	util.WriteJSON(w, http.StatusOK, r.updates.Status(req.Context()))
}

func (r *Router) itemChildren(w http.ResponseWriter, req *http.Request) {
	if !strings.HasSuffix(req.URL.Path, "/children") {
		util.WriteErr(w, 404, "not found")
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/v1/items/"), "/children")
	cs, ok := r.inv.ProjectChildren(req.Context(), id)
	if ok {
		util.WriteJSON(w, 200, cs)
		return
	}
	runners := r.inv.RunnerChildren(req.Context(), id)
	if runners != nil {
		util.WriteJSON(w, 200, runners)
		return
	}
	if cs == nil {
		util.WriteErr(w, 404, "item not found")
		return
	}
	util.WriteJSON(w, 200, cs)
}

func (r *Router) runnerRoute(w http.ResponseWriter, req *http.Request) {
	if r.runners == nil {
		util.WriteErr(w, 404, "runner management not configured")
		return
	}
	path := strings.TrimPrefix(req.URL.Path, "/v1/runners/")
	parts := strings.Split(path, "/")
	id, err := url.PathUnescape(parts[0])
	if err != nil || strings.TrimSpace(id) == "" {
		util.WriteErr(w, 400, "invalid runner id")
		return
	}
	if len(parts) == 1 && req.Method == http.MethodGet {
		out, ok := r.runners.Detail(req.Context(), id)
		if !ok {
			util.WriteErr(w, 404, "runner not found")
			return
		}
		util.WriteJSON(w, 200, out)
		return
	}
	if len(parts) == 2 && req.Method == http.MethodPost && (parts[1] == "start" || parts[1] == "stop" || parts[1] == "restart" || parts[1] == "uninstall") {
		out, err := r.runners.Action(req.Context(), id, parts[1])
		if err != nil {
			util.WriteErr(w, 400, err.Error())
			return
		}
		util.WriteJSON(w, 200, out)
		return
	}
	util.WriteErr(w, 404, "not found")
}

func (r *Router) targetRoute(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/v1/targets/")
	parts := strings.Split(path, "/")
	if len(parts) == 2 && parts[1] == "disk" && req.Method == http.MethodGet {
		targetID, err := url.PathUnescape(parts[0])
		if err != nil || strings.TrimSpace(targetID) == "" {
			util.WriteErr(w, 400, "invalid target id")
			return
		}
		if r.disk == nil {
			util.WriteErr(w, 404, "disk monitoring not configured")
			return
		}
		out, err := r.disk.Usage(req.Context(), targetID)
		if err != nil {
			util.WriteErr(w, 400, err.Error())
			return
		}
		util.WriteJSON(w, 200, out)
		return
	}
	if len(parts) == 2 && parts[1] == "ip" && req.Method == http.MethodGet {
		targetID, err := url.PathUnescape(parts[0])
		if err != nil || strings.TrimSpace(targetID) == "" {
			util.WriteErr(w, 400, "invalid target id")
			return
		}
		if r.network == nil {
			util.WriteErr(w, 404, "network information not configured")
			return
		}
		out, err := r.network.IPInfo(req.Context(), targetID)
		if err != nil {
			util.WriteErr(w, 400, err.Error())
			return
		}
		util.WriteJSON(w, 200, out)
		return
	}
	if len(parts) != 3 || parts[1] != "docker" {
		util.WriteErr(w, 404, "not found")
		return
	}
	targetID, err := url.PathUnescape(parts[0])
	if err != nil || strings.TrimSpace(targetID) == "" {
		util.WriteErr(w, 400, "invalid target id")
		return
	}
	if r.docker == nil {
		util.WriteErr(w, 404, "docker maintenance not configured")
		return
	}

	switch {
	case parts[2] == "df" && req.Method == http.MethodGet:
		out, err := r.docker.DiskReport(req.Context(), targetID)
		if err != nil {
			util.WriteErr(w, 400, err.Error())
			return
		}
		util.WriteJSON(w, 200, out)
		return
	case parts[2] == "safe-prune" && req.Method == http.MethodPost:
		if !r.authorized(req) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			util.WriteErr(w, 401, "unauthorized")
			return
		}
		out, err := r.docker.SafePrune(req.Context(), targetID)
		if err != nil {
			if errors.Is(err, dockermaint.ErrMaintenanceRunning) {
				util.WriteErr(w, 409, err.Error())
				return
			}
			util.WriteErr(w, 400, err.Error())
			return
		}
		r.inv.InvalidateTarget(targetID)
		util.WriteJSON(w, 200, out)
		return
	}
	util.WriteErr(w, 404, "not found")
}

func (r *Router) authorized(req *http.Request) bool {
	token := strings.TrimSpace(r.cfg.AuthToken)
	if token == "" {
		return true
	}
	auth := strings.TrimSpace(req.Header.Get("authorization"))
	return auth == "Bearer "+token
}

func (r *Router) containerRoute(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/v1/containers/")
	parts := strings.Split(path, "/")
	if len(parts) == 1 && req.Method == http.MethodGet {
		r.containerDetail(w, req, parts[0])
		return
	}
	if len(parts) == 2 && req.Method == http.MethodPost {
		r.containerAction(w, req, parts[0], parts[1])
		return
	}
	if len(parts) == 2 && parts[1] == "logs" && req.Method == http.MethodGet {
		r.containerLogTail(w, req, parts[0])
		return
	}
	if len(parts) == 3 && parts[1] == "logs" && parts[2] == "stream" && req.Method == http.MethodGet {
		r.containerLogs(w, req, parts[0])
		return
	}
	util.WriteErr(w, 404, "not found")
}

func (r *Router) composeRoute(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/v1/compose/")
	parts := strings.Split(path, "/")
	if len(parts) == 2 && parts[1] == "redeploy" && req.Method == http.MethodPost {
		out, err := r.deploy.RedeployStack(req.Context(), parts[0])
		if err != nil {
			util.WriteErr(w, 400, err.Error())
			return
		}
		if stack, ok := r.cfg.Stacks[parts[0]]; ok {
			r.inv.InvalidateTarget(stack.TargetID)
		}
		util.WriteJSON(w, 200, out)
		return
	}
	util.WriteErr(w, 404, "not found")
}

func (r *Router) deployRoute(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		util.WriteErr(w, 405, "method not allowed")
		return
	}
	var body model.DeployRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		util.WriteErr(w, 400, "invalid json")
		return
	}
	if strings.Contains(req.Header.Get("Accept"), "text/event-stream") {
		r.streamDeployRoute(w, req, body)
		return
	}
	out, err := r.deploy.Deploy(req.Context(), body)
	if err != nil {
		util.WriteErr(w, 400, err.Error())
		return
	}
	util.WriteJSON(w, 200, out)
}

func (r *Router) streamDeployRoute(w http.ResponseWriter, req *http.Request, body model.DeployRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		util.WriteErr(w, 500, "stream unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	emit := func(event deploy.StreamEvent) error {
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	_, err := r.deploy.DeployStream(req.Context(), body, emit)
	if err != nil {
		_ = emit(deploy.StreamEvent{Type: "complete", Status: "failed", Error: err.Error()})
		return
	}
	_ = emit(deploy.StreamEvent{Type: "complete", Status: "succeeded", Phase: "complete"})
}

func (r *Router) restartTracking(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		util.WriteErr(w, 405, "method not allowed")
		return
	}
	if r.restart == nil {
		util.WriteJSON(w, 200, []model.RestartTrackingEntry{})
		return
	}
	util.WriteJSON(w, 200, r.restart.Snapshot())
}

func (r *Router) scriptRoute(w http.ResponseWriter, req *http.Request) {
	if r.scripts == nil {
		util.WriteErr(w, 404, "scripts not configured")
		return
	}
	path := strings.TrimPrefix(req.URL.Path, "/v1/scripts/")
	parts := strings.Split(path, "/")
	if len(parts) == 1 && req.Method == http.MethodGet {
		stackID, err := url.PathUnescape(parts[0])
		if err != nil || strings.TrimSpace(stackID) == "" {
			util.WriteErr(w, 400, "invalid stack id")
			return
		}
		util.WriteJSON(w, 200, r.scripts.SnapshotByStack(stackID))
		return
	}
	if len(parts) == 3 && parts[2] == "run" && req.Method == http.MethodPost {
		stackID, err := url.PathUnescape(parts[0])
		if err != nil || strings.TrimSpace(stackID) == "" {
			util.WriteErr(w, 400, "invalid stack id")
			return
		}
		scriptName, err := url.PathUnescape(parts[1])
		if err != nil || strings.TrimSpace(scriptName) == "" {
			util.WriteErr(w, 400, "invalid script name")
			return
		}
		res, entry, runErr := r.scripts.RunNow(req.Context(), stackID, scriptName)
		if runErr != nil {
			if errors.Is(runErr, script.ErrScriptNotFound) {
				util.WriteErr(w, 404, runErr.Error())
				return
			}
			if errors.Is(runErr, script.ErrScriptRunning) {
				util.WriteErr(w, 409, runErr.Error())
				return
			}
			util.WriteErr(w, 400, runErr.Error())
			return
		}
		util.WriteJSON(w, 200, map[string]any{
			"script": entry,
			"result": res,
		})
		return
	}
	util.WriteErr(w, 404, "not found")
}

func (r *Router) containerDetail(w http.ResponseWriter, req *http.Request, id string) {
	c, ok := r.inv.ContainerByID(req.Context(), id)
	if !ok {
		util.WriteErr(w, 404, "container not found")
		return
	}
	c.Restart = r.resolveContainerRestart(c)
	util.WriteJSON(w, 200, c)
}

func (r *Router) resolveStackRestart(stackID string) *model.RestartDisplay {
	stack, ok := r.cfg.Stacks[stackID]
	if !ok || stack == nil {
		return nil
	}
	strategyName := strings.TrimSpace(stack.Restart.Strategy)
	if strategyName == "" {
		return nil
	}
	strategy, ok := r.cfg.RestartStrategies[strategyName]
	if !ok {
		return &model.RestartDisplay{
			Enabled: false,
			Note:    "configured strategy not found",
		}
	}
	tz := strings.TrimSpace(strategy.Timezone)
	if tz == "" {
		tz = "Local"
	}
	return &model.RestartDisplay{
		Enabled:  true,
		Scope:    "stack",
		Source:   "stack",
		Strategy: strategyName,
		Cron:     strategy.Cron,
		Timezone: tz,
	}
}

func (r *Router) resolveContainerRestart(c model.Container) *model.RestartDisplay {
	_, stack := r.findStackForContainer(c)
	if stack == nil {
		return nil
	}

	service := strings.TrimSpace(c.Labels["com.docker.compose.service"])
	containerName := strings.TrimSpace(c.Name)
	var (
		pref  model.ContainerRestartPreference
		found bool
	)
	if service != "" {
		pref, found = stack.Restart.Containers[service]
	}
	if !found && containerName != "" {
		pref, found = stack.Restart.Containers[containerName]
	}

	stackStrategy := strings.TrimSpace(stack.Restart.Strategy)
	if found {
		ref := strings.TrimSpace(pref.Strategy)
		switch {
		case strings.EqualFold(ref, "none"):
			return &model.RestartDisplay{
				Enabled: false,
				Scope:   "container",
				Source:  "container",
				Note:    "explicitly disabled (strategy: none)",
			}
		case ref == "" || strings.EqualFold(ref, "inherit"):
			if stackStrategy == "" {
				return nil
			}
			return r.renderStrategy("container", "stack(inherit)", stackStrategy)
		default:
			return r.renderStrategy("container", "container", ref)
		}
	}

	if stackStrategy == "" {
		return nil
	}
	return r.renderStrategy("container", "stack", stackStrategy)
}

func (r *Router) renderStrategy(scope, source, strategyName string) *model.RestartDisplay {
	strategy, ok := r.cfg.RestartStrategies[strategyName]
	if !ok {
		return &model.RestartDisplay{
			Enabled: false,
			Scope:   scope,
			Source:  source,
			Note:    "configured strategy not found",
		}
	}
	tz := strings.TrimSpace(strategy.Timezone)
	if tz == "" {
		tz = "Local"
	}
	return &model.RestartDisplay{
		Enabled:  true,
		Scope:    scope,
		Source:   source,
		Strategy: strategyName,
		Cron:     strategy.Cron,
		Timezone: tz,
	}
}

func (r *Router) findStackForContainer(c model.Container) (string, *model.CCMStack) {
	project := strings.TrimSpace(c.ComposeProject)
	if project == "" {
		return "", nil
	}
	for id, st := range r.cfg.Stacks {
		if st == nil || st.TargetID != c.TargetID {
			continue
		}
		if filepath.Base(st.DeploySubdir) == project {
			return id, st
		}
	}
	return "", nil
}

func (r *Router) containerAction(w http.ResponseWriter, req *http.Request, id, action string) {
	var (
		res model.CommandResult
		err error
	)
	switch action {
	case "start":
		res, err = r.control.Start(req.Context(), id)
	case "stop":
		res, err = r.control.Stop(req.Context(), id)
	case "restart":
		res, err = r.control.Restart(req.Context(), id)
	default:
		util.WriteErr(w, 404, "unknown action")
		return
	}
	if err != nil {
		util.WriteErr(w, 400, err.Error())
		return
	}
	if targetID, ok := parseTargetFromContainerID(id); ok {
		r.inv.InvalidateTarget(targetID)
	}
	util.WriteJSON(w, 200, res)
}

func parseTargetFromContainerID(id string) (string, bool) {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		return "", false
	}
	return parts[0], true
}

func (r *Router) containerLogs(w http.ResponseWriter, req *http.Request, id string) {
	tail := 200
	if raw := req.URL.Query().Get("tail"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			tail = n
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		util.WriteErr(w, 500, "stream unsupported")
		return
	}

	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()

	writer := &sseWriter{w: w}
	err := r.logs.StreamContainerLogs(ctx, id, tail, writer)
	if err != nil {
		fmt.Fprintf(w, "event: terminal-error\ndata: %s\n\n", strings.ReplaceAll(err.Error(), "\n", " "))
		flusher.Flush()
		return
	}
	fmt.Fprint(w, "event: done\ndata: eof\n\n")
	flusher.Flush()
}

func (r *Router) containerLogTail(w http.ResponseWriter, req *http.Request, id string) {
	tail := 250
	if raw := req.URL.Query().Get("tail"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			tail = n
		}
	}
	out, err := r.logs.ReadContainerLogs(req.Context(), id, tail)
	if err != nil {
		util.WriteErr(w, 400, err.Error())
		return
	}
	util.WriteJSON(w, 200, out)
}

type sseWriter struct {
	w http.ResponseWriter
}

func (s *sseWriter) Write(p []byte) (int, error) {
	f, _ := s.w.(http.Flusher)
	scanner := bufio.NewScanner(strings.NewReader(string(p)))
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintf(s.w, "data: %s\n\n", strings.ReplaceAll(line, "\r", ""))
		f.Flush()
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return len(p), nil
}
