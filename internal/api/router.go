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
	"strconv"
	"strings"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/control"
	"github.com/loganjanssen/ccm/internal/deploy"
	"github.com/loganjanssen/ccm/internal/inventory"
	"github.com/loganjanssen/ccm/internal/logs"
	"github.com/loganjanssen/ccm/internal/model"
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
	logs    *logs.Service
	index   *template.Template
	tpls    map[string]*template.Template
	rawLogs []byte
	themes  map[string]struct{}
}

func NewRouter(cfg *config.Config, inv *inventory.Service, d *deploy.Service, c *control.Service, l *logs.Service) http.Handler {
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
		logs:    l,
		index:   index,
		tpls:    tpls,
		rawLogs: rawLogs,
		themes:  themes,
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", r.health)
	mux.HandleFunc("/v1/stacks", r.stacks)
	mux.HandleFunc("/v1/inventory", r.inventory)
	mux.HandleFunc("/v1/items/", r.itemChildren)
	mux.HandleFunc("/v1/containers/", r.containerRoute)
	mux.HandleFunc("/v1/compose/", r.composeRoute)
	mux.HandleFunc("/v1/deploy", r.deployRoute)

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
	if err := tpl.Execute(w, map[string]string{"Theme": theme}); err != nil {
		util.WriteErr(w, 500, "template render failed")
		return
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
		rows = append(rows, map[string]any{
			"id":          id,
			"target_id":   s.TargetID,
			"deploy_path": s.Target.DeployRoot + "/" + s.DeploySubdir,
			"flags":       s.Flags,
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

func (r *Router) itemChildren(w http.ResponseWriter, req *http.Request) {
	if !strings.HasSuffix(req.URL.Path, "/children") {
		util.WriteErr(w, 404, "not found")
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/v1/items/"), "/children")
	cs := r.inv.ProjectChildren(req.Context(), id)
	if cs == nil {
		util.WriteErr(w, 404, "item not found")
		return
	}
	util.WriteJSON(w, 200, cs)
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
	out, err := r.deploy.Deploy(req.Context(), body)
	if err != nil {
		util.WriteErr(w, 400, err.Error())
		return
	}
	util.WriteJSON(w, 200, out)
}

func (r *Router) containerDetail(w http.ResponseWriter, req *http.Request, id string) {
	c, ok := r.inv.ContainerByID(req.Context(), id)
	if !ok {
		util.WriteErr(w, 404, "container not found")
		return
	}
	util.WriteJSON(w, 200, c)
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
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", strings.ReplaceAll(err.Error(), "\n", " "))
		flusher.Flush()
		return
	}
	fmt.Fprint(w, "event: done\ndata: eof\n\n")
	flusher.Flush()
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
