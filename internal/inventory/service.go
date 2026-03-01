package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/model"
	"github.com/loganjanssen/ccm/internal/sshx"
)

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

	cmd := "ids=$(docker ps -q); if [ -z \"$ids\" ]; then echo '[]'; else docker inspect $ids; fi"
	res, err := s.ssh.RunCommand(ctx, targetID, cmd, 8*time.Second)
	inv := model.TargetInventory{TargetID: targetID, At: time.Now()}
	if err != nil {
		inv.Err = err.Error()
		s.putCache(targetID, inv)
		return inv
	}
	if res.ExitCode != 0 {
		inv.Err = strings.TrimSpace(res.Stderr)
		s.putCache(targetID, inv)
		return inv
	}
	containers, projects, perr := s.parseInspect(targetID, res.Stdout)
	if perr != nil {
		inv.Err = perr.Error()
	} else {
		inv.Containers = containers
		inv.Projects = projects
	}
	s.putCache(targetID, inv)
	return inv
}

func (s *Service) putCache(targetID string, inv model.TargetInventory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[targetID] = inv
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
			if c.Status != "running" {
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
