package deploy

import (
	"context"
	"errors"
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
	cfg      *config.Config
	ssh      remoteClient
	pruner   imagePruner
	mu       sync.Mutex
	lock     map[string]*sync.Mutex
	notifier DeploymentNotifier
}

// DeploymentNotifier receives a human-readable summary after a successful deployment.
type DeploymentNotifier interface {
	Notify(ctx context.Context, message string) error
}

type StreamEvent struct {
	Type     string `json:"type"`
	Phase    string `json:"phase,omitempty"`
	Command  string `json:"command,omitempty"`
	Stream   string `json:"stream,omitempty"`
	Line     string `json:"line,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Status   string `json:"status,omitempty"`
	Error    string `json:"error,omitempty"`
}

type remoteClient interface {
	RunCommand(ctx context.Context, targetID, cmd string, timeout time.Duration) (model.CommandResult, error)
	WriteFile(ctx context.Context, targetID, remotePath string, content []byte, mode string, timeout time.Duration) error
}

type streamingRemoteClient interface {
	StreamCommand(context.Context, string, string, time.Duration, func(string, string) error) (model.CommandResult, error)
}

type imagePruner interface {
	ImagePrune(ctx context.Context, targetID string) (model.DockerMaintenanceResult, error)
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var scriptFilePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+\.sh$`)

func NewService(cfg *config.Config, ssh *sshx.Manager, pruner imagePruner) *Service {
	return &Service{cfg: cfg, ssh: ssh, pruner: pruner, lock: map[string]*sync.Mutex{}}
}

func (s *Service) SetNotifier(notifier DeploymentNotifier) {
	s.notifier = notifier
}

func (s *Service) Deploy(ctx context.Context, req model.DeployRequest) (map[string]any, error) {
	return s.deploy(ctx, req, nil)
}

func (s *Service) DeployStream(ctx context.Context, req model.DeployRequest, emit func(StreamEvent) error) (map[string]any, error) {
	return s.deploy(ctx, req, emit)
}

func (s *Service) deploy(ctx context.Context, req model.DeployRequest, emit func(StreamEvent) error) (map[string]any, error) {
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
	if err := emitEvent(emit, StreamEvent{Type: "started", Phase: "validation"}); err != nil {
		return nil, err
	}

	if err := emitEvent(emit, StreamEvent{Type: "phase", Phase: "write_compose"}); err != nil {
		return nil, err
	}
	if err := s.ssh.WriteFile(ctx, stack.TargetID, path.Join(deployPath, "docker-compose.yml"), []byte(req.ComposeYML), "0644", 10*time.Second); err != nil {
		return nil, fmt.Errorf("write docker-compose.yml: %w", err)
	}

	envContent, envCount, err := buildEnvContent(req.EnvFile, req.Env)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(envContent) != "" {
		if err := emitEvent(emit, StreamEvent{Type: "phase", Phase: "write_env"}); err != nil {
			return nil, err
		}
		if err := s.ssh.WriteFile(ctx, stack.TargetID, path.Join(deployPath, ".env"), []byte(envContent), "0600", 10*time.Second); err != nil {
			return nil, fmt.Errorf("write .env: %w", err)
		}
	}
	if strings.TrimSpace(req.Caddyfile) != "" {
		if err := emitEvent(emit, StreamEvent{Type: "phase", Phase: "write_caddyfile"}); err != nil {
			return nil, err
		}
		if err := s.ssh.WriteFile(ctx, stack.TargetID, path.Join(deployPath, "Caddyfile"), []byte(req.Caddyfile), "0644", 10*time.Second); err != nil {
			return nil, fmt.Errorf("write Caddyfile: %w", err)
		}
	}
	scriptCount, err := s.writeScripts(ctx, stack.TargetID, deployPath, req.Scripts)
	if err != nil {
		return nil, err
	}

	runCompose := !isLikelySelfCCMStack(stack)
	if req.RunCompose != nil {
		runCompose = *req.RunCompose
	}
	results := []model.CommandResult{}
	if runCompose {
		var err error
		if emit == nil {
			results, err = s.runComposeUp(ctx, stack, deployPath)
		} else {
			results, err = s.runComposeUpStream(ctx, stack, deployPath, emit)
		}
		if err != nil {
			return nil, err
		}
	}

	cleanup := model.DeployCleanupResult{
		Status: "skipped",
		Reason: "compose execution disabled",
	}
	if runCompose {
		if err := emitEvent(emit, StreamEvent{Type: "phase", Phase: "image_prune"}); err != nil {
			return nil, err
		}
		cleanup = s.pruneImages(ctx, stack.TargetID)
	}

	out := map[string]any{
		"stack":        req.CCMStack,
		"target":       stack.TargetID,
		"deploy_path":  deployPath,
		"repo":         req.Repo,
		"sha":          req.SHA,
		"env_count":    envCount,
		"caddyfile":    strings.TrimSpace(req.Caddyfile) != "",
		"script_count": scriptCount,
		"run_compose":  runCompose,
		"steps":        results,
		"image_prune":  cleanup,
	}
	if notifier := s.notifierForStack(stack); notifier != nil {
		message := fmt.Sprintf("CCM deployment completed:\n    stack: %s\n    target: %s\n    path: %s\n    repo: %s\n    sha: %s\n    compose: %t\n    env_count: %d\n    scripts: %d", req.CCMStack, stack.TargetID, deployPath, valueOrManual(req.Repo), valueOrManual(req.SHA), runCompose, envCount, scriptCount)
		if err := notifier.Notify(ctx, message); err != nil {
			out["notification"] = map[string]any{"status": "failed", "error": err.Error()}
		} else {
			out["notification"] = map[string]string{"status": "sent"}
		}
	}
	return out, nil
}

