package deploy

import (
	"context"
	"fmt"
	"path"
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

type Service struct {
	cfg  *config.Config
	ssh  *sshx.Manager
	mu   sync.Mutex
	lock map[string]*sync.Mutex
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func NewService(cfg *config.Config, ssh *sshx.Manager) *Service {
	return &Service{cfg: cfg, ssh: ssh, lock: map[string]*sync.Mutex{}}
}

func (s *Service) Deploy(ctx context.Context, req model.DeployRequest) (map[string]any, error) {
	stack, ok := s.cfg.Stacks[req.CCMStack]
	if !ok {
		return nil, fmt.Errorf("unknown ccm_stack %q", req.CCMStack)
	}
	if strings.TrimSpace(req.ComposeYML) == "" {
		return nil, fmt.Errorf("compose_yml is required")
	}

	unlock := s.stackLock(req.CCMStack)
	defer unlock()

	deployPath := path.Join(stack.Target.DeployRoot, stack.DeploySubdir)

	if err := s.ssh.WriteFile(ctx, stack.TargetID, path.Join(deployPath, "docker-compose.yml"), []byte(req.ComposeYML), "0644", 10*time.Second); err != nil {
		return nil, fmt.Errorf("write docker-compose.yml: %w", err)
	}

	envContent, envCount, err := buildEnvContent(req.EnvFile, req.Env)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(envContent) != "" {
		if err := s.ssh.WriteFile(ctx, stack.TargetID, path.Join(deployPath, ".env"), []byte(envContent), "0600", 10*time.Second); err != nil {
			return nil, fmt.Errorf("write .env: %w", err)
		}
	}
	if strings.TrimSpace(req.Caddyfile) != "" {
		if err := s.ssh.WriteFile(ctx, stack.TargetID, path.Join(deployPath, "Caddyfile"), []byte(req.Caddyfile), "0644", 10*time.Second); err != nil {
			return nil, fmt.Errorf("write Caddyfile: %w", err)
		}
	}

	results, err := s.runComposeUp(ctx, stack, deployPath)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"stack":       req.CCMStack,
		"target":      stack.TargetID,
		"deploy_path": deployPath,
		"repo":        req.Repo,
		"sha":         req.SHA,
		"env_count":   envCount,
		"caddyfile":   strings.TrimSpace(req.Caddyfile) != "",
		"steps":       results,
	}, nil
}

func (s *Service) RedeployStack(ctx context.Context, stackID string) (map[string]any, error) {
	stack, ok := s.cfg.Stacks[stackID]
	if !ok {
		return nil, fmt.Errorf("unknown stack: %s", stackID)
	}
	unlock := s.stackLock(stackID)
	defer unlock()
	deployPath := path.Join(stack.Target.DeployRoot, stack.DeploySubdir)
	results, async, logPath, err := s.runComposeUpSafe(ctx, stack, deployPath)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"stack":       stackID,
		"target":      stack.TargetID,
		"deploy_path": deployPath,
		"repo":        "manual",
		"sha":         "manual",
		"steps":       results,
		"async":       async,
	}
	if logPath != "" {
		out["log_path"] = logPath
	}
	return out, nil
}

