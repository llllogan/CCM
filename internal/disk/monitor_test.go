package disk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/model"
)

type monitorRunner struct {
	outputs []string
	index   int
}

func (r *monitorRunner) RunCommand(_ context.Context, _ string, _ string, _ time.Duration) (model.CommandResult, error) {
	output := r.outputs[r.index]
	r.index++
	return model.CommandResult{Stdout: output}, nil
}

type monitorNotifier struct {
	messages []string
	err      error
}

func (n *monitorNotifier) Notify(_ context.Context, message string) error {
	if n.err != nil {
		return n.err
	}
	n.messages = append(n.messages, message)
	return nil
}

func TestMonitorAlertsOnCrossingAndRecovery(t *testing.T) {
	runner := &monitorRunner{outputs: []string{
		"Filesystem Size Used Avail Use% Mounted on\n/dev/x 100G 81G 19G 81% /\n",
		"Filesystem Size Used Avail Use% Mounted on\n/dev/x 100G 81G 19G 81% /\n",
		"Filesystem Size Used Avail Use% Mounted on\n/dev/x 100G 80G 20G 80% /\n",
		"Filesystem Size Used Avail Use% Mounted on\n/dev/x 100G 82G 18G 82% /\n",
	}}
	notifier := &monitorNotifier{}
	m := newTestMonitor(t, runner, notifier)

	for range 4 {
		m.evaluate(context.Background())
	}

	if len(notifier.messages) != 2 {
		t.Fatalf("notifications = %d, want 2", len(notifier.messages))
	}
	if got := notifier.messages[0]; got == "" || !containsAll(got, "host: host", "usage: 81%", "available: 19G") {
		t.Fatalf("message = %q, missing useful disk details", got)
	}
}

func TestFormatAlertUsesBrisbaneTime(t *testing.T) {
	message := formatAlert(model.DiskUsage{
		TargetID:     "host",
		Path:         "/",
		Mountpoint:   "/",
		Filesystem:   "/dev/x",
		UsagePercent: 81,
		Used:         "81G",
		Available:    "19G",
		Size:         "100G",
		At:           time.Date(2026, time.July, 16, 5, 3, 33, 0, time.UTC),
	})
	want := "host at 81% disk usage.\nused: 81G\navailable: 19G\nsize: 100G\nhost: host\npath: /\nmount: /\nfilesystem: /dev/x\nusage: 81%\nchecked: 15:03:33 2026-07-16"
	if message != want {
		t.Fatalf("message = %q, want %q", message, want)
	}
}

func TestMonitorRetriesFailedNotification(t *testing.T) {
	runner := &monitorRunner{outputs: []string{
		"Filesystem Size Used Avail Use% Mounted on\n/dev/x 100G 81G 19G 81% /\n",
		"Filesystem Size Used Avail Use% Mounted on\n/dev/x 100G 81G 19G 81% /\n",
	}}
	notifier := &monitorNotifier{err: errors.New("unavailable")}
	m := newTestMonitor(t, runner, notifier)
	m.evaluate(context.Background())
	notifier.err = nil
	m.evaluate(context.Background())

	if len(notifier.messages) != 1 {
		t.Fatalf("notifications = %d, want retry after failure", len(notifier.messages))
	}
}

func TestMonitorPersistsActiveState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "disk-alert.json")
	firstNotifier := &monitorNotifier{}
	first := newTestMonitorWithPath(t, statePath, &monitorRunner{outputs: []string{
		"Filesystem Size Used Avail Use% Mounted on\n/dev/x 100G 81G 19G 81% /\n",
	}}, firstNotifier)
	first.evaluate(context.Background())

	secondNotifier := &monitorNotifier{}
	second := newTestMonitorWithPath(t, statePath, &monitorRunner{outputs: []string{
		"Filesystem Size Used Avail Use% Mounted on\n/dev/x 100G 82G 18G 82% /\n",
	}}, secondNotifier)
	second.evaluate(context.Background())

	if len(secondNotifier.messages) != 0 {
		t.Fatalf("notifications after loading active state = %d, want 0", len(secondNotifier.messages))
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file: %v", err)
	}
}

func newTestMonitor(t *testing.T, runner *monitorRunner, notifier *monitorNotifier) *Monitor {
	return newTestMonitorWithPath(t, filepath.Join(t.TempDir(), "disk-alert.json"), runner, notifier)
}

func newTestMonitorWithPath(t *testing.T, statePath string, runner *monitorRunner, notifier *monitorNotifier) *Monitor {
	t.Helper()
	cfg := &config.Config{
		Targets:            map[string]*model.Target{"host": {DiskPath: "/"}},
		DiskAlertStateFile: statePath,
	}
	return NewMonitor(cfg, NewService(cfg, runner), notifier)
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
