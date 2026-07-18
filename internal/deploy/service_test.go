package deploy

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/model"
)

type fakeRemoteClient struct {
	commands []string
	writes   []string
	results  []model.CommandResult
}

func (f *fakeRemoteClient) RunCommand(_ context.Context, _ string, cmd string, _ time.Duration) (model.CommandResult, error) {
	f.commands = append(f.commands, cmd)
	if len(f.results) > 0 {
		result := f.results[0]
		f.results = f.results[1:]
		return result, nil
	}
	return model.CommandResult{Stdout: cmd}, nil
}

func (f *fakeRemoteClient) WriteFile(_ context.Context, _ string, remotePath string, _ []byte, _ string, _ time.Duration) error {
	f.writes = append(f.writes, remotePath)
	return nil
}

func (f *fakeRemoteClient) StreamCommand(_ context.Context, _ string, cmd string, _ time.Duration, onLine func(string, string) error) (model.CommandResult, error) {
	f.commands = append(f.commands, cmd)
	result := model.CommandResult{}
	if len(f.results) > 0 {
		result = f.results[0]
		f.results = f.results[1:]
	}
	for _, line := range strings.Split(strings.TrimSuffix(result.Stdout, "\n"), "\n") {
		if line != "" {
			if err := onLine("stdout", line); err != nil {
				return model.CommandResult{}, err
			}
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(result.Stderr, "\n"), "\n") {
		if line != "" {
			if err := onLine("stderr", line); err != nil {
				return model.CommandResult{}, err
			}
		}
	}
	return result, nil
}

type fakeImagePruner struct {
	targets []string
	result  model.DockerMaintenanceResult
	err     error
}

type fakeDeploymentNotifier struct {
	messages []string
}

func (n *fakeDeploymentNotifier) Notify(_ context.Context, message string) error {
	n.messages = append(n.messages, message)
	return nil
}

func (f *fakeImagePruner) ImagePrune(_ context.Context, targetID string) (model.DockerMaintenanceResult, error) {
	f.targets = append(f.targets, targetID)
	return f.result, f.err
}

func TestDeployPrunesImagesAfterCompose(t *testing.T) {
	remote := &fakeRemoteClient{}
	pruner := &fakeImagePruner{
		result: model.DockerMaintenanceResult{
			TargetID:  "host",
			Operation: "image-prune",
		},
	}
	service := newTestService(remote, pruner, "app")

	out, err := service.Deploy(context.Background(), model.DeployRequest{
		CCMStack:   "app",
		ComposeYML: "services: {}",
	})
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	wantCommands := []string{
		"cd \"/srv/app\" && docker compose up -d",
	}
	if !reflect.DeepEqual(remote.commands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", remote.commands, wantCommands)
	}
	if !reflect.DeepEqual(pruner.targets, []string{"host"}) {
		t.Fatalf("prune targets = %#v, want host", pruner.targets)
	}

	steps, ok := out["steps"].([]model.CommandResult)
	if !ok {
		t.Fatalf("steps type = %T, want []model.CommandResult", out["steps"])
	}
	if len(steps) != 1 {
		t.Fatalf("steps = %#v, want only compose steps", steps)
	}
	cleanup, ok := out["image_prune"].(model.DeployCleanupResult)
	if !ok {
		t.Fatalf("image_prune type = %T, want model.DeployCleanupResult", out["image_prune"])
	}
	if cleanup.Status != "succeeded" || cleanup.Result == nil || cleanup.Result.Operation != "image-prune" {
		t.Fatalf("image_prune = %#v, want succeeded result", cleanup)
	}
}

func TestDeploySkipsImagePruneWhenComposeIsDisabled(t *testing.T) {
	remote := &fakeRemoteClient{}
	pruner := &fakeImagePruner{}
	service := newTestService(remote, pruner, "ccm")

	out, err := service.Deploy(context.Background(), model.DeployRequest{
		CCMStack:   "ccm",
		ComposeYML: "services: {}",
	})
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	if len(remote.commands) != 0 {
		t.Fatalf("commands = %#v, want none", remote.commands)
	}
	if len(pruner.targets) != 0 {
		t.Fatalf("prune targets = %#v, want none", pruner.targets)
	}
	cleanup, ok := out["image_prune"].(model.DeployCleanupResult)
	if !ok {
		t.Fatalf("image_prune type = %T, want model.DeployCleanupResult", out["image_prune"])
	}
	if cleanup.Status != "skipped" {
		t.Fatalf("image_prune = %#v, want skipped", cleanup)
	}
}

func TestDeployDoesNotPruneAfterComposeFailure(t *testing.T) {
	remote := &fakeRemoteClient{
		results: []model.CommandResult{{
			Stderr:   "compose failed",
			ExitCode: 1,
		}},
	}
	pruner := &fakeImagePruner{}
	service := newTestService(remote, pruner, "app")

	_, err := service.Deploy(context.Background(), model.DeployRequest{
		CCMStack:   "app",
		ComposeYML: "services: {}",
	})
	if err == nil || !strings.Contains(err.Error(), "docker compose up failed") {
		t.Fatalf("Deploy() error = %v, want compose failure", err)
	}
	if len(pruner.targets) != 0 {
		t.Fatalf("prune targets = %#v, want none", pruner.targets)
	}
}

func TestDeployReportsImagePruneFailureWithoutFailingDeployment(t *testing.T) {
	remote := &fakeRemoteClient{}
	pruner := &fakeImagePruner{
		result: model.DockerMaintenanceResult{
			TargetID:  "host",
			Operation: "image-prune",
			ExitCode:  1,
			Stderr:    "prune failed",
		},
		err: errors.New("prune failed"),
	}
	service := newTestService(remote, pruner, "app")

	out, err := service.Deploy(context.Background(), model.DeployRequest{
		CCMStack:   "app",
		ComposeYML: "services: {}",
	})
	if err != nil {
		t.Fatalf("Deploy() error = %v, want successful deployment", err)
	}
	cleanup, ok := out["image_prune"].(model.DeployCleanupResult)
	if !ok {
		t.Fatalf("image_prune type = %T, want model.DeployCleanupResult", out["image_prune"])
	}
	if cleanup.Status != "failed" || cleanup.Error != "prune failed" || cleanup.Result == nil || cleanup.Result.ExitCode != 1 {
		t.Fatalf("image_prune = %#v, want reported failure", cleanup)
	}
}

func TestDeployStreamEmitsOutputAndCompletion(t *testing.T) {
	remote := &fakeRemoteClient{results: []model.CommandResult{{Stdout: "pulling\n"}, {Stdout: "started\n"}}}
	service := newTestService(remote, &fakeImagePruner{}, "app")
	service.cfg.Stacks["app"].Flags.Pull = true
	var events []StreamEvent

	_, err := service.DeployStream(context.Background(), model.DeployRequest{CCMStack: "app", ComposeYML: "services: {}"}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("DeployStream() error = %v", err)
	}
	if len(remote.commands) != 2 || !strings.Contains(remote.commands[0], "docker compose pull") || !strings.Contains(remote.commands[1], "docker compose up") {
		t.Fatalf("commands = %#v, want pull followed by up", remote.commands)
	}
	var sawOutput, sawFinished bool
	for _, event := range events {
		if event.Type == "output" && event.Line == "pulling" && event.Stream == "stdout" {
			sawOutput = true
		}
		if event.Type == "command_finished" && event.ExitCode != nil && *event.ExitCode == 0 {
			sawFinished = true
		}
	}
	if !sawOutput || !sawFinished {
		t.Fatalf("events = %#v, want streamed output and command completion", events)
	}
}

func TestDeployStreamStopsAfterPullFailure(t *testing.T) {
	remote := &fakeRemoteClient{results: []model.CommandResult{{Stderr: "no space left on device", ExitCode: 1}}}
	pruner := &fakeImagePruner{}
	service := newTestService(remote, pruner, "app")
	service.cfg.Stacks["app"].Flags.Pull = true
	var events []StreamEvent

	_, err := service.DeployStream(context.Background(), model.DeployRequest{CCMStack: "app", ComposeYML: "services: {}"}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "docker compose pull failed") {
		t.Fatalf("DeployStream() error = %v, want pull failure", err)
	}
	if len(remote.commands) != 1 {
		t.Fatalf("commands = %#v, want pull only", remote.commands)
	}
	if len(pruner.targets) != 0 {
		t.Fatalf("prune targets = %#v, want none", pruner.targets)
	}
}

func TestDeployNotificationUsesReadableMultilineFormat(t *testing.T) {
	remote := &fakeRemoteClient{}
	notifier := &fakeDeploymentNotifier{}
	service := newTestService(remote, &fakeImagePruner{}, "app")
	service.SetNotifier(notifier)

	_, err := service.Deploy(context.Background(), model.DeployRequest{
		CCMStack:   "app",
		Repo:       "owner/repo",
		SHA:        "abc123",
		ComposeYML: "services: {}",
	})
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("notifications = %d, want 1", len(notifier.messages))
	}
	want := "CCM deployment completed:\n    stack: app\n    target: host\n    path: /srv/app\n    repo: owner/repo\n    sha: abc123\n    compose: true\n    env_count: 0\n    scripts: 0"
	if notifier.messages[0] != want {
		t.Fatalf("message = %q, want %q", notifier.messages[0], want)
	}
}

func newTestService(remote remoteClient, pruner imagePruner, stackID string) *Service {
	target := &model.Target{
		ID:         "host",
		DeployRoot: "/srv",
	}
	stack := &model.CCMStack{
		ID:           stackID,
		TargetID:     target.ID,
		DeploySubdir: stackID,
		Target:       target,
	}
	cfg := &config.Config{
		Targets: map[string]*model.Target{target.ID: target},
		Stacks:  map[string]*model.CCMStack{stack.ID: stack},
	}
	return &Service{
		cfg:    cfg,
		ssh:    remote,
		pruner: pruner,
		lock:   map[string]*sync.Mutex{},
	}
}
