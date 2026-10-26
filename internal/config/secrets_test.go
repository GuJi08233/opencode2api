package config

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type failedUpstreamTransport struct{}

func (failedUpstreamTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network unreachable")
}

func TestRedactorScrubsReverseProxyURLError(t *testing.T) {
	for _, test := range []struct {
		name, upstream, token string
	}{
		{
			name:     "Resin https target",
			upstream: "http://private-user:private-password@resin.invalid:2260/private-resin-token/private-platform/https/opencode.ai/inference?api_key=private-query",
			token:    "private-resin-token",
		},
		{
			name:     "encoded prefix and http target",
			upstream: "http://resin.invalid:2260/private%2Fencoded-token/private-platform/http/opencode.ai/inference?access_token=private-query",
			token:    "private%2Fencoded-token",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, err := url.Parse(test.upstream)
			if err != nil {
				t.Fatal(err)
			}
			requestURL := *base
			requestURL.Path += "/openai/v1/chat/completions"
			if requestURL.RawPath != "" {
				requestURL.RawPath += "/openai/v1/chat/completions"
			}
			req, err := http.NewRequest(http.MethodPost, requestURL.String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: failedUpstreamTransport{}}
			_, failure := client.Do(req)
			var requestError *url.Error
			if !errors.As(failure, &requestError) {
				t.Fatalf("expected real net/http URL error, got %T", failure)
			}
			if !strings.Contains(failure.Error(), test.token) || !strings.Contains(failure.Error(), "private-query") {
				t.Fatal("fixture did not reproduce the URL-error credential exposure")
			}
			original := req.URL.String()
			redactor := SecretRedactor()
			redactor.Replace(Config{Upstream: UpstreamConfig{Anonymous: test.upstream}})
			message := redactor.String(failure.Error())
			for _, secret := range []string{test.token, "private-platform", "private-user", "private-password", "private-query"} {
				if strings.Contains(message, secret) {
					t.Errorf("URL error leaked %q: %s", secret, message)
				}
			}
			if !strings.Contains(message, "resin.invalid:2260") || !strings.Contains(message, "opencode.ai/inference/") {
				t.Fatalf("scrubbing lost proxy/destination evidence: %s", message)
			}
			masked := "http://resin.invalid:2260/%5Bredacted%5D/https/opencode.ai/inference/openai/v1/chat/completions"
			if got := redactor.String(masked); got != masked {
				t.Fatalf("already sanitized routing evidence changed: %q", got)
			}
			if req.URL.String() != original {
				t.Fatal("registering log secrets mutated the actual request")
			}
		})
	}
}

func TestRedactorQueryFieldsDoNotMaskOrdinaryValues(t *testing.T) {
	redactor := SecretRedactor()
	redactor.Replace(Config{Upstream: UpstreamConfig{Anonymous: "https://example.invalid/inference?beta=true&token=abc"}})
	if got := redactor.String("beta=true token=abc, ordinary true and abc"); got != "*** ***, ordinary true and abc" {
		t.Fatalf("query fields were not isolated from ordinary values: %q", got)
	}
}
