package inventory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/model"
	"github.com/loganjanssen/ccm/internal/sshx"
)

var runnerUnitPattern = regexp.MustCompile(`^actions\.runner\.[A-Za-z0-9_.-]+\.service$`)

type Service struct {
	cfg   *config.Config
	ssh   *sshx.Manager
	ttl   time.Duration
	mu    sync.Mutex
	cache map[string]model.TargetInventory
}

func NewService(cfg *config.Config, ssh *sshx.Manager, ttl time.Duration) *Service {
	if cfg.InventoryTTLSeconds > 0 {
		ttl = time.Duration(cfg.InventoryTTLSeconds) * time.Second
	}
	return &Service{cfg: cfg, ssh: ssh, ttl: ttl, cache: map[string]model.TargetInventory{}}
}

func (s *Service) Global(ctx context.Context) ([]model.InventoryRow, []model.Container, []model.ComposeProject) {
	rows := make([]model.InventoryRow, 0)
	containers := make([]model.Container, 0)
	projects := make([]model.ComposeProject, 0)

	for targetID := range s.cfg.Targets {
		inv := s.targetInventory(ctx, targetID)
		if inv.Err != "" {
			rows = append(rows, model.InventoryRow{
				Type:     "target_error",
				ID:       "err:" + targetID,
				Name:     inv.Err,
				TargetID: targetID,
				Status:   "error",
			})
			continue
		}
		for _, c := range inv.Containers {
			containers = append(containers, c)
			if c.ComposeProject == "" {
				rows = append(rows, model.InventoryRow{
					Type:     "container",
					ID:       c.ID,
					Name:     c.Name,
					TargetID: c.TargetID,
					Status:   c.Status,
				})
			}
		}
		for _, p := range inv.Projects {
			projects = append(projects, p)
			rows = append(rows, model.InventoryRow{
				Type:     "compose",
				ID:       p.ID,
				Name:     p.ProjectName,
				TargetID: p.TargetID,
				Status:   p.Status,
				Count:    len(p.Containers),
			})
		}
		for _, h := range inv.RunnerHosts {
			rows = append(rows, model.InventoryRow{Type: "github_runner_host", ID: h.ID, Name: h.Name, TargetID: h.TargetID, Status: h.Status, Count: len(h.Runners)})
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, containers, projects
}

func (s *Service) ProjectChildren(ctx context.Context, id string) []model.Container {
	_, containers, projects := s.Global(ctx)
	_ = containers
	for _, p := range projects {
		if p.ID == id {
			return p.Containers
		}
	}
	return nil
}

func (s *Service) RunnerChildren(ctx context.Context, id string) []model.Runner {
	_, _, projects := s.Global(ctx)
	_ = projects
	for targetID := range s.cfg.Targets {
		inv := s.targetInventory(ctx, targetID)
		for _, h := range inv.RunnerHosts {
			if h.ID == id {
				if h.Runners == nil {
					return []model.Runner{}
				}
				return h.Runners
			}
		}
	}
	return nil
}

func (s *Service) RunnerByID(ctx context.Context, id string) (model.Runner, bool) {
	for targetID := range s.cfg.Targets {
		inv := s.targetInventory(ctx, targetID)
		for _, h := range inv.RunnerHosts {
			for _, runner := range h.Runners {
				if runner.ID == id {
					return runner, true
				}
			}
		}
	}
	return model.Runner{}, false
}

func (s *Service) ContainerByID(ctx context.Context, id string) (model.Container, bool) {
	_, containers, _ := s.Global(ctx)
	for _, c := range containers {
		if c.ID == id {
			return c, true
		}
	}
	return model.Container{}, false
}

func (s *Service) targetInventory(ctx context.Context, targetID string) model.TargetInventory {
	s.mu.Lock()
	cached, ok := s.cache[targetID]
	if ok && time.Since(cached.At) < s.ttl {
		s.mu.Unlock()
		return cached
	}
	s.mu.Unlock()

	inv := model.TargetInventory{TargetID: targetID, At: time.Now()}
	// Docker and runner discovery are independent: runner-only targets are valid.
	cmd := "ids=$(docker ps -q); if [ -z \"$ids\" ]; then echo '[]'; else docker inspect $ids; fi"
	res, err := s.ssh.RunCommand(ctx, targetID, cmd, 8*time.Second)
	if err == nil && res.ExitCode == 0 {
		containers, projects, perr := s.parseInspect(targetID, res.Stdout)
		if perr != nil {
			if t := s.cfg.Targets[targetID]; t == nil || t.GitHubRunners == nil || !t.GitHubRunners.Enabled {
				inv.Err = perr.Error()
			}
		} else {
			inv.Containers, inv.Projects = containers, projects
		}
	} else if err != nil {
		// Preserve Docker errors only for non-runner targets.
		if t := s.cfg.Targets[targetID]; t == nil || t.GitHubRunners == nil || !t.GitHubRunners.Enabled {
			inv.Err = err.Error()
		}
	} else if t := s.cfg.Targets[targetID]; t == nil || t.GitHubRunners == nil || !t.GitHubRunners.Enabled {
		inv.Err = strings.TrimSpace(res.Stderr)
	}
	if t := s.cfg.Targets[targetID]; t != nil && t.GitHubRunners != nil && t.GitHubRunners.Enabled {
		host, rerr := s.discoverRunners(ctx, targetID, t.GitHubRunners.Home)
		if rerr != nil {
			host.Status, host.Err = "error", rerr.Error()
			inv.RunnerHosts = []model.RunnerHost{host}
		} else {
			inv.RunnerHosts = []model.RunnerHost{host}
		}
	}
	s.putCache(targetID, inv)
	return inv
}

func (s *Service) discoverRunners(ctx context.Context, targetID, home string) (model.RunnerHost, error) {
	host := model.RunnerHost{ID: targetID + ":github-runners", TargetID: targetID, Name: targetID}
	cmd := "command -v systemctl >/dev/null 2>&1 || { echo 'systemctl unavailable' >&2; exit 127; }; for d in " + shellQuote(home) + "/*; do [ -d \"$d\" ] || continue; printf 'CCM_RUNNER_DIR\\t%s\\n' \"$d\"; done; systemctl list-units --all --type=service --no-legend 'actions.runner.*.service' 2>/dev/null | while read -r unit rest; do [ -n \"$unit\" ] || continue; wd=$(systemctl show \"$unit\" --no-page --property=WorkingDirectory --value); printf 'CCM_RUNNER_UNIT\\t%s\\n' \"$unit\"; printf 'CCM_RUNNER_STATE\\t%s\\t%s\\t%s\\t%s\\t%s\\t%s\\t%s\\n' \"$unit\" \"$(systemctl show \"$unit\" --no-page --property=ActiveState --value)\" \"$(systemctl show \"$unit\" --no-page --property=UnitFileState --value)\" \"$(systemctl show \"$unit\" --no-page --property=MainPID --value)\" \"$(systemctl show \"$unit\" --no-page --property=ExecMainStartTimestamp --value)\" \"$(systemctl show \"$unit\" --no-page --property=Result --value)\" \"$wd\"; if [ -f \"$wd/.runner\" ]; then printf 'CCM_RUNNER_META\\t%s\\t' \"$unit\"; base64 < \"$wd/.runner\" | tr -d '\\n'; printf '\\n'; fi; done"
	res, err := s.ssh.RunCommand(ctx, targetID, cmd, 12*time.Second)
	if err != nil {
		return host, err
	}
	if res.ExitCode != 0 {
		return host, fmt.Errorf("runner discovery failed: %s", strings.TrimSpace(res.Stderr))
	}
	runners, err := parseRunnerDiscovery(targetID, home, res.Stdout)
	if err != nil {
		return host, err
	}
	host.Runners = runners
	host.Status = "running"
	if len(runners) == 0 {
		host.Status = "empty"
	}
	for _, r := range runners {
		if r.Status != "active" {
			host.Status = "degraded"
			break
		}
	}
	return host, nil
}

func parseRunnerDiscovery(targetID, home, raw string) ([]model.Runner, error) {
	dirs := []string{}
	runners := []model.Runner{}
	var current *model.Runner
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "CCM_RUNNER_DIR\t") {
			dirs = append(dirs, strings.TrimSpace(strings.TrimPrefix(line, "CCM_RUNNER_DIR\t")))
			continue
		}
		if strings.HasPrefix(line, "CCM_RUNNER_UNIT\t") {
			if current != nil {
				runners = append(runners, *current)
			}
			unit := strings.TrimSpace(strings.TrimPrefix(line, "CCM_RUNNER_UNIT\t"))
			if !runnerUnitPattern.MatchString(unit) {
				current = nil
				continue
			}
			current = &model.Runner{ID: targetID + ":runner:" + unit, TargetID: targetID, Name: unit, UnitName: unit, Status: "unknown"}
			continue
		}
		if strings.HasPrefix(line, "CCM_RUNNER_META\t") {
			if current != nil {
				parts := strings.SplitN(line, "\t", 3)
				if len(parts) == 3 && parts[1] == current.UnitName {
					applyRunnerMetadata(current, parts[2])
				}
			}
			continue
		}
		if strings.HasPrefix(line, "CCM_RUNNER_STATE\t") {
			if current == nil {
				continue
			}
			fields := strings.SplitN(line, "\t", 8)
			if len(fields) != 8 || fields[1] != current.UnitName {
				continue
			}
			current.Status = strings.TrimSpace(fields[2])
			current.EnabledState = strings.TrimSpace(fields[3])
			current.PID = atoi(fields[4])
			current.StartTime = strings.TrimSpace(fields[5])
			current.Result = strings.TrimSpace(fields[6])
			current.WorkingDirectory = strings.TrimSpace(fields[7])
			current.RunnerDirectory = current.WorkingDirectory
			if current.StartTime != "" {
				current.Uptime = formatUptimeSystemd(current.StartTime)
			}
			continue
		}
		if current == nil || !strings.Contains(line, "\t") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 6 {
			continue
		}
		current.Status, current.EnabledState, current.PID, current.StartTime, current.Result, current.WorkingDirectory = fields[0], fields[1], atoi(fields[2]), strings.TrimSpace(fields[3]), fields[4], fields[5]
		current.RunnerDirectory = strings.TrimSpace(fields[5])
		if current.StartTime != "" {
			current.Uptime = formatUptimeSystemd(current.StartTime)
		}
		// Metadata is intentionally read only from the safe .runner file; credentials are never requested.
	}
	if current != nil {
		runners = append(runners, *current)
	}
	for i := range runners {
		if runners[i].RunnerDirectory == "" {
			for _, d := range dirs {
				if strings.Contains(runners[i].UnitName, filepath.Base(d)) {
					runners[i].RunnerDirectory = d
					break
				}
			}
		}
	}
	return runners, nil
}

