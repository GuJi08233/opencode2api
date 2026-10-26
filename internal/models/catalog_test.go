package models

import (
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

func TestAvailableModelsKeepsNewlyListedFreeModels(t *testing.T) {
	catalog := Catalog(config.TierZen, nil)
	catalog.ReplaceWithCapabilities(
		[]string{"brand-new-free", "mixed-case-FREE", "blocked-free", "known-free", "decision-free", "known-paid", "unknown-paid"},
		nil,
		map[config.Tier]map[string]wire.Protocol{
			config.TierZen: {"known-free": wire.Anthropic, "decision-free": wire.SystemOne, "known-paid": wire.Responses},
		},
		map[config.Tier]map[string]bool{config.TierZen: {"blocked-free": true}},
		nil,
	)
	routes, _ := catalog.AvailableModels(true, false, true)
	got := make(map[string]Route)
	for _, route := range routes {
		got[route.ID] = route
	}
	want := map[string]wire.Protocol{
		"brand-new-free":  wire.Chat,
		"mixed-case-FREE": wire.Chat,
		"known-free":      wire.Anthropic,
		"decision-free":   wire.SystemOne,
		"known-paid":      wire.Responses,
	}
	if len(got) != len(want) {
		t.Fatalf("available model count = %d, want %d; routes = %+v", len(got), len(want), routes)
	}
	for model, protocol := range want {
		if route, ok := got[model]; !ok || route.Protocol != protocol {
			t.Errorf("%s route = %+v, present = %v; want protocol %s", model, route, ok, protocol)
		}
		if !catalog.Supported(model) {
			t.Errorf("listed routable model %s was not supported by the catalog", model)
		}
	}
	if _, ok := got["blocked-free"]; ok {
		t.Error("explicitly unsupported free model was exposed")
	}
	if _, ok := got["unknown-paid"]; ok {
		t.Error("missing metadata fallback was broadened to a paid model")
	}
	if got["known-paid"].Anonymous {
		t.Error("known paid model acquired an anonymous route")
	}
	if _, err := catalog.Route("unlisted-free", true, false, true); err == nil {
		t.Error("a free suffix admitted a model absent from the live list")
	}
}

func TestFreeModelFallbackKeepsExplicitProtocolOverrides(t *testing.T) {
	catalog := Catalog(config.TierZen, map[string]string{"override-free": string(wire.Responses)})
	catalog.ReplaceWithCapabilities(
		[]string{"override-free"}, nil, nil,
		map[config.Tier]map[string]bool{config.TierZen: {"override-free": true}}, nil,
	)
	routes, _ := catalog.AvailableModels(false, false, true)
	if len(routes) != 1 || routes[0].Protocol != wire.Responses {
		t.Fatalf("explicit protocol override was lost: %+v", routes)
	}
}