func emitEvent(emit func(StreamEvent) error, event StreamEvent) error {
	if emit == nil {
		return nil
	}
	return emit(event)
}

func valueOrManual(value string) string {
	if strings.TrimSpace(value) == "" {
		return "manual"
	}
	return value
}

func (s *Service) notifierForStack(stack *model.CCMStack) DeploymentNotifier {
	if endpoint := strings.TrimSpace(stack.NotificationServiceURL); endpoint != "" {
		return NewHTTPNotifier(endpoint, s.cfg.NotificationServiceKey)
	}
	return s.notifier
}

func (s *Service) pruneImages(ctx context.Context, targetID string) model.DeployCleanupResult {
	if s.pruner == nil {
		return model.DeployCleanupResult{
			Status: "skipped",
			Reason: "image pruning not configured",
		}
	}
	result, err := s.pruner.ImagePrune(ctx, targetID)
	cleanup := model.DeployCleanupResult{
		Status: "succeeded",
		Result: &result,
	}
	if err != nil {
		cleanup.Status = "failed"
		cleanup.Error = err.Error()
	}
	return cleanup
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
	runID := strconv.FormatInt(time.Now().UnixNano(), 10)
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "================================================================")
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "REDEPLOY RUN START")
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "================================================================")

	results, async, err := s.runComposeUpSafe(ctx, stack, deployPath, logPath, runID)
	if err != nil {
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, fmt.Sprintf("REDEPLOY RUN FAILED: %v", err))
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "REDEPLOY RUN END")
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "================================================================")
		return nil, err
	}
	if async {
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "REDEPLOY RUN HANDOFF: detached worker running")
	} else {
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "REDEPLOY RUN SUCCESS")
	}
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "REDEPLOY RUN END")
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "================================================================")
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
	if notifier := s.notifierForStack(stack); notifier != nil {
		message := fmt.Sprintf("%s redeployed.\ntarget: %s\nstack: %s\npath: %s\nmode: %s", stackID, stack.TargetID, stackID, deployPath, map[bool]string{true: "async", false: "synchronous"}[async])
		if err := notifier.Notify(ctx, message); err != nil {
			out["notification"] = map[string]any{"status": "failed", "error": err.Error()}
		} else {
			out["notification"] = map[string]string{"status": "sent"}
		}
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
			return results, fmt.Errorf("docker compose pull failed: %s", commandErrSummary(res))
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
	if res.ExitCode != 0 {
		return results, fmt.Errorf("docker compose up failed: %s", commandErrSummary(res))
	}
	return results, nil
}

func (s *Service) runComposeUpStream(ctx context.Context, stack *model.CCMStack, deployPath string, emit func(StreamEvent) error) ([]model.CommandResult, error) {
	remote, ok := s.ssh.(streamingRemoteClient)
	if !ok {
		return nil, fmt.Errorf("streaming deployment is not supported by the remote client")
	}
	results := []model.CommandResult{}
	run := func(label, cmd string) error {
		if err := emitEvent(emit, StreamEvent{Type: "command", Phase: "compose", Command: label}); err != nil {
			return err
		}
		res, err := remote.StreamCommand(ctx, stack.TargetID, cmd, 10*time.Minute, func(stream, line string) error {
			return emitEvent(emit, StreamEvent{Type: "output", Phase: "compose", Command: label, Stream: stream, Line: line})
		})
		if err != nil {
			return err
		}
		results = append(results, res)
		exitCode := res.ExitCode
		if err := emitEvent(emit, StreamEvent{Type: "command_finished", Phase: "compose", Command: label, ExitCode: &exitCode}); err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%s failed: %s", label, commandErrSummary(res))
		}
		return nil
	}

	if stack.Flags.Pull {
		if err := run("docker compose pull", fmt.Sprintf("cd %q && docker compose pull", deployPath)); err != nil {
			return results, err
		}
	}
	up := "docker compose up -d"
	if stack.Flags.RemoveOrphans {
		up += " --remove-orphans"
	}
	if strings.EqualFold(stack.Flags.Recreate, "force") {
		up += " --force-recreate"
	}
	if err := run(up, fmt.Sprintf("cd %q && %s", deployPath, up)); err != nil {
		return results, err
	}
	return results, nil
}

