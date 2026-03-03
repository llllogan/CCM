package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/loganjanssen/ccm/internal/cronexpr"
	"github.com/loganjanssen/ccm/internal/model"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen              string                           `yaml:"listen"`
	AuthToken           string                           `yaml:"auth_token"`
	Targets             map[string]*model.Target         `yaml:"targets"`
	Stacks              map[string]*model.CCMStack       `yaml:"stacks"`
	RestartStrategies   map[string]model.RestartStrategy `yaml:"restart_strategies"`
	RestartStateFile    string                           `yaml:"restart_state_file"`
	InventoryTTLSeconds int                              `yaml:"inventory_ttl_seconds"`
}

var stackIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	if cfg.InventoryTTLSeconds <= 0 {
		cfg.InventoryTTLSeconds = 3
	}

	resolve(cfg.Targets, cfg.Stacks)
	return &cfg, nil
}

func (c *Config) Validate() error {
	if len(c.Targets) == 0 {
		return errors.New("targets is required")
	}
	if len(c.Stacks) == 0 {
		return errors.New("stacks is required")
	}

	var errs []string
	for id, t := range c.Targets {
		if t == nil {
			errs = append(errs, fmt.Sprintf("target %q is nil", id))
			continue
		}
		if id == "" {
			errs = append(errs, "target id cannot be empty")
		}
		if t.Host == "" || t.User == "" || t.DeployRoot == "" {
			errs = append(errs, fmt.Sprintf("target %q requires host/user/deploy_root", id))
		}
		if t.Port == 0 {
			t.Port = 22
		}
	}
	for id, s := range c.Stacks {
		if !stackIDPattern.MatchString(id) {
			errs = append(errs, fmt.Sprintf("stack id %q must match %s", id, stackIDPattern.String()))
		}
		if s == nil {
			errs = append(errs, fmt.Sprintf("stack %q is nil", id))
			continue
		}
		if _, ok := c.Targets[s.TargetID]; !ok {
			errs = append(errs, fmt.Sprintf("stack %q references unknown target %q", id, s.TargetID))
		}
		if s.DeploySubdir == "" || s.DeploySubdir == "." {
			errs = append(errs, fmt.Sprintf("stack %q requires non-empty deploy_subdir", id))
		}
		clean := filepath.Clean("/" + s.DeploySubdir)
		if clean == "/" || clean == "/." || clean == "" {
			errs = append(errs, fmt.Sprintf("stack %q deploy_subdir invalid", id))
		}
		if filepath.IsAbs(s.DeploySubdir) || containsTraversal(s.DeploySubdir) {
			errs = append(errs, fmt.Sprintf("stack %q deploy_subdir must be relative and traversal-safe", id))
		}
		if ref := strings.TrimSpace(s.Restart.Strategy); ref != "" {
			if _, ok := c.RestartStrategies[ref]; !ok {
				errs = append(errs, fmt.Sprintf("stack %q references unknown restart strategy %q", id, ref))
			}
		}
		for containerName, pref := range s.Restart.Containers {
			if strings.TrimSpace(containerName) == "" {
				errs = append(errs, fmt.Sprintf("stack %q has empty restart container key", id))
				continue
			}
			ref := strings.TrimSpace(pref.Strategy)
			if ref == "" || strings.EqualFold(ref, "inherit") || strings.EqualFold(ref, "none") {
				continue
			}
			if _, ok := c.RestartStrategies[ref]; !ok {
				errs = append(errs, fmt.Sprintf("stack %q container %q references unknown restart strategy %q", id, containerName, ref))
			}
		}
	}
	for name, strategy := range c.RestartStrategies {
		if !stackIDPattern.MatchString(name) {
			errs = append(errs, fmt.Sprintf("restart strategy id %q must match %s", name, stackIDPattern.String()))
		}
		spec := strings.TrimSpace(strategy.Cron)
		if spec == "" {
			errs = append(errs, fmt.Sprintf("restart strategy %q requires cron expression", name))
			continue
		}
		if _, err := cronexpr.Parse(spec); err != nil {
			errs = append(errs, fmt.Sprintf("restart strategy %q has invalid cron: %v", name, err))
		}
		if tz := strings.TrimSpace(strategy.Timezone); tz != "" {
			if _, err := time.LoadLocation(tz); err != nil {
				errs = append(errs, fmt.Sprintf("restart strategy %q timezone invalid: %v", name, err))
			}
		}
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("config validation failed: %v", errs)
	}
	return nil
}

func resolve(targets map[string]*model.Target, stacks map[string]*model.CCMStack) {
	for id, t := range targets {
		t.ID = id
	}
	for id, s := range stacks {
		s.ID = id
		t := targets[s.TargetID]
		s.Target = t
		flags := t.Defaults
		if s.Profile != "" {
			if p, ok := t.Profiles[s.Profile]; ok {
				if p.Pull != nil {
					flags.Pull = *p.Pull
				}
				if p.RemoveOrphans != nil {
					flags.RemoveOrphans = *p.RemoveOrphans
				}
				if p.Recreate != nil {
					flags.Recreate = *p.Recreate
				}
			}
		}
		s.Flags = flags
	}
}

func containsTraversal(p string) bool {
	clean := filepath.Clean(strings.ReplaceAll(p, "\\", "/"))
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return true
		}
	}
	return strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../")
}
