package models

import (
	"testing"
	"time"

	wire "opencode2api/internal/protocol"
)

func TestProbeRouteRechecksDisabledModelsWithoutBypassingRoutingRequirements(t *testing.T) {
	availability, err := NewAvailabilityStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := availability.Record("recovering-free", 0, false, "model_unavailable", "anonymous", time.Now(), EffortProbe{}); err != nil {
		t.Fatal(err)
	}
	catalog := NewCatalog(map[string]string{"override-free": string(wire.Anthropic)})
	catalog.SetAvailabilityStore(availability)
	catalog.ReplaceWithCapabilities(
		[]string{"recovering-free", "paid-model", "unsupported-free", "override-free"},
		map[string]wire.Protocol{"recovering-free": wire.Responses, "paid-model": wire.Chat},
		map[string]bool{"unsupported-free": true, "override-free": true},
		nil,
	)
	if _, err := catalog.Route("recovering-free"); err == nil {
		t.Fatal("ordinary routing accepted a disabled model")
	}
	for _, tc := range []struct {
		model    string
		protocol wire.Protocol
		wantErr  bool
	}{
		{model: "recovering-free", protocol: wire.Responses},
		{model: "override-free", protocol: wire.Anthropic},
		{model: "paid-model", wantErr: true},
		{model: "missing-free", wantErr: true},
		{model: "unsupported-free", wantErr: true},
	} {
		t.Run(tc.model, func(t *testing.T) {
			route, err := catalog.RouteForProbe(tc.model)
			if (err != nil) != tc.wantErr {
				t.Fatalf("RouteForProbe error = %v, want error = %v", err, tc.wantErr)
			}
			if !tc.wantErr && (route.ID != tc.model || route.Protocol != tc.protocol) {
				t.Fatalf("RouteForProbe = %+v, want model %s with protocol %s", route, tc.model, tc.protocol)
			}
		})
	}
	if _, err := catalog.Route("recovering-free"); err == nil {
		t.Fatal("probing changed the disabled state before a successful recheck")
	}
}
