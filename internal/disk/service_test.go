package disk

import (
	"context"
	"testing"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/model"
)

type recordingRunner struct {
	command string
	result  model.CommandResult
}

func (r *recordingRunner) RunCommand(_ context.Context, _ string, command string, _ time.Duration) (model.CommandResult, error) {
	r.command = command
	return r.result, nil
}

func TestUsageParsesDFOutput(t *testing.T) {
	runner := &recordingRunner{result: model.CommandResult{
		Stdout: "Filesystem      Size  Used Avail Use% Mounted on\n/dev/sda1       100G   73G   27G  73% /\n",
	}}
	service := NewService(&config.Config{Targets: map[string]*model.Target{
		"host": {DiskPath: "/mnt/media"},
	}}, runner)

	usage, err := service.Usage(context.Background(), "host")
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	if usage.UsagePercent != 73 || usage.Filesystem != "/dev/sda1" || usage.Mountpoint != "/" {
		t.Fatalf("usage = %#v, want parsed disk usage", usage)
	}
	if runner.command != "df -P -h -- '/mnt/media'" {
		t.Fatalf("command = %q, want configured path", runner.command)
	}
}

func TestUsageQuotesDiskPath(t *testing.T) {
	runner := &recordingRunner{result: model.CommandResult{
		Stdout: "Filesystem Size Used Avail Use% Mounted on\n/dev/x 1G 1M 999M 1% /\n",
	}}
	service := NewService(&config.Config{Targets: map[string]*model.Target{
		"host": {DiskPath: "/mnt/it's-data"},
	}}, runner)

	if _, err := service.Usage(context.Background(), "host"); err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	if runner.command != "df -P -h -- '/mnt/it'\\''s-data'" {
		t.Fatalf("command = %q, want shell-quoted path", runner.command)
	}
}
