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

type fakeImagePruner struct {
	targets []string
	result  model.DockerMaintenanceResult
	err     error
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