func (s *Service) runComposeUpSafe(ctx context.Context, stack *model.CCMStack, deployPath, logPath, runID string) ([]model.CommandResult, bool, error) {
	if !isLikelySelfCCMStack(stack) {
		results, err := s.runComposeUpWithLog(ctx, stack, deployPath, logPath, runID)
		return results, false, err
	}

	results := []model.CommandResult{}
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "Self-redeploy worker launch requested")
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, fmt.Sprintf("Working directory: %s", deployPath))
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, fmt.Sprintf("Flags: pull=%t remove_orphans=%t recreate=%s", stack.Flags.Pull, stack.Flags.RemoveOrphans, stack.Flags.Recreate))

	script := buildRedeployScript(stack, runID)
	scriptPath := path.Join("/tmp", fmt.Sprintf("ccm-redeploy-%s-%d.sh", stack.ID, time.Now().UnixNano()))
	if err := s.ssh.WriteFile(ctx, stack.TargetID, scriptPath, []byte(script), "0700", 10*time.Second); err != nil {
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, fmt.Sprintf("Failed writing worker script: %v", err))
		return nil, false, fmt.Errorf("write redeploy worker script: %w", err)
	}
	detachCmd := fmt.Sprintf("cd %q && nohup sh %q >> %q 2>&1 < /dev/null & echo $!", deployPath, scriptPath, logPath)
	detachRes, err := s.ssh.RunCommand(ctx, stack.TargetID, detachCmd, 15*time.Second)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "Worker handoff command timed out; CCM may have restarted during detach. Continuing.")
			return results, true, nil
		}
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, fmt.Sprintf("Failed launching worker: %v", err))
		return nil, false, err
	}
	results = append(results, detachRes)
	pid := strings.TrimSpace(detachRes.Stdout)
	if pid == "" {
		msg := strings.TrimSpace(detachRes.Stderr)
		if msg == "" {
			msg = "detached redeploy did not return a pid"
		}
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, fmt.Sprintf("Worker launch failed: %s", msg))
		return results, false, fmt.Errorf("launch redeploy worker failed: %s", msg)
	}
	if _, perr := strconv.Atoi(pid); perr != nil {
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, fmt.Sprintf("Worker launch returned invalid pid: %q", pid))
		return results, false, fmt.Errorf("launch redeploy worker returned invalid pid %q", pid)
	}
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, fmt.Sprintf("Worker launched with pid %s", pid))
	checkCmd := fmt.Sprintf("kill -0 %s", pid)
	checkRes, err := s.ssh.RunCommand(ctx, stack.TargetID, checkCmd, 5*time.Second)
	if err != nil {
		return nil, false, err
	}
	results = append(results, checkRes)
	if checkRes.ExitCode == 0 {
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "Worker process is running")
	} else {
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "Worker process exited quickly; inspect following lines for command results")
	}
	return results, true, nil
}

func isLikelySelfCCMStack(stack *model.CCMStack) bool {
	return strings.EqualFold(stack.ID, "ccm")
}

