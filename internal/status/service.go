package status

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/inventory"
	"github.com/loganjanssen/ccm/internal/model"
	"github.com/loganjanssen/ccm/internal/restart"
	"github.com/loganjanssen/ccm/internal/script"
)

type Service struct {
	cfg     *config.Config
	inv     *inventory.Service
	restart *restart.Service
	scripts *script.Service
}

func NewService(cfg *config.Config, inv *inventory.Service, rs *restart.Service, ss *script.Service) *Service {
	return &Service{cfg: cfg, inv: inv, restart: rs, scripts: ss}
}

func (s *Service) Summary(ctx context.Context) model.SystemSummary {
	rows, _, projects := s.inv.Global(ctx)
	targetErrors := map[string]string{}
	for _, row := range rows {
		if row.Type == "target_error" {
			targetErrors[row.TargetID] = row.Name
		}
	}

	targets := make([]model.TargetSummary, 0, len(s.cfg.Targets))
	for targetID := range s.cfg.Targets {
		target := model.TargetSummary{TargetID: targetID, Status: "ok"}
		if errText := strings.TrimSpace(targetErrors[targetID]); errText != "" {
			target.Status = "error"
			target.Error = errText
		}
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].TargetID < targets[j].TargetID })

	stacks := s.stackSummaries(projects, targetErrors)
	restartFailures := failedRestarts(s.restart)
	scriptFailures := failedScripts(s.scripts)

	overall := "ok"
	for _, t := range targets {
		if t.Status == "error" {
			overall = "error"
			break
		}
	}
	if overall == "ok" {
		for _, st := range stacks {
			if st.Status != "running" {
				overall = "degraded"
				break
			}
		}
	}
	if overall == "ok" && (len(restartFailures) > 0 || len(scriptFailures) > 0) {
		overall = "degraded"
	}

	return model.SystemSummary{
		Status:          overall,
		Targets:         targets,
		Stacks:          stacks,
		RestartFailures: restartFailures,
		ScriptFailures:  scriptFailures,
	}
}

func (s *Service) stackSummaries(projects []model.ComposeProject, targetErrors map[string]string) []model.StackSummary {
	byID := map[string]model.ComposeProject{}
	byTargetProject := map[string]model.ComposeProject{}
	for _, p := range projects {
		byID[p.ID] = p
		byTargetProject[p.TargetID+":"+p.ProjectName] = p
	}

	stackIDs := make([]string, 0, len(s.cfg.Stacks))
	for id := range s.cfg.Stacks {
		stackIDs = append(stackIDs, id)
	}
	sort.Strings(stackIDs)

	out := make([]model.StackSummary, 0, len(stackIDs))
	for _, stackID := range stackIDs {
		stack := s.cfg.Stacks[stackID]
		if stack == nil {
			continue
		}
		projectName := filepath.Base(stack.DeploySubdir)
		p, ok := byID[stackID]
		if !ok {
			p, ok = byTargetProject[stack.TargetID+":"+projectName]
		}
		if !ok {
			status := "missing"
			reason := "compose project was not found in Docker inventory"
			if _, targetFailed := targetErrors[stack.TargetID]; targetFailed {
				status = "unknown"
				reason = "target inventory failed"
			}
			out = append(out, model.StackSummary{
				StackID:     stackID,
				TargetID:    stack.TargetID,
				ProjectName: projectName,
				Status:      status,
				Reason:      reason,
			})
			continue
		}
		containers := make([]model.SummaryContainer, 0, len(p.Containers))
		statusText := p.Status
		reason := ""
		for _, c := range p.Containers {
			containers = append(containers, model.SummaryContainer{
				ID:             c.ID,
				Name:           c.Name,
				Status:         c.Status,
				Health:         c.Health,
				RestartCount:   c.RestartCount,
				Uptime:         c.Uptime,
				ContainerID:    c.ContainerID,
				TargetID:       c.TargetID,
				ComposeProject: c.ComposeProject,
			})
			if c.Status != "running" && reason == "" {
				reason = "one or more containers are not running"
			}
			if c.Health == "unhealthy" && reason == "" {
				reason = "one or more containers are unhealthy"
			}
		}
		out = append(out, model.StackSummary{
			StackID:     stackID,
			TargetID:    stack.TargetID,
			ProjectName: p.ProjectName,
			Status:      statusText,
			Containers:  containers,
			Reason:      reason,
		})
	}
	return out
}

func failedRestarts(rs *restart.Service) []model.RestartTrackingEntry {
	if rs == nil {
		return nil
	}
	rows := rs.Snapshot()
	out := make([]model.RestartTrackingEntry, 0)
	for _, row := range rows {
		if row.ConsecutiveFailures > 0 || row.LastResult == "failed" {
			out = append(out, row)
		}
	}
	return out
}

func failedScripts(ss *script.Service) []model.ScriptTrackingEntry {
	if ss == nil {
		return nil
	}
	rows := ss.Snapshot()
	out := make([]model.ScriptTrackingEntry, 0)
	for _, row := range rows {
		if row.ConsecutiveFailures > 0 || row.LastResult == "failed" {
			out = append(out, row)
		}
	}
	return out
}
