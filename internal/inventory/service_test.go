package inventory

import (
	"context"
	"testing"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/model"
)

func TestGlobalIncludesConfiguredStackWhileEmpty(t *testing.T) {
	target := &model.Target{ID: "host"}
	cfg := &config.Config{
		Targets: map[string]*model.Target{"host": target},
		Stacks: map[string]*model.CCMStack{
			"app": {ID: "app", TargetID: "host", DeploySubdir: "app", Target: target},
		},
	}
	service := NewService(cfg, nil, time.Minute)
	service.cache["host"] = model.TargetInventory{TargetID: "host", At: time.Now(), Projects: []model.ComposeProject{}}

	rows, _, _ := service.Global(context.Background())
	if len(rows) != 1 {
		t.Fatalf("rows = %#v, want one configured stack", rows)
	}
	if rows[0].Type != "compose" || rows[0].ID != "app" || rows[0].Status != "missing" {
		t.Fatalf("row = %#v, want missing configured stack", rows[0])
	}
}

func TestProjectChildrenKnownEmptyStack(t *testing.T) {
	target := &model.Target{ID: "host"}
	cfg := &config.Config{
		Targets: map[string]*model.Target{"host": target},
		Stacks: map[string]*model.CCMStack{
			"app": {ID: "app", TargetID: "host", DeploySubdir: "app", Target: target},
		},
	}
	service := NewService(cfg, nil, time.Minute)
	service.cache["host"] = model.TargetInventory{TargetID: "host", At: time.Now(), Projects: []model.ComposeProject{}}

	children, ok := service.ProjectChildren(context.Background(), "app")
	if !ok || children == nil || len(children) != 0 {
		t.Fatalf("children = %#v, ok = %v, want known empty stack", children, ok)
	}
	if _, ok := service.ProjectChildren(context.Background(), "unknown"); ok {
		t.Fatal("unknown item was reported as a known stack")
	}
}
