package logs

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/model"
	"github.com/loganjanssen/ccm/internal/sshx"
)

const maxReadLogBytes = 512 * 1024

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

func (s *Service) ReadContainerLogs(ctx context.Context, id string, tail int) (model.ContainerLogsResponse, error) {
	targetID, containerID, err := parseContainerRef(id)
	if err != nil {
		return model.ContainerLogsResponse{}, err
	}
	if _, ok := s.cfg.Targets[targetID]; !ok {
		return model.ContainerLogsResponse{}, fmt.Errorf("unknown target %q", targetID)
	}
	if err := s.ensureLogsAvailable(ctx, targetID, containerID); err != nil {
		return model.ContainerLogsResponse{}, err
	}
	if tail <= 0 {
		tail = 250
	}
	if tail > 1000 {
		tail = 1000
	}

	cmd := "docker logs --tail " + strconv.Itoa(tail) + " " + containerID
	res, err := s.ssh.RunCommand(ctx, targetID, cmd, 15*time.Second)
	if err != nil {
		return model.ContainerLogsResponse{}, err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		if msg == "" {
			msg = fmt.Sprintf("docker logs exited %d", res.ExitCode)
		}
		return model.ContainerLogsResponse{}, fmt.Errorf("%s", msg)
	}

	raw := res.Stdout
	truncated := false
	if len(raw) > maxReadLogBytes {
		raw = raw[len(raw)-maxReadLogBytes:]
		truncated = true
		if idx := strings.IndexByte(raw, '\n'); idx >= 0 && idx+1 < len(raw) {
			raw = raw[idx+1:]
		}
	}
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.TrimRight(raw, "\n")
	lines := []string{}
	if raw != "" {
		lines = strings.Split(raw, "\n")
	}
	return model.ContainerLogsResponse{
		ContainerID: id,
		Tail:        tail,
		Truncated:   truncated,
		Lines:       lines,
	}, nil
}

func (s *Service) ensureLogsAvailable(ctx context.Context, targetID, containerID string) error {
	inspectCmd := "docker inspect -f '{{.HostConfig.LogConfig.Type}}' " + containerID
	res, err := s.ssh.RunCommand(ctx, targetID, inspectCmd, 5*time.Second)
	if err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(res.Stdout), "none") {
		return fmt.Errorf("container logs unavailable: Docker logging driver is set to \"none\"")
	}
	return nil
}

func parseContainerRef(id string) (string, string, error) {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid container id; expected target:container")
	}
	return parts[0], parts[1], nil
}
