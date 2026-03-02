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
	logPath, err := s.resolveRedeployLogPath(ctx, stack.TargetID, deployPath, stack.ID)
	if err != nil {
		return nil, err
	}
	results, async, err := s.runComposeUpSafe(ctx, stack, deployPath, logPath)
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
		"log_path":    logPath,
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

func (s *Service) runComposeUpSafe(ctx context.Context, stack *model.CCMStack, deployPath, logPath string) ([]model.CommandResult, bool, error) {
	if !isLikelySelfCCMStack(stack) {
		results, err := s.runComposeUpWithLog(ctx, stack, deployPath, logPath)
		return results, false, err
	}

	results := []model.CommandResult{}
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, "Self-redeploy worker launch requested")
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, fmt.Sprintf("Working directory: %s", deployPath))
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, fmt.Sprintf("Flags: pull=%t remove_orphans=%t recreate=%s", stack.Flags.Pull, stack.Flags.RemoveOrphans, stack.Flags.Recreate))

	script := buildRedeployScript(stack)
	scriptPath := path.Join("/tmp", fmt.Sprintf("ccm-redeploy-%s-%d.sh", stack.ID, time.Now().UnixNano()))
	if err := s.ssh.WriteFile(ctx, stack.TargetID, scriptPath, []byte(script), "0700", 10*time.Second); err != nil {
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, fmt.Sprintf("Failed writing worker script: %v", err))
		return nil, false, fmt.Errorf("write redeploy worker script: %w", err)
	}
	detachCmd := fmt.Sprintf("cd %q && nohup sh %q >> %q 2>&1 < /dev/null & echo $!", deployPath, scriptPath, logPath)
	detachRes, err := s.ssh.RunCommand(ctx, stack.TargetID, detachCmd, 15*time.Second)
	if err != nil {
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, fmt.Sprintf("Failed launching worker: %v", err))
		return nil, false, err
	}
	results = append(results, detachRes)
	pid := strings.TrimSpace(detachRes.Stdout)
	if pid == "" {
		msg := strings.TrimSpace(detachRes.Stderr)
		if msg == "" {
			msg = "detached redeploy did not return a pid"
		}
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, fmt.Sprintf("Worker launch failed: %s", msg))
		return results, false, fmt.Errorf("launch redeploy worker failed: %s", msg)
	}
	if _, perr := strconv.Atoi(pid); perr != nil {
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, fmt.Sprintf("Worker launch returned invalid pid: %q", pid))
		return results, false, fmt.Errorf("launch redeploy worker returned invalid pid %q", pid)
	}
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, fmt.Sprintf("Worker launched with pid %s", pid))
	checkCmd := fmt.Sprintf("kill -0 %s", pid)
	checkRes, err := s.ssh.RunCommand(ctx, stack.TargetID, checkCmd, 5*time.Second)
	if err != nil {
		return nil, false, err
	}
	results = append(results, checkRes)
	if checkRes.ExitCode == 0 {
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, "Worker process is running")
	} else {
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, "Worker process exited quickly; inspect following lines for command results")
	}
	return results, true, nil
}

func isLikelySelfCCMStack(stack *model.CCMStack) bool {
	return strings.EqualFold(stack.ID, "ccm")
}

