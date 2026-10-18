package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	wire "opencode2api/internal/protocol"
)

const modelCatalogCacheSchemaVersion = 4

var modelCatalogCacheWriteMu sync.Mutex

type modelCatalogCache struct {
	SchemaVersion   int                      `json:"schema_version"`
	UpdatedAt       time.Time                `json:"updated_at"`
	Zen             []string                 `json:"zen"`
	NativeProtocols map[string]wire.Protocol `json:"native_protocols"`
	Unsupported     map[string]bool          `json:"unsupported"`
	Metadata        map[string]Metadata      `json:"metadata,omitempty"`
}

func (c *Catalog) LoadCache(path string) error {
	cache, err := loadModelCatalogCache(path)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.cachePath == "" {
		c.cachePath = path
	}
	c.state.Store(&catalogState{
		zen:             toSet(cache.Zen),
		nativeProtocols: cloneProtocols(cache.NativeProtocols),
		unsupported:     cloneBools(cache.Unsupported),
		modelMeta:       cloneMetadataMap(cache.Metadata),
		sortedIDs:       cache.Zen,
		updatedAt:       cache.UpdatedAt.UTC(),
		cacheSource:     "disk",
		stale:           true,
	})
	c.mu.Unlock()
	return nil
}

func (c *Catalog) SaveCache() error {
	c.mu.RLock()
	path := c.cachePath
	c.mu.RUnlock()
	if path == "" {
		return nil
	}
	s := c.state.Load()
	cache := modelCatalogCache{
		SchemaVersion:   modelCatalogCacheSchemaVersion,
		UpdatedAt:       s.updatedAt.UTC(),
		Zen:             append([]string(nil), s.sortedIDs...),
		NativeProtocols: cloneProtocols(s.nativeProtocols),
		Unsupported:     cloneBools(s.unsupported),
		Metadata:        cloneMetadataMap(s.modelMeta),
	}
	return saveModelCatalogCache(path, cache)
}

func CatalogCachePath(configPath string) string {
	if configPath == "" {
		return ""
	}
	return configPath + ".models.catalog.json"
}

func loadModelCatalogCache(path string) (modelCatalogCache, error) {
	if path == "" {
		return modelCatalogCache{}, os.ErrNotExist
	}
	file, err := os.Open(path)
	if err != nil {
		return modelCatalogCache{}, err
	}
	defer file.Close()
	var cache modelCatalogCache
	decoder := json.NewDecoder(io.LimitReader(file, 32<<20))
	if err := decoder.Decode(&cache); err != nil {
		return modelCatalogCache{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return modelCatalogCache{}, errors.New("model catalog cache contains multiple JSON values")
		}
		return modelCatalogCache{}, err
	}
	if cache.SchemaVersion != modelCatalogCacheSchemaVersion {
		return modelCatalogCache{}, fmt.Errorf("unsupported model catalog cache schema version %d", cache.SchemaVersion)
	}
	if cache.UpdatedAt.IsZero() {
		return modelCatalogCache{}, errors.New("model catalog cache is missing updated_at")
	}
	cache.Zen = normalizeModelIDs(cache.Zen)
	if len(cache.Zen) == 0 {
		return modelCatalogCache{}, errors.New("model catalog cache is empty")
	}
	for model, protocol := range cache.NativeProtocols {
		if model == "" || !wire.Valid(protocol) {
			return modelCatalogCache{}, fmt.Errorf("model catalog cache contains invalid protocol for %q", model)
		}
	}
	for model := range cache.Unsupported {
		if strings.TrimSpace(model) == "" {
			return modelCatalogCache{}, errors.New("model catalog cache contains an empty unsupported model")
		}
	}
	cache.NativeProtocols = cloneProtocols(cache.NativeProtocols)
	cache.Unsupported = cloneBools(cache.Unsupported)
	cache.UpdatedAt = cache.UpdatedAt.UTC()
	return cache, nil
}

func normalizeModelIDs(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	result := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	sort.Strings(result)
	return result
}

func saveModelCatalogCache(path string, cache modelCatalogCache) error {
	if path == "" {
		return nil
	}
	modelCatalogCacheWriteMu.Lock()
	defer modelCatalogCacheWriteMu.Unlock()
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".models-catalog-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}

	if runtime.GOOS == "windows" {
		backup := path + ".replace"
		if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
			return err
		}
		hadOld := false
		if _, statErr := os.Stat(path); statErr == nil {
			if err := os.Rename(path, backup); err != nil {
				return err
			}
			hadOld = true
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		if err := os.Rename(tempPath, path); err != nil {
			if hadOld {
				_ = os.Rename(backup, path)
			}
			return err
		}
		if hadOld {
			_ = os.Remove(backup)
		}
		return nil
	}
	return os.Rename(tempPath, path)
}
