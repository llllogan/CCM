package network

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/model"
)

type commandRunner interface {
	RunCommand(context.Context, string, string, time.Duration) (model.CommandResult, error)
}

type Service struct {
	cfg *config.Config
	ssh commandRunner
}

func NewService(cfg *config.Config, ssh commandRunner) *Service {
	return &Service{cfg: cfg, ssh: ssh}
}

func (s *Service) IPInfo(ctx context.Context, targetID string) (model.TargetIPInfo, error) {
	target, ok := s.cfg.Targets[targetID]
	if !ok || target == nil {
		return model.TargetIPInfo{}, fmt.Errorf("unknown target %q", targetID)
	}

	res, err := s.ssh.RunCommand(ctx, targetID, "curl -4 -fsS --max-time 5 https://api.ipify.org", 8*time.Second)
	if err != nil {
		return model.TargetIPInfo{}, fmt.Errorf("public IP lookup failed: %w", err)
	}
	if res.ExitCode != 0 {
		message := strings.TrimSpace(res.Stderr)
		if message == "" {
			message = strings.TrimSpace(res.Stdout)
		}
		if message == "" {
			message = fmt.Sprintf("curl exited %d", res.ExitCode)
		}
		return model.TargetIPInfo{}, fmt.Errorf("public IP lookup failed: %s", message)
	}

	publicIP := strings.TrimSpace(res.Stdout)
	parsed := net.ParseIP(publicIP)
	if parsed == nil || parsed.To4() == nil {
		return model.TargetIPInfo{}, fmt.Errorf("public IP lookup returned invalid IPv4 address")
	}

	return model.TargetIPInfo{
		TargetID: targetID,
		HostIP:   strings.TrimSpace(target.Host),
		PublicIP: parsed.To4().String(),
	}, nil
}