func buildRedeployScript(stack *model.CCMStack) string {
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
printf '%s [%s] %s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "` + stack.ID + `" "Running: docker compose pull"
docker compose pull
rc=$?
printf '%s [%s] %s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "` + stack.ID + `" "docker compose pull exit=$rc"
if [ "$rc" -ne 0 ]; then
  printf '%s [%s] %s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "` + stack.ID + `" "Redeploy failed during pull"
  exit "$rc"
fi
`
	}

	return fmt.Sprintf(`#!/bin/sh
{
  printf '%%s [%%s] %%s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "%s" "Redeploy started"
  printf '%%s [%%s] %%s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "%s" "Working directory: $(pwd)"
  printf '%%s [%%s] %%s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "%s" "Flags: pull=%t remove_orphans=%t recreate=%s"
  printf '%%s [%%s] %%s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "%s" "Running: docker compose config -q"
  docker compose config -q
  rc=$?
  printf '%%s [%%s] %%s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "%s" "docker compose config exit=$rc"
  if [ "$rc" -ne 0 ]; then
    printf '%%s [%%s] %%s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "%s" "Redeploy failed during config validation"
    exit "$rc"
  fi
%s
  printf '%%s [%%s] %%s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "%s" "Running: %s"
  %s
  rc=$?
  printf '%%s [%%s] %%s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "%s" "%s exit=$rc"
  if [ "$rc" -ne 0 ]; then
    printf '%%s [%%s] %%s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "%s" "Redeploy failed during up"
    exit "$rc"
  fi
  printf '%%s [%%s] %%s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "%s" "Running: docker compose ps"
  docker compose ps
  printf '%%s [%%s] %%s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "%s" "Redeploy finished successfully"
}
`, stack.ID, stack.ID, stack.ID, stack.Flags.Pull, stack.Flags.RemoveOrphans, stack.Flags.Recreate, stack.ID, stack.ID, stack.ID, pullLine, stack.ID, up, up, stack.ID, up, stack.ID, stack.ID, stack.ID)
}

func (s *Service) resolveRedeployLogPath(ctx context.Context, targetID, deployPath, stackID string) (string, error) {
	logFile := fmt.Sprintf("ccm-redeploy-%s.log", stackID)
	prepCmd := fmt.Sprintf("cd %q && touch %q", deployPath, logFile)
	prepRes, err := s.ssh.RunCommand(ctx, targetID, prepCmd, 10*time.Second)
	if err != nil {
		return "", err
	}
	if prepRes.ExitCode == 0 {
		return logFile, nil
	}

	fallback := path.Join("/tmp", logFile)
	fallbackCmd := fmt.Sprintf("touch %q", fallback)
	fallbackRes, err := s.ssh.RunCommand(ctx, targetID, fallbackCmd, 10*time.Second)
	if err != nil {
		return "", err
	}
	if fallbackRes.ExitCode != 0 {
		return "", fmt.Errorf("prepare redeploy log failed in deploy path and fallback path: %s", commandErrSummary(fallbackRes))
	}
	return fallback, nil
}

func (s *Service) runComposeUpWithLog(ctx context.Context, stack *model.CCMStack, deployPath, logPath string) ([]model.CommandResult, error) {
	results := []model.CommandResult{}
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, "Redeploy started")
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, fmt.Sprintf("Working directory: %s", deployPath))
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, fmt.Sprintf("Flags: pull=%t remove_orphans=%t recreate=%s", stack.Flags.Pull, stack.Flags.RemoveOrphans, stack.Flags.Recreate))

	if stack.Flags.Pull {
		cmd := fmt.Sprintf("cd %q && docker compose pull", deployPath)
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, "Running: docker compose pull")
		res, err := s.ssh.RunCommand(ctx, stack.TargetID, cmd, 10*time.Minute)
		if err != nil {
			_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, fmt.Sprintf("docker compose pull failed to execute: %v", err))
			return nil, err
		}
		results = append(results, res)
		_ = s.appendCommandResultToLog(ctx, stack, deployPath, logPath, "docker compose pull", res)
		if res.ExitCode != 0 {
			_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, "Redeploy stopped after pull failure")
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
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, fmt.Sprintf("Running: %s", up))
	res, err := s.ssh.RunCommand(ctx, stack.TargetID, cmd, 10*time.Minute)
	if err != nil {
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, fmt.Sprintf("%s failed to execute: %v", up, err))
		return nil, err
	}
	results = append(results, res)
	_ = s.appendCommandResultToLog(ctx, stack, deployPath, logPath, up, res)
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, "Redeploy finished")
	return results, nil
}

func (s *Service) appendCommandResultToLog(ctx context.Context, stack *model.CCMStack, deployPath, logPath, command string, res model.CommandResult) error {
	if err := s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, fmt.Sprintf("%s exit=%d", command, res.ExitCode)); err != nil {
		return err
	}
	stdout := strings.TrimSpace(res.Stdout)
	if stdout != "" {
		for _, line := range strings.Split(stdout, "\n") {
			if err := s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, "stdout: "+line); err != nil {
				return err
			}
		}
	}
	stderr := strings.TrimSpace(res.Stderr)
	if stderr != "" {
		for _, line := range strings.Split(stderr, "\n") {
			if err := s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, "stderr: "+line); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) appendRedeployLog(ctx context.Context, targetID, deployPath, logPath, stackID, message string) error {
	line := fmt.Sprintf("%s [%s] %s", time.Now().Format(time.RFC3339), stackID, message)
	var cmd string
	if path.IsAbs(logPath) {
		cmd = fmt.Sprintf("printf '%%s\\n' %s >> %q", strconv.Quote(line), logPath)
	} else {
		cmd = fmt.Sprintf("cd %q && printf '%%s\\n' %s >> %q", deployPath, strconv.Quote(line), logPath)
	}
	res, err := s.ssh.RunCommand(ctx, targetID, cmd, 10*time.Second)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("append redeploy log failed: %s", commandErrSummary(res))
	}
	return nil
}

func commandErrSummary(res model.CommandResult) string {
	msg := strings.TrimSpace(res.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(res.Stdout)
	}
	if msg == "" {
		msg = fmt.Sprintf("exit %d", res.ExitCode)
	}
	return msg
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
