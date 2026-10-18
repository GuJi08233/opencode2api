// Package models discovers model capabilities, pricing, and available routes.
package models

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wire "opencode2api/internal/protocol"
)

type Route struct {
	ID       string
	Protocol wire.Protocol
}

type catalogState struct {
	zen             map[string]bool
	nativeProtocols map[string]wire.Protocol
	unsupported     map[string]bool
	modelMeta       map[string]Metadata
	sortedIDs       []string
	updatedAt       time.Time
	cacheSource     string
	stale           bool
}

type Catalog struct {
	state        atomic.Pointer[catalogState]
	protocols    map[string]wire.Protocol
	mu           sync.RWMutex
	pricing      *PricingStore
	availability *AvailabilityStore
	cachePath    string
	refreshAfter time.Duration
}

type CatalogSnapshot struct {
	Total       int       `json:"total"`
	Exposed     int       `json:"exposed"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	CacheSource string    `json:"cache_source,omitempty"`
	Stale       bool      `json:"stale"`
}

func NewCatalog(overrides map[string]string) *Catalog {
	protocols := make(map[string]wire.Protocol, len(overrides))
	for model, protocol := range overrides {
		protocols[model] = wire.Protocol(protocol)
	}
	c := &Catalog{
		protocols: protocols,
	}
	c.state.Store(&catalogState{
		zen:             map[string]bool{},
		nativeProtocols: map[string]wire.Protocol{},
		unsupported:     map[string]bool{},
		modelMeta:       map[string]Metadata{},
		cacheSource:     "none",
	})
	return c
}

func (c *Catalog) SetPricingStore(store *PricingStore) {
	c.mu.Lock()
	c.pricing = store
	c.mu.Unlock()
}

func (c *Catalog) SetAvailabilityStore(store *AvailabilityStore) {
	c.mu.Lock()
	c.availability = store
	c.mu.Unlock()
}

func (c *Catalog) SetCachePath(path string) {
	c.mu.Lock()
	c.cachePath = path
	c.mu.Unlock()
}

func (c *Catalog) SetRefreshInterval(interval time.Duration) {
	c.mu.Lock()
	c.refreshAfter = interval
	c.mu.Unlock()
}

func (c *Catalog) ReplaceWithCapabilities(zen []string, native map[string]wire.Protocol, unsupported map[string]bool, metadata map[string]Metadata) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.state.Load()
	next := &catalogState{
		zen:             prev.zen,
		nativeProtocols: prev.nativeProtocols,
		unsupported:     prev.unsupported,
		modelMeta:       prev.modelMeta,
		sortedIDs:       prev.sortedIDs,
		updatedAt:       time.Now().UTC(),
		cacheSource:     "live",
		stale:           false,
	}
	if zen != nil {
		next.zen = toSet(zen)
		next.sortedIDs = sortedSetKeys(next.zen)
	}
	if native != nil {
		next.nativeProtocols = cloneProtocols(native)
	}
	if unsupported != nil {
		next.unsupported = cloneBools(unsupported)
	}
	if metadata != nil {
		next.modelMeta = cloneMetadataMap(metadata)
	}
	c.state.Store(next)
}

// Route resolves an anonymous free-tier route without acquiring locks on the
// catalog state (atomic pointer load).
func (c *Catalog) Route(model string) (Route, error) {
	if c.availability != nil && c.availability.Disabled(model) {
		return Route{}, fmt.Errorf("model %q is disabled after a failed availability check", model)
	}
	return c.route(model)
}

// RouteForProbe lets availability checks recheck disabled models while keeping
// the free-tier, upstream catalog, and protocol requirements of normal routes.
func (c *Catalog) RouteForProbe(model string) (Route, error) {
	return c.route(model)
}

func (c *Catalog) route(model string) (Route, error) {
	s := c.state.Load()
	decision := c.anonymousDecision(model)
	if !decision.Allowed {
		return Route{}, fmt.Errorf("model %q is not available on the anonymous free tier", model)
	}
	if c.protocols[model] == "" && s.unsupported[model] {
		return Route{}, fmt.Errorf("model %q uses an unsupported upstream protocol", model)
	}
	if len(s.zen) > 0 && !s.zen[model] {
		return Route{}, fmt.Errorf("model %q is not available in the upstream catalog", model)
	}
	return Route{ID: model, Protocol: c.protocolFor(s, model)}, nil
}

func (c *Catalog) protocolFor(s *catalogState, model string) wire.Protocol {
	if protocol := c.protocols[model]; protocol != "" {
		return protocol
	}
	if protocol := s.nativeProtocols[model]; protocol != "" {
		return protocol
	}
	return wire.Chat
}

func (c *Catalog) anonymousDecision(model string) AnonymousDecision {
	if c.pricing != nil {
		return c.pricing.Decide(model)
	}
	return AnonymousDecision{Allowed: isFreeModel(model), Source: "name_fallback_metadata_pending"}
}

func (c *Catalog) IsFreeModel(model string) bool {
	return c.anonymousDecision(model).Allowed
}

func isFreeModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "free")
}

func (c *Catalog) List() []string {
	s := c.state.Load()
	models := make([]string, 0, len(s.sortedIDs))
	for _, model := range s.sortedIDs {
		if c.supportedWith(s, model) {
			models = append(models, model)
		}
	}
	return models
}

func (c *Catalog) AvailableModels() ([]Route, CatalogSnapshot) {
	s := c.state.Load()
	routes := make([]Route, 0, len(s.sortedIDs))
	for _, model := range s.sortedIDs {
		if !c.supportedWith(s, model) {
			continue
		}
		if route, err := c.Route(model); err == nil {
			routes = append(routes, route)
		}
	}
	stale := s.stale
	if !s.updatedAt.IsZero() && c.refreshAfter > 0 {
		stale = stale || time.Since(s.updatedAt) > max(2*c.refreshAfter, time.Minute)
	}
	snapshot := CatalogSnapshot{
		Total:       len(s.sortedIDs),
		Exposed:     len(routes),
		UpdatedAt:   s.updatedAt,
		CacheSource: s.cacheSource,
		Stale:       stale,
	}
	return routes, snapshot
}

func (c *Catalog) Supported(model string) bool {
	return c.supportedWith(c.state.Load(), model)
}

func (c *Catalog) supportedWith(s *catalogState, model string) bool {
	if len(s.zen) == 0 {
		return true
	}
	if !s.zen[model] {
		return false
	}
	if c.protocols[model] != "" {
		return true
	}
	if s.unsupported[model] {
		return false
	}
	return s.nativeProtocols[model] != ""
}

func (c *Catalog) MetadataFor(model string) Metadata {
	s := c.state.Load()
	md := s.modelMeta[model]
	pricing := c.pricing
	if pricing == nil {
		return md
	}
	price, ok := pricing.Price(model)
	if !ok {
		return md
	}
	if md.ContextWindow == 0 {
		md.ContextWindow = price.ContextWindow
	}
	if md.MaxInput == 0 {
		md.MaxInput = price.MaxInput
	}
	if md.MaxOutput == 0 {
		md.MaxOutput = price.MaxOutput
	}
	if price.Input != nil || price.Output != nil {
		md.Cost = &Cost{Input: price.Input, Output: price.Output, CacheRead: price.CacheRead, CacheWrite: price.CacheWrite}
	}
	return md
}

func toSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

func cloneProtocols(source map[string]wire.Protocol) map[string]wire.Protocol {
	result := make(map[string]wire.Protocol, len(source))
	for model, protocol := range source {
		result[model] = protocol
	}
	return result
}

func cloneBools(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for model, value := range source {
		result[model] = value
	}
	return result
}

func cloneMetadataMap(source map[string]Metadata) map[string]Metadata {
	result := make(map[string]Metadata, len(source))
	for id, md := range source {
		result[id] = md
	}
	return result
}

func sortedSetKeys(source map[string]bool) []string {
	result := make([]string, 0, len(source))
	for model, available := range source {
		if available {
			result = append(result, model)
		}
	}
	sort.Strings(result)
	return result
}
