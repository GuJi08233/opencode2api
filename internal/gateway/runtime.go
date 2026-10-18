package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	"opencode2api/internal/config"
	modelcatalog "opencode2api/internal/models"
	"opencode2api/internal/telemetry"
)

type gatewayRuntime struct {
	availability *modelcatalog.AvailabilityStore
	config       config.Config
	gateway      *Gateway
	handler      http.Handler
	cancel       context.CancelFunc
}

type RuntimeManager struct {
	configPath         string
	root               context.Context
	logger             *slog.Logger
	monitor            *telemetry.Monitor
	redactor           *config.SecretRedactor
	level              *slog.LevelVar
	current            atomic.Pointer[gatewayRuntime]
	updateMu           sync.Mutex
	metadata           *modelcatalog.PricingStore
	availabilityCancel context.CancelFunc
}

func NewRuntimeManager(root context.Context, configPath string, cfg config.Config, logger *slog.Logger, monitor *telemetry.Monitor, redactor *config.SecretRedactor, level *slog.LevelVar) (*RuntimeManager, error) {
	manager := &RuntimeManager{
		configPath: configPath, root: root, logger: logger, monitor: monitor, redactor: redactor, level: level,
	}
	manager.metadata = modelcatalog.NewPricingStore(configPath, logger)
	manager.metadata.SetClientProvider(func() []*http.Client {
		runtime := manager.current.Load()
		if runtime == nil || runtime.gateway == nil || runtime.gateway.transports == nil {
			return nil
		}
		clients := make([]*http.Client, 0, len(runtime.gateway.transports.items))
		for _, proxy := range runtime.gateway.transports.items {
			if proxy != nil && proxy.healthy.Load() {
				clients = append(clients, proxy.client)
			}
		}
		return clients
	})
	runtime, err := manager.build(cfg)
	if err != nil {
		return nil, err
	}
	if err := runtime.gateway.catalog.LoadCache(modelcatalog.CatalogCachePath(configPath)); err != nil && !os.IsNotExist(err) {
		if manager.logger != nil {
			manager.logger.Warn("model catalog cache ignored", "component", "models", "event", "catalog_cache_load_failed", "path", modelcatalog.CatalogCachePath(configPath), "error", err)
		}
	}
	manager.current.Store(runtime)
	manager.redactor.Replace(cfg)
	telemetry.SetLogLevel(manager.level, cfg.Logging.Level)
	manager.start(runtime)
	manager.metadata.Start(root)
	manager.startAvailabilityChecks()
	return manager, nil
}

func (m *RuntimeManager) build(cfg config.Config) (*gatewayRuntime, error) {
	gateway, err := New(cfg, m.logger, m.monitor, m.redactor)
	if err != nil {
		return nil, err
	}
	availabilityPath := ""
	if m.configPath != "" {
		availabilityPath = m.configPath + "." + config.Fingerprint(cfg.Upstream.Zen) + ".availability.json"
	}
	availability, err := modelcatalog.NewAvailabilityStore(availabilityPath)
	if err != nil {
		return nil, fmt.Errorf("load model availability: %w", err)
	}
	if current := m.current.Load(); current != nil && current.config.Upstream.Zen == cfg.Upstream.Zen {
		availability = current.availability
	}
	gateway.catalog.SetPricingStore(m.metadata)
	gateway.catalog.SetAvailabilityStore(availability)
	gateway.availability = availability
	gateway.catalog.SetCachePath(modelcatalog.CatalogCachePath(m.configPath))
	return &gatewayRuntime{config: cfg, gateway: gateway, availability: availability, handler: gateway.Handler(), cancel: func() {}}, nil
}

func (m *RuntimeManager) start(runtime *gatewayRuntime) {
	runtimeCtx, cancel := context.WithCancel(m.root)
	runtime.cancel = cancel
	runtime.gateway.StartProxyHealthChecks(runtimeCtx)
	runtime.gateway.StartModelRefresh(runtimeCtx)
}

func (m *RuntimeManager) Handler() http.Handler {
	dynamic := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtime := m.current.Load()
		if runtime == nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		runtime.handler.ServeHTTP(w, r)
	})
	return telemetry.Middleware(m.monitor, m.logger, dynamic)
}

func (m *RuntimeManager) Shutdown() {
	if m.availabilityCancel != nil {
		m.availabilityCancel()
	}
	if current := m.current.Load(); current != nil {
		current.cancel()
	}
}
