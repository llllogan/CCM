package restart

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/cronexpr"
	"github.com/loganjanssen/ccm/internal/model"
	"github.com/loganjanssen/ccm/internal/sshx"
)

const noMatchExitCode = 3

type Service struct {
	cfg         *config.Config
	ssh         *sshx.Manager
	statePath   string
	assignments []assignment

	mu    sync.Mutex
	state map[string]model.RestartTrackingEntry

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type assignment struct {
	key               string
	scope             string
	stackID           string
	targetID          string
	projectName       string
	containerSelector string
	strategyName      string
	cron              string
	tzName            string
	loc               *time.Location
	spec              cronexpr.Spec
	excludeContainers map[string]struct{}
}

func NewService(cfg *config.Config, ssh *sshx.Manager) (*Service, error) {
	assignments, err := buildAssignments(cfg)
	if err != nil {
		return nil, err
	}

	statePath := strings.TrimSpace(cfg.RestartStateFile)
	if statePath == "" {
		statePath = "/tmp/ccm-restart-state.json"
	}

	s := &Service{
		cfg:         cfg,
		ssh:         ssh,
		statePath:   statePath,
		assignments: assignments,
		state:       map[string]model.RestartTrackingEntry{},
	}
	s.loadState()
	s.seedState()
	_ = s.persistState()
	return s, nil
}

func (s *Service) Start(ctx context.Context) {
	if len(s.assignments) == 0 {
		return
	}
	if s.cancel != nil {
		return
	}
	workerCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		s.evaluate(workerCtx, time.Now())
		for {
			select {
			case <-workerCtx.Done():
				return
			case now := <-ticker.C:
				s.evaluate(workerCtx, now)
			}
		}
	}()
}

func (s *Service) Stop() {
	if s.cancel != nil {
		s.cancel()
		s.wg.Wait()
		s.cancel = nil
	}
	_ = s.persistState()
}

func (s *Service) Snapshot() []model.RestartTrackingEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.RestartTrackingEntry, 0, len(s.state))
	for _, v := range s.state {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StackID == out[j].StackID {
			return out[i].Key < out[j].Key
		}
		return out[i].StackID < out[j].StackID
	})
	return out
}

func (s *Service) evaluate(ctx context.Context, now time.Time) {
	for _, a := range s.assignments {
		localNow := now.In(a.loc)
		if !a.spec.Match(localNow) {
			continue
		}
		minuteKey := localNow.Format("2006-01-02T15:04")
		if !s.markAttempt(a.key, minuteKey, now) {
			continue
		}

		res, err := s.runAssignment(ctx, a)
		s.recordResult(a.key, now, res, err)
	}
}

func (s *Service) markAttempt(key, minuteKey string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.state[key]
	if entry.LastEvaluatedMinute == minuteKey {
		return false
	}
	entry.LastEvaluatedMinute = minuteKey
	entry.LastAttemptAt = now
	s.state[key] = entry
	return true
}

func (s *Service) recordResult(key string, now time.Time, res model.CommandResult, err error) {
	s.mu.Lock()
	entry := s.state[key]
	if err != nil {
		entry.LastResult = "failed"
		entry.LastError = err.Error()
		entry.LastExitCode = 0
		entry.ConsecutiveFailures++
		s.state[key] = entry
		s.mu.Unlock()
		_ = s.persistState()
		return
	}

	entry.LastExitCode = res.ExitCode
	entry.LastError = ""
	switch res.ExitCode {
	case 0:
		entry.LastResult = "success"
		entry.LastSuccessAt = now
		entry.ScheduledRestarts++
		entry.ConsecutiveFailures = 0
	case noMatchExitCode:
		entry.LastResult = "skipped_no_match"
		entry.ConsecutiveFailures = 0
	default:
		entry.LastResult = "failed"
		entry.ConsecutiveFailures++
	}
	s.state[key] = entry
	s.mu.Unlock()
	_ = s.persistState()
}

func (s *Service) runAssignment(ctx context.Context, a assignment) (model.CommandResult, error) {
	cmd := ""
	if a.scope == "stack" {
		cmd = buildStackCommand(a.projectName, a.excludeContainers)
	} else {
		cmd = buildContainerCommand(a.projectName, a.containerSelector)
	}

	res, err := s.ssh.RunCommand(ctx, a.targetID, cmd, 45*time.Second)
	if err != nil {
		log.Printf("restart scheduler: %s failed on %s: %v", a.key, a.targetID, err)
		return res, err
	}
	if res.ExitCode == 0 {
		log.Printf("restart scheduler: %s restarted on %s", a.key, a.targetID)
	} else if res.ExitCode == noMatchExitCode {
		log.Printf("restart scheduler: %s no running containers matched", a.key)
	} else {
		log.Printf("restart scheduler: %s command failed with exit=%d", a.key, res.ExitCode)
	}
	return res, nil
}

