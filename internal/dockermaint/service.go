package dockermaint

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/model"
	"github.com/loganjanssen/ccm/internal/sshx"
)

const maxOutputBytes = 256 * 1024

var ErrMaintenanceRunning = errors.New("docker maintenance already running for target")

type Service struct {
	cfg *config.Config
	ssh *sshx.Manager

	mu      sync.Mutex
	running map[string]struct{}
}

func NewService(cfg *config.Config, ssh *sshx.Manager) *Service {
	return &Service{
		cfg:     cfg,
		ssh:     ssh,
		running: map[string]struct{}{},
	}
}

func (s *Service) DiskReport(ctx context.Context, targetID string) (model.DockerMaintenanceResult, error) {
	return s.run(ctx, targetID, "disk-report", dockerDiskReportCommand, 45*time.Second, false)
}

func (s *Service) SafePrune(ctx context.Context, targetID string) (model.DockerMaintenanceResult, error) {
	return s.run(ctx, targetID, "safe-prune", dockerSafePruneCommand, 20*time.Minute, true)
}

func (s *Service) run(ctx context.Context, targetID, operation, cmd string, timeout time.Duration, exclusive bool) (model.DockerMaintenanceResult, error) {
	if _, ok := s.cfg.Targets[targetID]; !ok {
		return model.DockerMaintenanceResult{}, fmt.Errorf("unknown target %q", targetID)
	}
	if exclusive {
		if !s.markRunning(targetID) {
			return model.DockerMaintenanceResult{}, ErrMaintenanceRunning
		}
		defer s.markFinished(targetID)
	}

	started := time.Now()
	res, err := s.ssh.RunCommand(ctx, targetID, cmd, timeout)
	out, outTruncated := truncateOutput(res.Stdout)
	stderr, stderrTruncated := truncateOutput(res.Stderr)
	result := model.DockerMaintenanceResult{
		TargetID:        targetID,
		Operation:       operation,
		StartedAt:       started,
		DurationMillis:  time.Since(started).Milliseconds(),
		ExitCode:        res.ExitCode,
		Stdout:          out,
		Stderr:          stderr,
		StdoutTruncated: outTruncated,
		StderrTruncated: stderrTruncated,
	}
	if err != nil {
		return result, err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		if msg == "" {
			msg = fmt.Sprintf("docker maintenance exited %d", res.ExitCode)
		}
		return result, fmt.Errorf("%s", msg)
	}
	return result, nil
}

func (s *Service) markRunning(targetID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.running[targetID]; ok {
		return false
	}
	s.running[targetID] = struct{}{}
	return true
}

func (s *Service) markFinished(targetID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, targetID)
}

func truncateOutput(value string) (string, bool) {
	if len(value) <= maxOutputBytes {
		return value, false
	}
	value = value[len(value)-maxOutputBytes:]
	if idx := strings.IndexByte(value, '\n'); idx >= 0 && idx+1 < len(value) {
		value = value[idx+1:]
	}
	return value, true
}

const dockerDiskReportCommand = `set -eu
echo "== Disk usage =="
df -h /
if [ -d /var/lib/docker ]; then
  echo
  echo "== /var/lib/docker size =="
  du -sh /var/lib/docker 2>/dev/null || true
fi
echo
echo "== Docker system df =="
docker system df -v`

const dockerSafePruneCommand = `set -eu
echo "== Disk before =="
df -h /
echo
echo "== Docker before =="
docker system df -v || true
echo
echo "== Pruning stopped containers older than 24h =="
docker container prune -f --filter "until=24h"
echo
echo "== Pruning unused images older than 7d =="
docker image prune -af --filter "until=168h"
echo
echo "== Pruning build cache older than 7d =="
docker builder prune -af --filter "until=168h"
echo
echo "== Pruning unused networks older than 7d =="
docker network prune -f --filter "until=168h"
echo
echo "== Disk after =="
df -h /
echo
echo "== Docker after =="
docker system df -v || true`
