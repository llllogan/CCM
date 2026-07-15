package network

import (
	"context"
	"testing"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/model"
)

type recordingRunner struct {
	result  model.CommandResult
	command string
}

func (r *recordingRunner) RunCommand(_ context.Context, _ string, command string, _ time.Duration) (model.CommandResult, error) {
	r.command = command
	return r.result, nil
}

func TestIPInfo(t *testing.T) {
	runner := &recordingRunner{result: model.CommandResult{Stdout: "203.0.113.42\n"}}
	service := NewService(&config.Config{Targets: map[string]*model.Target{
		"host": {Host: "192.0.2.10"},
	}}, runner)

	got, err := service.IPInfo(context.Background(), "host")
	if err != nil {
		t.Fatalf("IPInfo() error = %v", err)
	}
	if got.HostIP != "192.0.2.10" || got.PublicIP != "203.0.113.42" {
		t.Fatalf("IPInfo() = %#v", got)
	}
	if runner.command != "curl -4 -fsS --max-time 5 https://api.ipify.org" {
		t.Fatalf("command = %q", runner.command)
	}
}

func TestIPInfoRejectsInvalidPublicIP(t *testing.T) {
	service := NewService(&config.Config{Targets: map[string]*model.Target{
		"host": {Host: "192.0.2.10"},
	}}, &recordingRunner{result: model.CommandResult{Stdout: "not-an-ip"}})

	if _, err := service.IPInfo(context.Background(), "host"); err == nil {
		t.Fatal("IPInfo() error = nil, want invalid IP error")
	}
}
