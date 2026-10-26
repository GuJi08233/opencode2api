package gateway

import (
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"opencode2api/internal/config"
)

type redirectRoundTripFunc func(*http.Request) (*http.Response, error)

func (f redirectRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFreeTierClientsDoNotFollowReverseProxyRedirect(t *testing.T) {
	t.Setenv("OPENCODE2API_BUN_BINARY", "off")
	pool, err := transportPool([]string{"direct"}, config.PerformanceConfig{}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// A nil lease is sufficient here: the stub below handles every request,
	// so neither the WARP dialer nor a real network can be reached.
	warp, err := (&Gateway{}).newWarpLease(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		client *http.Client
		follow bool
	}{
		{"proxy free", pool.items[0].bun, false},
		{"WARP free", warp.client(), false},
		{"proxy paid", pool.items[0].client, true},
		{"WARP paid", warp.regular, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const reverse = "http://resin.invalid:2260/private/https/opencode.ai/inference/openai/v1/chat/completions"
			const origin = "https://opencode.ai/inference/openai/v1/chat/completions"
			var requested []string
			test.client.Transport = redirectRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				requested = append(requested, req.URL.String())
				if len(requested) == 1 {
					return &http.Response{
						StatusCode: http.StatusTemporaryRedirect,
						Header:     http.Header{"Location": {origin}},
						Body:       io.NopCloser(strings.NewReader("redirect")),
						Request:    req,
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("followed")),
					Request:    req,
				}, nil
			})
			req, _ := http.NewRequest(http.MethodPost, reverse, strings.NewReader(`{"model":"test-free"}`))
			req.Header.Set("Authorization", "Bearer public")
			response, err := test.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			wantURLs := []string{reverse}
			wantStatus, wantBody := http.StatusTemporaryRedirect, "redirect"
			if test.follow {
				wantURLs = append(wantURLs, origin)
				wantStatus, wantBody = http.StatusOK, "followed"
			}
			if !reflect.DeepEqual(requested, wantURLs) || response.StatusCode != wantStatus || string(body) != wantBody {
				t.Fatalf("requests=%q status=%d body=%q; want requests=%q status=%d body=%q", requested, response.StatusCode, body, wantURLs, wantStatus, wantBody)
			}
		})
	}
}