func buildRedeployScript(stack *model.CCMStack, runID string) string {
	up := "docker compose up -d"
	if stack.Flags.RemoveOrphans {
		up += " --remove-orphans"
	}
	if strings.EqualFold(stack.Flags.Recreate, "force") {
		up += " --force-recreate"
	}
	logLine := func(msg string) string {
		return fmt.Sprintf(`printf '%%s [%%s run=%%s] %%s\n' "$(date -Iseconds)" %s %s %s`, strconv.Quote(stack.ID), strconv.Quote(runID), strconv.Quote(msg))
	}

	lines := []string{
		"#!/bin/sh",
		"{",
		"  " + logLine("================================================================"),
		"  " + logLine("REDEPLOY WORKER START"),
		"  " + logLine("Redeploy started"),
		"  " + logLine("Working directory: $(pwd)"),
		"  " + logLine(fmt.Sprintf("Flags: pull=%t remove_orphans=%t recreate=%s", stack.Flags.Pull, stack.Flags.RemoveOrphans, stack.Flags.Recreate)),
		"  " + logLine("Running: docker compose config -q"),
		"  docker compose config -q",
		"  rc=$?",
		"  " + logLine("docker compose config exit=$rc"),
		"  if [ \"$rc\" -ne 0 ]; then",
		"    " + logLine("Redeploy failed during config validation"),
		"    exit \"$rc\"",
		"  fi",
	}

	if stack.Flags.Pull {
		lines = append(lines,
			"  "+logLine("Running: docker compose pull"),
			"  docker compose pull",
			"  rc=$?",
			"  "+logLine("docker compose pull exit=$rc"),
			"  if [ \"$rc\" -ne 0 ]; then",
			"    "+logLine("Redeploy failed during pull"),
			"    exit \"$rc\"",
			"  fi",
		)
	}

	lines = append(lines,
		"  "+logLine("Running: "+up),
		"  "+up,
		"  rc=$?",
		"  "+logLine(up+" exit=$rc"),
		"  if [ \"$rc\" -ne 0 ]; then",
		"    "+logLine("Redeploy failed during up"),
		"    exit \"$rc\"",
		"  fi",
		"  "+logLine("Running: docker compose ps"),
		"  docker compose ps",
		"  "+logLine("Redeploy finished successfully"),
		"  "+logLine("REDEPLOY WORKER END"),
		"  "+logLine("================================================================"),
		"}",
		"",
	)

	return strings.Join(lines, "\n")
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

func (s *Service) runComposeUpWithLog(ctx context.Context, stack *model.CCMStack, deployPath, logPath, runID string) ([]model.CommandResult, error) {
	results := []model.CommandResult{}
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "Redeploy started")
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, fmt.Sprintf("Working directory: %s", deployPath))
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, fmt.Sprintf("Flags: pull=%t remove_orphans=%t recreate=%s", stack.Flags.Pull, stack.Flags.RemoveOrphans, stack.Flags.Recreate))

	if stack.Flags.Pull {
		cmd := fmt.Sprintf("cd %q && docker compose pull", deployPath)
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "Running: docker compose pull")
		res, err := s.ssh.RunCommand(ctx, stack.TargetID, cmd, 10*time.Minute)
		if err != nil {
			_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, fmt.Sprintf("docker compose pull failed to execute: %v", err))
			return nil, err
		}
		results = append(results, res)
		_ = s.appendCommandResultToLog(ctx, stack, deployPath, logPath, runID, "docker compose pull", res)
		if res.ExitCode != 0 {
			_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "Redeploy stopped after pull failure")
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
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, fmt.Sprintf("Running: %s", up))
	res, err := s.ssh.RunCommand(ctx, stack.TargetID, cmd, 10*time.Minute)
	if err != nil {
		_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, fmt.Sprintf("%s failed to execute: %v", up, err))
		return nil, err
	}
	results = append(results, res)
	_ = s.appendCommandResultToLog(ctx, stack, deployPath, logPath, runID, up, res)
	_ = s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "Redeploy finished")
	return results, nil
}

func (s *Service) appendCommandResultToLog(ctx context.Context, stack *model.CCMStack, deployPath, logPath, runID, command string, res model.CommandResult) error {
	if err := s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, fmt.Sprintf("%s exit=%d", command, res.ExitCode)); err != nil {
		return err
	}
	stdout := strings.TrimSpace(res.Stdout)
	if stdout != "" {
		for _, line := range strings.Split(stdout, "\n") {
			if err := s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "stdout: "+line); err != nil {
				return err
			}
		}
	}
	stderr := strings.TrimSpace(res.Stderr)
	if stderr != "" {
		for _, line := range strings.Split(stderr, "\n") {
			if err := s.appendRedeployLog(ctx, stack.TargetID, deployPath, logPath, stack.ID, runID, "stderr: "+line); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) appendRedeployLog(ctx context.Context, targetID, deployPath, logPath, stackID, runID, message string) error {
	line := fmt.Sprintf("%s [%s run=%s] %s", time.Now().Format(time.RFC3339), stackID, runID, message)
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

func (s *Service) writeScripts(ctx context.Context, targetID, deployPath string, scripts []model.DeployScript) (int, error) {
	if len(scripts) == 0 {
		return 0, nil
	}

	seen := map[string]struct{}{}
	count := 0
	for _, script := range scripts {
		file := strings.TrimSpace(script.File)
		if file == "" {
			return 0, fmt.Errorf("script file is required")
		}
		if !scriptFilePattern.MatchString(file) {
			return 0, fmt.Errorf("script file %q is invalid; expected %s", file, scriptFilePattern.String())
		}
		if _, exists := seen[file]; exists {
			return 0, fmt.Errorf("duplicate script file %q", file)
		}
		seen[file] = struct{}{}

		if strings.TrimSpace(script.Content) == "" {
			return 0, fmt.Errorf("script %q content is required", file)
		}
		scriptPath := path.Join(deployPath, "ccm_scripts", file)
		if err := s.ssh.WriteFile(ctx, targetID, scriptPath, []byte(script.Content), "0755", 10*time.Second); err != nil {
			return 0, fmt.Errorf("write script %q: %w", file, err)
		}
		count++
	}

	return count, nil
}
