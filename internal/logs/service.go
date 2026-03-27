package logs

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/sshx"
)

type Service struct {
	cfg *config.Config
	ssh *sshx.Manager
}

func NewService(cfg *config.Config, ssh *sshx.Manager) *Service {
	return &Service{cfg: cfg, ssh: ssh}
}

func (s *Service) StreamContainerLogs(ctx context.Context, id string, tail int, out io.Writer) error {
	targetID, containerID, err := parseContainerRef(id)
	if err != nil {
		return err
	}
	if _, ok := s.cfg.Targets[targetID]; !ok {
		return fmt.Errorf("unknown target %q", targetID)
	}
	inspectCmd := "docker inspect -f '{{.HostConfig.LogConfig.Type}}' " + containerID
	res, err := s.ssh.RunCommand(ctx, targetID, inspectCmd, 5*time.Second)
	if err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(res.Stdout), "none") {
		return fmt.Errorf("container logs unavailable: Docker logging driver is set to \"none\"")
	}
	if tail < 0 {
		tail = 200
	}
	cmd := "docker logs -f --tail " + strconv.Itoa(tail) + " " + containerID
	return s.ssh.StreamLogs(ctx, targetID, cmd, out)
}

func parseContainerRef(id string) (string, string, error) {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid container id; expected target:container")
	}
	return parts[0], parts[1], nil
}