type runnerMetadata struct {
	Name      string `json:"name"`
	AgentName string `json:"agentName"`
	URL       string `json:"serverUrl"`
	GitHubURL string `json:"gitHubUrl"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Work string `json:"workFolder"`
}

func applyRunnerMetadata(r *model.Runner, encoded string) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return
	}
	var m runnerMetadata
	if json.Unmarshal(b, &m) != nil {
		return
	}
	r.RunnerName = m.Name
	if r.RunnerName == "" {
		r.RunnerName = m.AgentName
	}
	r.GitHubURL = m.URL
	if r.GitHubURL == "" {
		r.GitHubURL = m.GitHubURL
	}
	r.Labels = make([]string, 0, len(m.Labels))
	for _, label := range m.Labels {
		if strings.TrimSpace(label.Name) != "" {
			r.Labels = append(r.Labels, label.Name)
		}
	}
	r.WorkFolder = m.Work
}

func shellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }
func atoi(v string) int          { n, _ := strconv.Atoi(strings.TrimSpace(v)); return n }
func formatUptimeSystemd(v string) string {
	if t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", strings.TrimSpace(v)); err == nil {
		return formatUptime(t.UTC().Format(time.RFC3339))
	}
	return v
}

func (s *Service) putCache(targetID string, inv model.TargetInventory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[targetID] = inv
}

func (s *Service) InvalidateTarget(targetID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, targetID)
}

type inspectContainer struct {
	ID              string        `json:"Id"`
	Name            string        `json:"Name"`
	Config          inspectConfig `json:"Config"`
	State           inspectState  `json:"State"`
	NetworkSettings inspectNet    `json:"NetworkSettings"`
}

type inspectConfig struct {
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

type inspectState struct {
	Status       string `json:"Status"`
	RestartCount int    `json:"RestartCount"`
	StartedAt    string `json:"StartedAt"`
	Health       *struct {
		Status string `json:"Status"`
	} `json:"Health"`
}

type inspectNet struct {
	Ports map[string][]map[string]string `json:"Ports"`
}

func (s *Service) parseInspect(targetID, raw string) ([]model.Container, []model.ComposeProject, error) {
	var arr []inspectContainer
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return nil, nil, fmt.Errorf("inspect parse: %w", err)
	}

	containers := make([]model.Container, 0, len(arr))
	byProject := map[string][]model.Container{}
	for _, c := range arr {
		ports := make([]string, 0)
		for k, vv := range c.NetworkSettings.Ports {
			if len(vv) == 0 {
				ports = append(ports, k)
				continue
			}
			for _, b := range vv {
				ports = append(ports, b["HostIp"]+":"+b["HostPort"]+"->"+k)
			}
		}
		sort.Strings(ports)

		name := strings.TrimPrefix(c.Name, "/")
		proj := c.Config.Labels["com.docker.compose.project"]
		item := model.Container{
			ID:             targetID + ":" + shortID(c.ID),
			ContainerID:    shortID(c.ID),
			Name:           name,
			Image:          c.Config.Image,
			Status:         c.State.Status,
			Health:         healthStatus(c.State),
			RestartCount:   c.State.RestartCount,
			Ports:          ports,
			TargetID:       targetID,
			ComposeProject: proj,
			Labels:         c.Config.Labels,
			Uptime:         formatUptime(c.State.StartedAt),
		}
		containers = append(containers, item)
		if proj != "" {
			byProject[proj] = append(byProject[proj], item)
		}
	}

	projects := make([]model.ComposeProject, 0, len(byProject))
	for proj, cs := range byProject {
		status := "running"
		for _, c := range cs {
			if c.Status != "running" || c.Health == "unhealthy" {
				status = "degraded"
				break
			}
		}
		id := targetID + ":" + proj
		if stackID := s.matchStack(targetID, proj); stackID != "" {
			id = stackID
		}
		projects = append(projects, model.ComposeProject{
			ID:          id,
			TargetID:    targetID,
			ProjectName: proj,
			Status:      status,
			Containers:  cs,
		})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ProjectName < projects[j].ProjectName })
	return containers, projects, nil
}

func healthStatus(state inspectState) string {
	if state.Health == nil {
		return ""
	}
	return strings.TrimSpace(state.Health.Status)
}

func (s *Service) matchStack(targetID, project string) string {
	for id, st := range s.cfg.Stacks {
		if st.TargetID != targetID {
			continue
		}
		if filepath.Base(st.DeploySubdir) == project {
			return id
		}
	}
	return ""
}

func shortID(in string) string {
	if len(in) >= 12 {
		return in[:12]
	}
	return in
}

func formatUptime(startedAt string) string {
	if strings.TrimSpace(startedAt) == "" || startedAt == "0001-01-01T00:00:00Z" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return startedAt
	}
	d := time.Since(t)
	if d < 0 {
		return "-"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}
