package disk

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
	"github.com/loganjanssen/ccm/internal/model"
	"github.com/loganjanssen/ccm/internal/util"
)

const (
	alertThreshold = 80
	alertInterval  = 5 * time.Minute
)

type Notifier interface {
	Notify(context.Context, string) error
}

type Monitor struct {
	cfg       *config.Config
	usage     *Service
	notifier  Notifier
	statePath string

	mu     sync.Mutex
	active map[string]bool
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewMonitor(cfg *config.Config, usage *Service, notifier Notifier) *Monitor {
	path := strings.TrimSpace(cfg.DiskAlertStateFile)
	if path == "" {
		path = "/tmp/ccm-disk-alert-state.json"
	}
	m := &Monitor{
		cfg: cfg, usage: usage, notifier: notifier, statePath: path,
		active: map[string]bool{},
	}
	m.loadState()
	return m
}

func (m *Monitor) Start(ctx context.Context) {
	if m.notifier == nil || m.cancel != nil {
		return
	}
	workerCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(alertInterval)
		defer ticker.Stop()
		m.evaluate(workerCtx)
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				m.evaluate(workerCtx)
			}
		}
	}()
}

func (m *Monitor) Stop() {
	if m.cancel == nil {
		return
	}
	m.cancel()
	m.wg.Wait()
	m.cancel = nil
	if err := m.persistState(); err != nil {
		log.Printf("disk monitor: persist state failed: %v", err)
	}
}

func (m *Monitor) evaluate(ctx context.Context) {
	ids := make([]string, 0, len(m.cfg.Targets))
	for id := range m.cfg.Targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, targetID := range ids {
		usage, err := m.usage.Usage(ctx, targetID)
		if err != nil {
			log.Printf("disk monitor: check failed for %s: %v", targetID, err)
			continue
		}

		if usage.UsagePercent <= alertThreshold {
			if m.clear(targetID) {
				if err := m.persistState(); err != nil {
					log.Printf("disk monitor: persist recovery state failed: %v", err)
				}
			}
			continue
		}

		if m.isActive(targetID) {
			continue
		}
		if err := m.notifier.Notify(ctx, formatAlert(usage)); err != nil {
			log.Printf("disk monitor: notification failed for %s: %v", targetID, err)
			continue
		}
		m.setActive(targetID)
		if err := m.persistState(); err != nil {
			log.Printf("disk monitor: persist alert state failed: %v", err)
		}
	}
}

func formatAlert(u model.DiskUsage) string {
	return fmt.Sprintf("CCM disk alert:\n    host: %s\n    path: %s\n    mount: %s\n    filesystem: %s\n    usage: %d%%\n    used: %s\n    available: %s\n    size: %s\n    checked: %s", u.TargetID, u.Path, u.Mountpoint, u.Filesystem, u.UsagePercent, u.Used, u.Available, u.Size, util.BrisbaneTime(u.At).Format(time.RFC3339))
}

func (m *Monitor) isActive(targetID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[targetID]
}

func (m *Monitor) setActive(targetID string) {
	m.mu.Lock()
	m.active[targetID] = true
	m.mu.Unlock()
}

func (m *Monitor) clear(targetID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active[targetID] {
		return false
	}
	delete(m.active, targetID)
	return true
}

func (m *Monitor) loadState() {
	b, err := os.ReadFile(m.statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("disk monitor: read state failed: %v", err)
		}
		return
	}
	var loaded map[string]bool
	if err := json.Unmarshal(b, &loaded); err != nil {
		log.Printf("disk monitor: parse state failed: %v", err)
		return
	}
	for targetID, active := range loaded {
		if active && m.cfg.Targets[targetID] != nil {
			m.active[targetID] = true
		}
	}
}

func (m *Monitor) persistState() error {
	m.mu.Lock()
	state := make(map[string]bool, len(m.active))
	for targetID, active := range m.active {
		state[targetID] = active
	}
	m.mu.Unlock()

	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.statePath), 0o755); err != nil {
		return err
	}
	tmp := m.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.statePath)
}
