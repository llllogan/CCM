package disk

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/model"
)

type Service struct {
	cfg *config.Config
	ssh commandRunner
}

type commandRunner interface {
	RunCommand(ctx context.Context, targetID, cmd string, timeout time.Duration) (model.CommandResult, error)
}

func NewService(cfg *config.Config, ssh commandRunner) *Service {
	return &Service{cfg: cfg, ssh: ssh}
}

func (s *Service) Usage(ctx context.Context, targetID string) (model.DiskUsage, error) {
	target, ok := s.cfg.Targets[targetID]
	if !ok || target == nil {
		return model.DiskUsage{}, fmt.Errorf("unknown target %q", targetID)
	}
	path := strings.TrimSpace(target.DiskPath)
	if path == "" {
		path = "/"
	}

	res, err := s.ssh.RunCommand(ctx, targetID, "df -P -h -- "+shellQuote(path), 8*time.Second)
	if err != nil {
		return model.DiskUsage{}, err
	}
	if res.ExitCode != 0 {
		message := strings.TrimSpace(res.Stderr)
		if message == "" {
			message = strings.TrimSpace(res.Stdout)
		}
		if message == "" {
			message = fmt.Sprintf("df exited %d", res.ExitCode)
		}
		return model.DiskUsage{}, fmt.Errorf("disk usage unavailable: %s", message)
	}

	usage, err := parseDF(res.Stdout)
	if err != nil {
		return model.DiskUsage{}, err
	}
	usage.TargetID = targetID
	usage.Path = path
	usage.At = time.Now()
	return usage, nil
}

func parseDF(output string) (model.DiskUsage, error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		percentText := fields[len(fields)-2]
		if !strings.HasSuffix(percentText, "%") {
			continue
		}
		percent, err := strconv.Atoi(strings.TrimSuffix(percentText, "%"))
		if err != nil || percent < 0 || percent > 100 {
			continue
		}
		mountpoint := strings.Join(fields[5:], " ")
		return model.DiskUsage{
			Filesystem:   fields[0],
			Size:         fields[1],
			Used:         fields[2],
			Available:    fields[3],
			UsagePercent: percent,
			Mountpoint:   mountpoint,
		}, nil
	}
	return model.DiskUsage{}, fmt.Errorf("disk usage unavailable: unable to parse df output")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