func buildAssignments(cfg *config.Config) ([]assignment, error) {
	if len(cfg.RestartStrategies) == 0 {
		return nil, nil
	}

	stackIDs := make([]string, 0, len(cfg.Stacks))
	for id := range cfg.Stacks {
		stackIDs = append(stackIDs, id)
	}
	sort.Strings(stackIDs)

	out := make([]assignment, 0)
	for _, stackID := range stackIDs {
		stack := cfg.Stacks[stackID]
		if stack == nil {
			continue
		}
		project := filepath.Base(stack.DeploySubdir)

		explicit := map[string]struct{}{}
		for name := range stack.Restart.Containers {
			explicit[name] = struct{}{}
		}

		stackStrategy := strings.TrimSpace(stack.Restart.Strategy)
		if stackStrategy != "" {
			a, err := newAssignment(cfg, "stack:"+stackID, "stack", stackID, stack.TargetID, project, "", stackStrategy)
			if err != nil {
				return nil, err
			}
			a.excludeContainers = explicit
			out = append(out, a)
		}

		containerNames := make([]string, 0, len(stack.Restart.Containers))
		for name := range stack.Restart.Containers {
			containerNames = append(containerNames, name)
		}
		sort.Strings(containerNames)
		for _, name := range containerNames {
			pref := stack.Restart.Containers[name]
			strategy := strings.TrimSpace(pref.Strategy)
			if strategy == "" || strings.EqualFold(strategy, "inherit") {
				strategy = stackStrategy
			}
			if strategy == "" || strings.EqualFold(strategy, "none") {
				continue
			}
			key := fmt.Sprintf("stack:%s:container:%s", stackID, name)
			a, err := newAssignment(cfg, key, "container", stackID, stack.TargetID, project, name, strategy)
			if err != nil {
				return nil, err
			}
			out = append(out, a)
		}
	}
	return out, nil
}

func newAssignment(cfg *config.Config, key, scope, stackID, targetID, project, container, strategyName string) (assignment, error) {
	strategy, ok := cfg.RestartStrategies[strategyName]
	if !ok {
		return assignment{}, fmt.Errorf("unknown restart strategy %q", strategyName)
	}
	spec, err := cronexpr.Parse(strategy.Cron)
	if err != nil {
		return assignment{}, fmt.Errorf("parse restart strategy %q: %w", strategyName, err)
	}
	loc := time.Local
	tz := strings.TrimSpace(strategy.Timezone)
	if tz != "" {
		loc, err = time.LoadLocation(tz)
		if err != nil {
			return assignment{}, fmt.Errorf("load timezone for strategy %q: %w", strategyName, err)
		}
	} else {
		tz = time.Local.String()
	}

	return assignment{
		key:               key,
		scope:             scope,
		stackID:           stackID,
		targetID:          targetID,
		projectName:       project,
		containerSelector: container,
		strategyName:      strategyName,
		cron:              strategy.Cron,
		tzName:            tz,
		loc:               loc,
		spec:              spec,
	}, nil
}

func buildStackCommand(project string, excluded map[string]struct{}) string {
	quotedProject := shellQuote(project)
	script := []string{
		"ids=''",
		fmt.Sprintf("while IFS='|' read -r cid cname cservice; do"),
		"  [ -n \"$cid\" ] || continue",
		"  skip=0",
	}
	if len(excluded) > 0 {
		names := make([]string, 0, len(excluded))
		for name := range excluded {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			script = append(script, fmt.Sprintf("  if [ \"$cservice\" = %s ] || [ \"$cname\" = %s ]; then skip=1; fi", shellQuote(name), shellQuote(name)))
		}
	}
	script = append(script,
		"  if [ \"$skip\" -eq 0 ]; then ids=\"$ids $cid\"; fi",
		fmt.Sprintf(`done <<EOF
$(docker ps --filter label=com.docker.compose.project=%s --format '{{.ID}}|{{.Names}}|{{.Label "com.docker.compose.service"}}')
EOF`, quotedProject),
		"if [ -z \"$(echo \"$ids\" | xargs)\" ]; then",
		fmt.Sprintf("  echo %s", shellQuote("no matching running containers")),
		fmt.Sprintf("  exit %d", noMatchExitCode),
		"fi",
		"docker restart $(echo \"$ids\" | xargs)",
	)
	return "sh -lc " + shellQuote(strings.Join(script, "\n"))
}

func buildContainerCommand(project, selector string) string {
	quotedProject := shellQuote(project)
	quotedSelector := shellQuote(selector)
	script := fmt.Sprintf(`ids=$(docker ps --filter label=com.docker.compose.project=%s --format '{{.ID}}|{{.Names}}|{{.Label "com.docker.compose.service"}}' | awk -F'|' -v n=%s '$2==n || $3==n {print $1}' | xargs)
if [ -z "$ids" ]; then
  echo 'no matching running containers'
  exit %d
fi
docker restart $ids`, quotedProject, quotedSelector, noMatchExitCode)
	return "sh -lc " + shellQuote(script)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func (s *Service) seedState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.assignments {
		entry, ok := s.state[a.key]
		if !ok {
			entry = model.RestartTrackingEntry{Key: a.key}
		}
		entry.Scope = a.scope
		entry.StackID = a.stackID
		entry.TargetID = a.targetID
		entry.ProjectName = a.projectName
		entry.ContainerSelector = a.containerSelector
		entry.Strategy = a.strategyName
		entry.Cron = a.cron
		entry.Timezone = a.tzName
		s.state[a.key] = entry
	}
}

func (s *Service) loadState() {
	b, err := os.ReadFile(s.statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("restart scheduler: read state failed: %v", err)
		}
		return
	}
	var loaded []model.RestartTrackingEntry
	if err := json.Unmarshal(b, &loaded); err != nil {
		log.Printf("restart scheduler: parse state failed: %v", err)
		return
	}
	for _, e := range loaded {
		if strings.TrimSpace(e.Key) == "" {
			continue
		}
		s.state[e.Key] = e
	}
}

func (s *Service) persistState() error {
	s.mu.Lock()
	entries := make([]model.RestartTrackingEntry, 0, len(s.state))
	for _, e := range s.state {
		entries = append(entries, e)
	}
	s.mu.Unlock()

	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })

	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.statePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath)
}