func (s *Service) stackLock(id string) func() {
	s.mu.Lock()
	m, ok := s.lock[id]
	if !ok {
		m = &sync.Mutex{}
		s.lock[id] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (s *Service) runComposeUp(ctx context.Context, stack *model.CCMStack, deployPath string) ([]model.CommandResult, error) {
	results := []model.CommandResult{}
	if stack.Flags.Pull {
		cmd := fmt.Sprintf("cd %q && docker compose pull", deployPath)
		res, err := s.ssh.RunCommand(ctx, stack.TargetID, cmd, 10*time.Minute)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
		if res.ExitCode != 0 {
			return results, nil
		}
	}

	up := "docker compose up -d"
	if stack.Flags.RemoveOrphans {
		up += " --remove-orphans"
	}
	if strings.EqualFold(stack.Flags.Recreate, "force") {
		up += " --force-recreate"
	}
	cmd := fmt.Sprintf("cd %q && %s", deployPath, up)
	res, err := s.ssh.RunCommand(ctx, stack.TargetID, cmd, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	results = append(results, res)
	return results, nil
}

func (s *Service) runComposeUpSafe(ctx context.Context, stack *model.CCMStack, deployPath string) ([]model.CommandResult, bool, string, error) {
	if !isLikelySelfCCMStack(stack) {
		results, err := s.runComposeUp(ctx, stack, deployPath)
		return results, false, "", err
	}

	results := []model.CommandResult{}
	logFile := fmt.Sprintf("ccm-redeploy-%s-%d.log", stack.ID, time.Now().Unix())
	logPath := logFile
	prepareLogCmd := fmt.Sprintf("cd %q && : > %q", deployPath, logPath)
	prepareLogRes, err := s.ssh.RunCommand(ctx, stack.TargetID, prepareLogCmd, 10*time.Second)
	if err != nil {
		return nil, false, "", err
	}
	results = append(results, prepareLogRes)
	if prepareLogRes.ExitCode != 0 {
		// Some hosts mount deploy paths read-only for the SSH user; fall back to /tmp.
		fallbackPath := path.Join("/tmp", logFile)
		fallbackCmd := fmt.Sprintf(": > %q", fallbackPath)
		fallbackRes, ferr := s.ssh.RunCommand(ctx, stack.TargetID, fallbackCmd, 10*time.Second)
		if ferr != nil {
			return nil, false, "", ferr
		}
		results = append(results, fallbackRes)
		if fallbackRes.ExitCode != 0 {
			msg := strings.TrimSpace(fallbackRes.Stderr)
			if msg == "" {
				msg = strings.TrimSpace(fallbackRes.Stdout)
			}
			if msg == "" {
				msg = "unknown error preparing log file"
			}
			return results, false, "", fmt.Errorf("prepare redeploy log fallback %q failed: %s", fallbackPath, msg)
		}
		logPath = fallbackPath
	}

	script := buildRedeployScript(stack, logPath)
	scriptPath := path.Join("/tmp", fmt.Sprintf("ccm-redeploy-%s-%d.sh", stack.ID, time.Now().UnixNano()))
	if err := s.ssh.WriteFile(ctx, stack.TargetID, scriptPath, []byte(script), "0700", 10*time.Second); err != nil {
		return nil, false, "", fmt.Errorf("write redeploy worker script: %w", err)
	}
	detachCmd := fmt.Sprintf("cd %q && nohup sh %q >> %q 2>&1 < /dev/null & echo $!", deployPath, scriptPath, logPath)
	detachRes, err := s.ssh.RunCommand(ctx, stack.TargetID, detachCmd, 15*time.Second)
	if err != nil {
		return nil, false, "", err
	}
	results = append(results, detachRes)
	pid := strings.TrimSpace(detachRes.Stdout)
	if pid == "" {
		msg := strings.TrimSpace(detachRes.Stderr)
		if msg == "" {
			msg = "detached redeploy did not return a pid"
		}
		return results, false, "", fmt.Errorf("launch redeploy worker failed: %s", msg)
	}
	if _, perr := strconv.Atoi(pid); perr != nil {
		return results, false, "", fmt.Errorf("launch redeploy worker returned invalid pid %q", pid)
	}
	checkCmd := fmt.Sprintf("kill -0 %s", pid)
	checkRes, err := s.ssh.RunCommand(ctx, stack.TargetID, checkCmd, 5*time.Second)
	if err != nil {
		return nil, false, "", err
	}
	results = append(results, checkRes)
	return results, true, logPath, nil
}

func isLikelySelfCCMStack(stack *model.CCMStack) bool {
	if strings.EqualFold(stack.TargetID, "self") {
		return true
	}
	if strings.EqualFold(stack.ID, "ccm") {
		return true
	}
	if strings.EqualFold(path.Base(stack.DeploySubdir), "ccm") {
		return true
	}
	return false
}

func buildRedeployScript(stack *model.CCMStack, logPath string) string {
	up := "docker compose up -d"
	if stack.Flags.RemoveOrphans {
		up += " --remove-orphans"
	}
	if strings.EqualFold(stack.Flags.Recreate, "force") {
		up += " --force-recreate"
	}
	pullLine := ""
	if stack.Flags.Pull {
		pullLine = `
printf '%s [%s] %s\n' "$(date -Iseconds)" "` + stack.ID + `" "Running: docker compose pull"
docker compose pull
rc=$?
printf '%s [%s] %s\n' "$(date -Iseconds)" "` + stack.ID + `" "docker compose pull exit=$rc"
if [ "$rc" -ne 0 ]; then
  printf '%s [%s] %s\n' "$(date -Iseconds)" "` + stack.ID + `" "Redeploy failed during pull"
  exit "$rc"
fi
`
	}

	return fmt.Sprintf(`#!/bin/sh
{
  printf '%%s [%%s] %%s\n' "$(date -Iseconds)" "%s" "Redeploy started"
  printf '%%s [%%s] %%s\n' "$(date -Iseconds)" "%s" "Working directory: $(pwd)"
  printf '%%s [%%s] %%s\n' "$(date -Iseconds)" "%s" "Flags: pull=%t remove_orphans=%t recreate=%s"
  printf '%%s [%%s] %%s\n' "$(date -Iseconds)" "%s" "Running: docker compose config -q"
  docker compose config -q
  rc=$?
  printf '%%s [%%s] %%s\n' "$(date -Iseconds)" "%s" "docker compose config exit=$rc"
  if [ "$rc" -ne 0 ]; then
    printf '%%s [%%s] %%s\n' "$(date -Iseconds)" "%s" "Redeploy failed during config validation"
    exit "$rc"
  fi
%s
  printf '%%s [%%s] %%s\n' "$(date -Iseconds)" "%s" "Running: %s"
  %s
  rc=$?
  printf '%%s [%%s] %%s\n' "$(date -Iseconds)" "%s" "%s exit=$rc"
  if [ "$rc" -ne 0 ]; then
    printf '%%s [%%s] %%s\n' "$(date -Iseconds)" "%s" "Redeploy failed during up"
    exit "$rc"
  fi
  printf '%%s [%%s] %%s\n' "$(date -Iseconds)" "%s" "Running: docker compose ps"
  docker compose ps
  printf '%%s [%%s] %%s\n' "$(date -Iseconds)" "%s" "Redeploy finished successfully"
} >>%s 2>&1
`, stack.ID, stack.ID, stack.ID, stack.Flags.Pull, stack.Flags.RemoveOrphans, stack.Flags.Recreate, stack.ID, stack.ID, stack.ID, pullLine, stack.ID, up, up, stack.ID, up, stack.ID, stack.ID, stack.ID, strconv.Quote(logPath))
}

func buildEnvContent(raw string, env map[string]string) (string, int, error) {
	values := map[string]string{}
	if strings.TrimSpace(raw) != "" {
		for _, line := range strings.Split(raw, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			parts := strings.SplitN(trimmed, "=", 2)
			if len(parts) != 2 {
				return "", 0, fmt.Errorf("invalid env_file line %q", line)
			}
			key := strings.TrimSpace(parts[0])
			if !envKeyPattern.MatchString(key) {
				return "", 0, fmt.Errorf("invalid env key %q in env_file", key)
			}
			values[key] = strings.TrimSpace(parts[1])
		}
	}
	for k, v := range env {
		if !envKeyPattern.MatchString(k) {
			return "", 0, fmt.Errorf("invalid env key %q", k)
		}
		values[k] = v
	}
	if len(values) == 0 {
		return "", 0, nil
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+renderEnvValue(values[k]))
	}
	return strings.Join(lines, "\n") + "\n", len(values), nil
}

func renderEnvValue(v string) string {
	if v == "" {
		return ""
	}
	if strings.ContainsAny(v, " \t#\"'\\") {
		return strconv.Quote(v)
	}
	return v
}
