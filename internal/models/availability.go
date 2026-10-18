package models

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const FreeModelCheckInterval = time.Hour
const FailedModelCheckInterval = 24 * time.Hour

type EffortProbe struct {
	Effort   string `json:"effort,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Reset    bool   `json:"-"`
}

type Availability struct {
	Model           string      `json:"model"`
	Disabled        bool        `json:"disabled"`
	CheckedAt       time.Time   `json:"checked_at,omitempty"`
	NextCheck       time.Time   `json:"next_check"`
	Reason          string      `json:"reason,omitempty"`
	Channel         string      `json:"channel,omitempty"`
	Manual          bool        `json:"manual,omitempty"`
	Generation      uint64      `json:"-"`
	ProbeVersion    int         `json:"probe_version,omitempty"`
	AutoEffort      EffortProbe `json:"auto_effort,omitempty"`
	EffortCheckedAt time.Time   `json:"effort_checked_at,omitempty"`
}

type AvailabilityStore struct {
	mu    sync.Mutex
	path  string
	items atomic.Pointer[map[string]Availability]
}

func NewAvailabilityStore(path string) (*AvailabilityStore, error) {
	items := map[string]Availability{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			if err = json.Unmarshal(b, &items); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if items == nil {
		items = map[string]Availability{}
	}
	for model, item := range items {
		if item.ProbeVersion == 0 && item.Disabled {
			item.Disabled = false
			item.NextCheck = time.Time{}
			item.Reason = "legacy_probe_recheck"
			items[model] = item
		}
		if !item.Disabled && !item.Manual && item.Reason == "manually_enabled" {
			item.Manual = true
			items[model] = item
		}
	}
	s := &AvailabilityStore{path: path}
	s.items.Store(&items)
	return s, nil
}

func (s *AvailabilityStore) Get(model string) Availability {
	items := *s.items.Load()
	item := items[model]
	item.Model = model
	return item
}

func (s *AvailabilityStore) Disabled(model string) bool {
	items := *s.items.Load()
	return items[model].Disabled
}

func (s *AvailabilityStore) SetManual(model string, manual bool, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := *s.items.Load()
	item := cur[model]
	if item.Manual == manual {
		return nil
	}
	next := maps.Clone(cur)
	item.Model = model
	item.Manual = manual
	item.Generation++
	item.ProbeVersion = 1
	if manual {
		item.Disabled = false
		item.Reason = "manually_enabled"
		item.NextCheck = now.Add(FreeModelCheckInterval)
	} else {
		item.Reason = "auto_probe"
		item.NextCheck = time.Time{}
	}
	next[model] = item
	if err := s.saveLocked(next); err != nil {
		return err
	}
	s.items.Store(&next)
	return nil
}

func (s *AvailabilityStore) Restore(model string, now time.Time) error {
	return s.SetManual(model, true, now)
}

func (s *AvailabilityStore) Record(model string, generation uint64, success bool, reason, channel string, now time.Time, effort EffortProbe) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := *s.items.Load()
	item := cur[model]
	if item.Generation != generation || item.Manual {
		return nil
	}
	item.Model = model
	if success {
		item.Disabled = false
	} else if reason == "model_unavailable" {
		item.Disabled = true
	} else if item.Disabled {
		item.CheckedAt = now
		item.ProbeVersion = 1
		if !item.NextCheck.After(now) {
			item.NextCheck = now.Add(FailedModelCheckInterval)
		}
		next := maps.Clone(cur)
		next[model] = item
		if err := s.saveLocked(next); err != nil {
			return err
		}
		s.items.Store(&next)
		return nil
	}
	item.CheckedAt, item.Reason, item.Channel = now, reason, channel
	item.ProbeVersion = 1
	if success && effort.Effort != "" {
		item.AutoEffort, item.EffortCheckedAt = effort, now
	} else if success && effort.Reset {
		item.AutoEffort, item.EffortCheckedAt = EffortProbe{}, time.Time{}
	}
	interval := FreeModelCheckInterval
	if !success && reason == "model_unavailable" {
		interval = FailedModelCheckInterval
	}
	item.NextCheck = now.Add(interval)
	next := maps.Clone(cur)
	next[model] = item
	if err := s.saveLocked(next); err != nil {
		return err
	}
	s.items.Store(&next)
	return nil
}

func (s *AvailabilityStore) saveLocked(items map[string]Availability) error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".availability-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}
