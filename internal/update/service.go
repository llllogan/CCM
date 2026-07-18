package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/model"
)

const (
	imageRef    = "ghcr.io/llllogan/ccm:latest"
	manifestURL = "https://ghcr.io/v2/llllogan/ccm/manifests/latest"
	cacheTTL    = 5 * time.Minute
)

type commandRunner interface {
	RunCommand(context.Context, string, string, time.Duration) (model.CommandResult, error)
}

type Service struct {
	cfg        *config.Config
	ssh        commandRunner
	httpClient *http.Client

	mu   sync.Mutex
	last model.CCMUpdateStatus
}

func NewService(cfg *config.Config, ssh commandRunner) *Service {
	return &Service{cfg: cfg, ssh: ssh, httpClient: &http.Client{Timeout: 10 * time.Second}}
}

func (s *Service) Status(ctx context.Context) model.CCMUpdateStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.last.CheckedAt.IsZero() && time.Since(s.last.CheckedAt) < cacheTTL {
		return s.last
	}

	status := model.CCMUpdateStatus{CheckedAt: time.Now().UTC()}
	stack, ok := s.cfg.Stacks["ccm"]
	if !ok || stack == nil {
		status.Error = "CCM stack is not configured"
		s.last = status
		return status
	}

	current, err := s.localDigest(ctx, stack.TargetID)
	if err != nil {
		status.Error = err.Error()
		s.last = status
		return status
	}
	status.CurrentDigest = current
	latest, err := s.registryDigest(ctx)
	if err != nil {
		status.Error = err.Error()
		s.last = status
		return status
	}
	status.LatestDigest = latest
	status.Available = current != latest
	s.last = status
	return status
}

func (s *Service) localDigest(ctx context.Context, targetID string) (string, error) {
	command := fmt.Sprintf("docker image inspect --format '{{index .RepoDigests 0}}' %q", imageRef)
	result, err := s.ssh.RunCommand(ctx, targetID, command, 15*time.Second)
	if err != nil || result.ExitCode != 0 {
		return "", fmt.Errorf("inspect installed CCM image")
	}
	digest := digestFromReference(result.Stdout)
	if digest == "" {
		return "", fmt.Errorf("installed CCM image has no registry digest")
	}
	return digest, nil
}

func (s *Service) registryDigest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("check CCM registry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		token, err := s.registryToken(ctx, resp.Header.Get("Www-Authenticate"))
		if err != nil {
			return "", err
		}
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
		req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err = s.httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("check CCM registry: %w", err)
		}
		defer resp.Body.Close()
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("check CCM registry: %s", resp.Status)
	}
	digest := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest"))
	if digest == "" {
		return "", fmt.Errorf("CCM registry did not return an image digest")
	}
	return digest, nil
}

func (s *Service) registryToken(ctx context.Context, challenge string) (string, error) {
	parts := map[string]string{}
	for _, item := range strings.Split(strings.TrimPrefix(challenge, "Bearer "), ",") {
		kv := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(kv) == 2 {
			parts[kv[0]] = strings.Trim(kv[1], "\"")
		}
	}
	realm := parts["realm"]
	if realm == "" {
		return "", fmt.Errorf("CCM registry authentication challenge was invalid")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("service", parts["service"])
	q.Set("scope", parts["scope"])
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("authenticate with CCM registry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("authenticate with CCM registry: %s", resp.Status)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Token == "" {
		return "", fmt.Errorf("CCM registry returned an empty access token")
	}
	return body.Token, nil
}

func digestFromReference(value string) string {
	ref := strings.TrimSpace(value)
	if at := strings.LastIndex(ref, "@sha256:"); at >= 0 {
		return ref[at+1:]
	}
	if i := strings.Index(ref, "sha256:"); i >= 0 {
		return ref[i:]
	}
	return ""
}
