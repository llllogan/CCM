package dockermaint

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/model"
)

type recordingRunner struct {
	mu       sync.Mutex
	commands []string
}

func (r *recordingRunner) RunCommand(_ context.Context, _ string, cmd string, _ time.Duration) (model.CommandResult, error) {
	r.mu.Lock()
	r.commands = append(r.commands, cmd)
	r.mu.Unlock()
	return model.CommandResult{}, nil
}

func TestImagePruneRunsExactCommand(t *testing.T) {
	runner := &recordingRunner{}
	service := newTestService(runner)

	result, err := service.ImagePrune(context.Background(), "host")
	if err != nil {
		t.Fatalf("ImagePrune() error = %v", err)
	}
	if result.Operation != "image-prune" || result.TargetID != "host" {
		t.Fatalf("result = %#v, want image-prune on host", result)
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.commands) != 1 || runner.commands[0] != "docker image prune -a -f" {
		t.Fatalf("commands = %#v, want exact image prune command", runner.commands)
	}
}

type blockingRunner struct {
	started chan struct{}
	release chan struct{}

	mu        sync.Mutex
	active    int
	maxActive int
}

func (r *blockingRunner) RunCommand(ctx context.Context, _ string, _ string, _ time.Duration) (model.CommandResult, error) {
	r.mu.Lock()
	r.active++
	if r.active > r.maxActive {
		r.maxActive = r.active
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.active--
		r.mu.Unlock()
	}()

	r.started <- struct{}{}
	select {
	case <-r.release:
	case <-ctx.Done():
		return model.CommandResult{}, ctx.Err()
	}
	return model.CommandResult{}, nil
}

func TestImagePruneIsSerializedPerTarget(t *testing.T) {
	runner := &blockingRunner{
		started: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	service := newTestService(runner)
	errs := make(chan error, 2)

	go func() {
		_, err := service.ImagePrune(context.Background(), "host")
		errs <- err
	}()
	waitForSignal(t, runner.started, "first image prune to start")

	go func() {
		_, err := service.ImagePrune(context.Background(), "host")
		errs <- err
	}()

	select {
	case <-runner.started:
		t.Fatal("second image prune started before the first completed")
	case <-time.After(50 * time.Millisecond):
	}

	runner.release <- struct{}{}
	waitForSignal(t, runner.started, "second image prune to start")
	runner.release <- struct{}{}

	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("ImagePrune() error = %v", err)
		}
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.maxActive != 1 {
		t.Fatalf("maximum concurrent image prunes = %d, want 1", runner.maxActive)
	}
}

func TestSafePruneRejectsConcurrentImagePrune(t *testing.T) {
	runner := &blockingRunner{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	service := newTestService(runner)
	done := make(chan error, 1)

	go func() {
		_, err := service.ImagePrune(context.Background(), "host")
		done <- err
	}()
	waitForSignal(t, runner.started, "image prune to start")

	_, err := service.SafePrune(context.Background(), "host")
	if !errors.Is(err, ErrMaintenanceRunning) {
		t.Fatalf("SafePrune() error = %v, want ErrMaintenanceRunning", err)
	}

	runner.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatalf("ImagePrune() error = %v", err)
	}
}

func newTestService(runner commandRunner) *Service {
	return &Service{
		cfg: &config.Config{
			Targets: map[string]*model.Target{
				"host": {ID: "host"},
			},
		},
		ssh:   runner,
		gates: map[string]chan struct{}{},
	}
}

func waitForSignal(t *testing.T, ch <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}
