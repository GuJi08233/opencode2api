package gateway

import (
	"testing"

	"opencode2api/internal/config"
	"opencode2api/internal/models"
)

// TestAnonymousUpstreamsOrder pins the anonymous lane's upstream order: the
// configured v1 upstream.zen comes first so free-tier traffic can be routed
// through a reverse proxy, with the v2 /inference origin kept as a fallback.
func TestAnonymousUpstreamsOrder(t *testing.T) {
	const proxyURL = "http://resin:2260/yishiguji/opencode/https/opencode.ai/zen"
	cases := []struct {
		name      string
		zen       string
		anonymous string
		want      []string
	}{
		{
			name:      "default config keeps the built-in origins",
			zen:       models.V1Base,
			anonymous: "https://opencode.ai/inference",
			want:      []string{models.V1Base, "https://opencode.ai/inference"},
		},
		{
			name:      "reverse proxy in upstream.zen is tried first",
			zen:       proxyURL,
			anonymous: "https://opencode.ai/inference",
			want:      []string{proxyURL, "https://opencode.ai/inference"},
		},
		{
			name:      "empty anonymous yields only the v1 origin",
			zen:       models.V1Base,
			anonymous: "",
			want:      []string{models.V1Base},
		},
		{
			name:      "trailing slash is trimmed",
			zen:       proxyURL + "/",
			anonymous: "",
			want:      []string{proxyURL},
		},
		{
			name:      "anonymous equal to zen collapses to one origin",
			zen:       proxyURL,
			anonymous: proxyURL,
			want:      []string{proxyURL},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &Gateway{cfg: config.Config{Upstream: config.UpstreamConfig{Zen: tc.zen, Anonymous: tc.anonymous}}}
			got := g.anonymousUpstreams()
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("upstream %d: got %q, want %q", i, got[i], tc.want[i])
				}
			}
			// The v1 lane is detected by URL suffix, so a proxy path ending in
			// /zen must still be shaped with the v1 header set and Bun client.
			if !models.IsV1Upstream(got[0]) {
				t.Fatalf("first upstream %q should be treated as the v1 lane", got[0])
			}
		})
	}
}
