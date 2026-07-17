package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/inventory"
	"github.com/loganjanssen/ccm/internal/model"
	"github.com/loganjanssen/ccm/internal/sshx"
)

var unitPattern = regexp.MustCompile(`^actions\.runner\.[A-Za-z0-9_.-]+\.service$`)

type Service struct {
	cfg *config.Config
	ssh *sshx.Manager
	inv *inventory.Service
}

func NewService(cfg *config.Config, ssh *sshx.Manager, inv *inventory.Service) *Service {
	return &Service{cfg: cfg, ssh: ssh, inv: inv}
}

func (s *Service) Detail(ctx context.Context, id string) (model.Runner, bool) {
	r, ok := s.inv.RunnerByID(ctx, id)
	if !ok || strings.TrimSpace(r.RunnerDirectory) == "" {
		return r, ok
	}
	t := s.cfg.Targets[r.TargetID]
	if t == nil || t.GitHubRunners == nil || !t.GitHubRunners.Enabled {
		return r, ok
	}
	workPath := strings.TrimRight(r.RunnerDirectory, "/") + "/_work"
	home := filepath.Clean(t.GitHubRunners.Home)
	cleanWork := filepath.Clean(workPath)
	if cleanWork != home && !strings.HasPrefix(cleanWork, home+string(filepath.Separator)) {
		return r, ok
	}
	res, err := s.ssh.RunCommand(ctx, r.TargetID, "du -sh -- "+shellQuote(cleanWork), 10*time.Second)
	if err == nil && res.ExitCode == 0 {
		fields := strings.Fields(res.Stdout)
		if len(fields) > 0 {
			r.WorkUsage = fields[0]
		}
	}
	return r, ok
}

func (s *Service) Action(ctx context.Context, id, op string) (model.CommandResult, error) {
	if op != "start" && op != "stop" && op != "restart" && op != "uninstall" {
		return model.CommandResult{}, fmt.Errorf("invalid runner action")
	}
	r, ok := s.inv.RunnerByID(ctx, id)
	if !ok {
		return model.CommandResult{}, fmt.Errorf("runner not found")
	}
	t := s.cfg.Targets[r.TargetID]
	if t == nil || t.GitHubRunners == nil || !t.GitHubRunners.Enabled {
		return model.CommandResult{}, fmt.Errorf("runner host not configured")
	}
	if !unitPattern.MatchString(r.UnitName) {
		return model.CommandResult{}, fmt.Errorf("invalid runner service unit")
	}
	// Unit names are discovered and validated, and are shell-quoted again at execution.
	cmd := fmt.Sprintf("sudo systemctl %s %s", op, shellQuote(r.UnitName))
	if op == "uninstall" {
		runnerDir := filepath.Clean(strings.TrimSpace(r.RunnerDirectory))
		home := filepath.Clean(t.GitHubRunners.Home)
		if runnerDir == "." || (runnerDir != home && !strings.HasPrefix(runnerDir, home+string(filepath.Separator))) {
			return model.CommandResult{}, fmt.Errorf("invalid runner directory")
		}
		cmd = fmt.Sprintf("cd %s && sudo ./svc.sh stop && sudo ./svc.sh uninstall", shellQuote(runnerDir))
	}
	res, err := s.ssh.RunCommand(ctx, r.TargetID, cmd, 30*time.Second)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("runner action failed: %s", strings.TrimSpace(res.Stderr))
	}
	s.inv.InvalidateTarget(r.TargetID)
	return res, nil
}

func shellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }
