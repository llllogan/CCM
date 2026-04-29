package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/loganjanssen/ccm/internal/logs"
	"github.com/loganjanssen/ccm/internal/model"
	ccmstatus "github.com/loganjanssen/ccm/internal/status"
)

type Service struct {
	cfg    model.CliveNotificationConfig
	status *ccmstatus.Service
	logs   *logs.Service
	client *http.Client

	mu     sync.Mutex
	states map[string]alertState
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type alertState struct {
	active bool
	last   time.Time
}

type alert struct {
	key         string
	severity    string
	title       string
	content     string
	targetID    string
	stackID     string
	containerID string
	status      string
	logs        []string
}

func NewService(cfg model.CliveNotificationConfig, statusSvc *ccmstatus.Service, logSvc *logs.Service) *Service {
	return &Service{
		cfg:    cfg,
		status: statusSvc,
		logs:   logSvc,
		client: &http.Client{Timeout: 20 * time.Second},
		states: map[string]alertState{},
	}
}

func (s *Service) Start(ctx context.Context) {
	if !s.cfg.Enabled {
		return
	}
	if strings.TrimSpace(s.cfg.WebhookURL) == "" || strings.TrimSpace(s.cfg.UserNumber) == "" {
		log.Printf("clive notifier disabled: webhook_url and user_number are required")
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.wg.Add(1)
	go s.loop(runCtx)
}

func (s *Service) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func (s *Service) loop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	s.evaluate(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.evaluate(ctx)
		}
	}
}

func (s *Service) evaluate(ctx context.Context) {
	now := time.Now()
	summary := s.status.Summary(ctx)
	active := s.alertsForSummary(ctx, summary)
	activeKeys := map[string]struct{}{}
	for _, a := range active {
		activeKeys[a.key] = struct{}{}
		if !s.shouldSend(a, now) {
			continue
		}
		if err := s.send(ctx, a, now); err != nil {
			log.Printf("clive notifier: send failed for %s: %v", a.key, err)
			continue
		}
		s.markSent(a.key, now)
	}

	s.mu.Lock()
	for key, state := range s.states {
		if !state.active {
			continue
		}
		if _, ok := activeKeys[key]; ok {
			continue
		}
		state.active = false
		s.states[key] = state
	}
	s.mu.Unlock()
}

func (s *Service) alertsForSummary(ctx context.Context, summary model.SystemSummary) []alert {
	out := []alert{}
	for _, target := range summary.Targets {
		if target.Status != "error" {
			continue
		}
		out = append(out, alert{
			key:      "target:" + target.TargetID,
			severity: "critical",
			title:    fmt.Sprintf("CCM target %s is unreachable", target.TargetID),
			content:  fmt.Sprintf("Target %s inventory failed: %s", target.TargetID, target.Error),
			targetID: target.TargetID,
			status:   target.Status,
		})
	}

	for _, stack := range summary.Stacks {
		if stack.Status == "running" {
			continue
		}
		a := alert{
			key:      "stack:" + stack.StackID,
			severity: "warning",
			title:    fmt.Sprintf("CCM stack %s is %s", stack.StackID, stack.Status),
			content:  fmt.Sprintf("Stack %s on target %s is %s.", stack.StackID, stack.TargetID, stack.Status),
			targetID: stack.TargetID,
			stackID:  stack.StackID,
			status:   stack.Status,
		}
		if stack.Reason != "" {
			a.content += " " + stack.Reason + "."
		}
		for _, c := range stack.Containers {
			if c.Status == "running" && c.Health != "unhealthy" {
				continue
			}
			a.key = "container:" + c.ID
			containerStatus := c.Status
			if c.Health != "" {
				containerStatus += " / " + c.Health
			}
			a.title = fmt.Sprintf("CCM container %s is %s", c.Name, containerStatus)
			a.content = fmt.Sprintf("Container %s in stack %s on target %s is %s. Restart count: %d.", c.Name, stack.StackID, stack.TargetID, containerStatus, c.RestartCount)
			a.containerID = c.ID
			if s.cfg.IncludeLogsTail > 0 {
				if logs, err := s.logs.ReadContainerLogs(ctx, c.ID, s.cfg.IncludeLogsTail); err == nil {
					a.logs = logs.Lines
				}
			}
			break
		}
		out = append(out, a)
	}

	for _, row := range summary.RestartFailures {
		out = append(out, alert{
			key:      "restart:" + row.Key,
			severity: "warning",
			title:    fmt.Sprintf("CCM scheduled restart failed for %s", row.StackID),
			content:  fmt.Sprintf("Scheduled restart %s has %d consecutive failure(s). Last result: %s. Last error: %s", row.Key, row.ConsecutiveFailures, row.LastResult, row.LastError),
			targetID: row.TargetID,
			stackID:  row.StackID,
			status:   row.LastResult,
		})
	}
	for _, row := range summary.ScriptFailures {
		out = append(out, alert{
			key:      "script:" + row.Key,
			severity: "warning",
			title:    fmt.Sprintf("CCM script %s failed", row.Name),
			content:  fmt.Sprintf("Script %s for stack %s has %d consecutive failure(s). Last result: %s. Last error: %s", row.Name, row.StackID, row.ConsecutiveFailures, row.LastResult, row.LastError),
			targetID: row.TargetID,
			stackID:  row.StackID,
			status:   row.LastResult,
		})
	}
	return out
}

func (s *Service) shouldSend(a alert, now time.Time) bool {
	if severityRank(a.severity) < severityRank(s.minSeverity()) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[a.key]
	if !state.active {
		state.active = true
		s.states[a.key] = state
		return true
	}
	cooldown := s.cooldown()
	return cooldown > 0 && now.Sub(state.last) >= cooldown
}

func (s *Service) markSent(key string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[key]
	state.active = true
	state.last = now
	s.states[key] = state
}

func (s *Service) send(ctx context.Context, a alert, now time.Time) error {
	payload := map[string]any{
		"source":     "ccm",
		"eventId":    fmt.Sprintf("%s:%s", a.key, now.UTC().Format("20060102T150405Z")),
		"userNumber": strings.TrimSpace(s.cfg.UserNumber),
		"severity":   a.severity,
		"title":      a.title,
		"content":    a.content,
		"actionHint": "Summarize the likely cause. Ask before restarting, redeploying, or running scripts.",
		"data": map[string]any{
			"target_id":    a.targetID,
			"stack_id":     a.stackID,
			"container_id": a.containerID,
			"status":       a.status,
			"recent_logs":  a.logs,
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(s.cfg.WebhookURL), bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	if token := strings.TrimSpace(s.cfg.Token); token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("clive webhook returned HTTP %d", res.StatusCode)
	}
	log.Printf("clive notifier: sent %s alert %s", a.severity, a.key)
	return nil
}

func (s *Service) minSeverity() string {
	raw := strings.ToLower(strings.TrimSpace(s.cfg.MinSeverity))
	if raw == "" {
		return "warning"
	}
	return raw
}

func (s *Service) cooldown() time.Duration {
	raw := strings.TrimSpace(s.cfg.Cooldown)
	if raw == "" {
		return 15 * time.Minute
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 15 * time.Minute
	}
	return d
}

func severityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	default:
		return 2
	}
}
