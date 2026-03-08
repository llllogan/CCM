package script

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/cronexpr"
	"github.com/loganjanssen/ccm/internal/model"
	"github.com/loganjanssen/ccm/internal/sshx"
)

var (
	ErrScriptNotFound = errors.New("script not found")
	ErrScriptRunning  = errors.New("script already running")
)

type Service struct {
	cfg         *config.Config
	ssh         *sshx.Manager
	assignments []assignment

	mu       sync.Mutex
	state    map[string]string
	tracking map[string]model.ScriptTrackingEntry

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type assignment struct {
	key        string
	stackID    string
	targetID   string
	deployPath string
	name       string
	cron       string
	spec       cronexpr.Spec
	timezone   string
	location   *time.Location
	file       string
}

func NewService(cfg *config.Config, ssh *sshx.Manager) (*Service, error) {
	assignments, err := buildAssignments(cfg)
	if err != nil {
		return nil, err
	}
	return &Service{
		cfg:         cfg,
		ssh:         ssh,
		assignments: assignments,
		state:       map[string]string{},
		tracking:    buildTracking(assignments),
	}, nil
}

func (s *Service) Start(ctx context.Context) {
	if len(s.assignments) == 0 {
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
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	s.evaluate(ctx, time.Now())

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.evaluate(ctx, now)
		}
	}
}

func (s *Service) evaluate(ctx context.Context, now time.Time) {
	for _, a := range s.assignments {
		localNow := now.In(a.location)
		minuteKey := localNow.Format("2006-01-02T15:04")
		if !a.spec.Match(localNow) {
			continue
		}
		if !s.markIfNewMinute(a.key, minuteKey) {
			continue
		}
		res, err := s.runAssignment(ctx, a, "scheduled")
		if err != nil {
			log.Printf("script scheduler: %s failed on %s: %v", a.key, a.targetID, err)
			continue
		}
		if res.ExitCode != 0 {
			msg := strings.TrimSpace(res.Stderr)
			if msg == "" {
				msg = strings.TrimSpace(res.Stdout)
			}
			if msg == "" {
				msg = fmt.Sprintf("exit %d", res.ExitCode)
			}
			log.Printf("script scheduler: %s failed on %s: %s", a.key, a.targetID, msg)
			continue
		}
		log.Printf("script scheduler: %s executed on %s", a.key, a.targetID)
	}
}

func (s *Service) markIfNewMinute(key, minuteKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state[key] == minuteKey {
		return false
	}
	s.state[key] = minuteKey
	return true
}

func (s *Service) SnapshotByStack(stackID string) []model.ScriptTrackingEntry {
	entries := make([]model.ScriptTrackingEntry, 0)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.assignments {
		if a.stackID != stackID {
			continue
		}
		if row, ok := s.tracking[a.key]; ok {
			entries = append(entries, row)
		}
	}
	return entries
}

func (s *Service) RunNow(ctx context.Context, stackID, scriptName string) (model.CommandResult, model.ScriptTrackingEntry, error) {
	a, ok := s.findAssignment(stackID, scriptName)
	if !ok {
		return model.CommandResult{}, model.ScriptTrackingEntry{}, ErrScriptNotFound
	}
	res, err := s.runAssignment(ctx, a, "manual")
	entry := s.snapshotForKey(a.key)
	return res, entry, err
}

func (s *Service) findAssignment(stackID, scriptName string) (assignment, bool) {
	for _, a := range s.assignments {
		if a.stackID == stackID && a.name == scriptName {
			return a, true
		}
	}
	return assignment{}, false
}

func (s *Service) snapshotForKey(key string) model.ScriptTrackingEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tracking[key]
}

func (s *Service) runAssignment(ctx context.Context, a assignment, trigger string) (model.CommandResult, error) {
	if !s.markRunning(a.key) {
		return model.CommandResult{}, ErrScriptRunning
	}
	defer s.markFinished(a.key)

	started := time.Now()
	cmd := fmt.Sprintf("cd %q && /bin/sh %q", a.deployPath, path.Join("ccm_scripts", a.file))
	res, err := s.ssh.RunCommand(ctx, a.targetID, cmd, 30*time.Minute)
	s.recordAttempt(a.key, trigger, started, res, err)
	return res, err
}

func (s *Service) markRunning(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.tracking[key]
	if row.Running {
		return false
	}
	row.Running = true
	s.tracking[key] = row
	return true
}

func (s *Service) markFinished(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.tracking[key]
	row.Running = false
	s.tracking[key] = row
}

func (s *Service) recordAttempt(key, trigger string, started time.Time, res model.CommandResult, runErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.tracking[key]
	row.LastAttemptAt = started
	row.LastExitCode = res.ExitCode
	if trigger == "manual" {
		row.ManualRuns++
	} else {
		row.ScheduledRuns++
	}

	if runErr != nil {
		row.LastResult = "failed"
		row.LastError = runErr.Error()
		row.ConsecutiveFailures++
		s.tracking[key] = row
		return
	}

	row.LastError = ""
	if res.ExitCode != 0 {
		row.LastResult = "failed"
		row.ConsecutiveFailures++
		s.tracking[key] = row
		return
	}
	row.LastResult = "success"
	row.LastSuccessAt = started
	row.ConsecutiveFailures = 0
	s.tracking[key] = row
}

func buildTracking(assignments []assignment) map[string]model.ScriptTrackingEntry {
	rows := make(map[string]model.ScriptTrackingEntry, len(assignments))
	for _, a := range assignments {
		rows[a.key] = model.ScriptTrackingEntry{
			Key:      a.key,
			StackID:  a.stackID,
			TargetID: a.targetID,
			Name:     a.name,
			Cron:     a.cron,
			Timezone: a.timezone,
			File:     a.file,
		}
	}
	return rows
}

func buildAssignments(cfg *config.Config) ([]assignment, error) {
	stackIDs := make([]string, 0, len(cfg.Stacks))
	for id := range cfg.Stacks {
		stackIDs = append(stackIDs, id)
	}
	sort.Strings(stackIDs)

	assignments := make([]assignment, 0)
	for _, stackID := range stackIDs {
		stack := cfg.Stacks[stackID]
		if stack == nil || len(stack.Scripts) == 0 {
			continue
		}
		deployPath := path.Join(stack.Target.DeployRoot, stack.DeploySubdir)
		for _, script := range stack.Scripts {
			spec, err := cronexpr.Parse(script.Cron)
			if err != nil {
				return nil, fmt.Errorf("parse stack %q script %q cron: %w", stackID, script.Name, err)
			}
			loc := time.Local
			tz := strings.TrimSpace(script.Timezone)
			if tz == "" {
				tz = "Local"
			} else {
				loaded, err := time.LoadLocation(tz)
				if err != nil {
					return nil, fmt.Errorf("load stack %q script %q timezone: %w", stackID, script.Name, err)
				}
				loc = loaded
			}
			assignments = append(assignments, assignment{
				key:        fmt.Sprintf("%s:%s", stackID, script.Name),
				stackID:    stackID,
				targetID:   stack.TargetID,
				deployPath: deployPath,
				name:       script.Name,
				cron:       script.Cron,
				spec:       spec,
				timezone:   tz,
				location:   loc,
				file:       script.File,
			})
		}
	}
	return assignments, nil
}
