package httpx

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func bunTestTransport(t *testing.T) *BunFetchTransport {
	t.Helper()
	// Bun gives lowercase proxy variables precedence on some platforms. Clear
	// both spellings so a developer's environment cannot mask proxy behavior.
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	binary := os.Getenv("OPENCODE2API_BUN_BINARY")
	if binary == "" || binary == "off" {
		binary = "bun"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		t.Skip("Bun runtime not installed; run with OPENCODE2API_BUN_BINARY to exercise the real fetch transport")
	}
	return &BunFetchTransport{binary: resolved, headerTimeout: 3 * time.Second}
}

func TestBunFetchReverseProxyAndRequestBody(t *testing.T) {
	transport := bunTestTransport(t)
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, "http://127.0.0.1:1")
	}
	body := `{"model":"test","messages":[],"stream":true}`
	type receivedRequest struct{ path, body, agent string }
	received := make(chan receivedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		received <- receivedRequest{r.URL.RequestURI(), string(payload), r.Header.Get("User-Agent")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	path := "/private/proxy/https/opencode.ai/inference/openai/v1/chat/completions?beta=true"
	req, _ := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
	req.Header.Set("User-Agent", "opencode/latest/2.1.4/cli")
	req.Header.Set("Content-Type", "application/json")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("response = %q, error = %v", got, err)
	}
	observed := <-received
	if observed.path != path || observed.body != body || observed.agent != req.Header.Get("User-Agent") {
		t.Fatalf("path/body/agent changed: %+v", observed)
	}
}

func TestBunFetchStreamsBeforeCompletion(t *testing.T) {
	transport := bunTestTransport(t)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	first := make(chan string, 1)
	go func() {
		line, _ := reader.ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "data: first\n" {
			t.Fatalf("first frame = %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transport buffered the stream until completion")
	}
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("remaining stream = %q, error = %v", rest, err)
	}
}

func TestBunFetchCancellationReapsChild(t *testing.T) {
	transport := bunTestTransport(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body := response.Body.(*bunFetchBody)
	reader := bufio.NewReader(body)
	if first, err := reader.ReadString('\n'); err != nil || first != "first\n" {
		t.Fatalf("first chunk = %q, error = %v", first, err)
	}
	cancel()
	_, err = io.ReadAll(reader)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stream error = %v", err)
	}
	response.Body.Close()
	if body.cmd.ProcessState == nil {
		t.Fatal("canceled child process was not reaped")
	}
}

func TestBunFetchCloseReapsStreamingChild(t *testing.T) {
	transport := bunTestTransport(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body := response.Body.(*bunFetchBody)
	closed := make(chan struct{})
	go func() { _ = body.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("closing a stream did not stop Bun")
	}
	if body.cmd.ProcessState == nil {
		t.Fatal("closed child process was not reaped")
	}
}

func TestBunFetchHeaderTimeout(t *testing.T) {
	transport := bunTestTransport(t)
	transport.headerTimeout = 75 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	_, err := transport.RoundTrip(req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("header timeout error = %v", err)
	}
}

func TestBunFetchDecodesCompression(t *testing.T) {
	transport := bunTestTransport(t)
	want := strings.Repeat("decoded response ", 20)
	var compressed bytes.Buffer
	encoder := gzip.NewWriter(&compressed)
	_, _ = io.WriteString(encoder, want)
	_ = encoder.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", fmt.Sprint(compressed.Len()))
		_, _ = w.Write(compressed.Bytes())
	}))
	defer server.Close()
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil || string(got) != want {
		t.Fatalf("decoded body = %q, error = %v", got, err)
	}
	if response.Header.Get("Content-Encoding") != "" || response.Header.Get("Content-Length") != "" || !response.Uncompressed {
		t.Fatalf("decoded response retained stale headers: %v", response.Header)
	}
}

func TestBunFetchExplicitForwardProxy(t *testing.T) {
	transport := bunTestTransport(t)
	t.Setenv("NO_PROXY", "*")
	t.Setenv("no_proxy", "*")
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, "http://127.0.0.1:1")
	}
	want := "http://origin.invalid/prefix/inference/v1/models"
	received := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.RequestURI
		_, _ = io.WriteString(w, "proxied")
	}))
	defer proxy.Close()
	transport.proxy, _ = url.Parse(proxy.URL)
	req, _ := http.NewRequest(http.MethodGet, want, nil)
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, err = io.ReadAll(response.Body)
	observed := <-received
	if err != nil || observed != want {
		t.Fatalf("proxy received %q, want %q; error = %v", observed, want, err)
	}
}

func TestBunFetchCanceledBeforeStart(t *testing.T) {
	transport := bunTestTransport(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:1", nil)
	if _, err := transport.RoundTrip(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled request error = %v", err)
	}
}

func TestBunFetchTruncatedBodyFails(t *testing.T) {
	transport := bunTestTransport(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\nContent-Type: text/plain\r\n\r\nshort")
	}))
	defer server.Close()
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	response, err := transport.RoundTrip(req)
	if err != nil {
		// fetch may reject before publishing the response if the close was
		// observed with the headers. Both stages must report the failure.
		return
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated upstream body error = %v", err)
	}
}

func TestBunFetchConcurrentRequestsRemainIsolated(t *testing.T) {
	transport := bunTestTransport(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, r.Body)
	}))
	defer server.Close()
	var group sync.WaitGroup
	for i := range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			want := fmt.Sprintf("request-%d", i)
			req, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(want))
			response, err := transport.RoundTrip(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			got, err := io.ReadAll(response.Body)
			if err != nil || string(got) != want {
				t.Errorf("concurrent request %d body = %q, error = %v", i, got, err)
			}
		}()
	}
	group.Wait()
}

func TestBunFetchFailureDoesNotExposeURL(t *testing.T) {
	transport := bunTestTransport(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.Close()
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/private-secret?key=private-secret", nil)
	_, err := transport.RoundTrip(req)
	if err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("expected a sanitized network error, got %v", err)
	}
}

func TestFreeTierTransportSelection(t *testing.T) {
	t.Setenv("OPENCODE2API_BUN_BINARY", "off")
	transport, err := NewFreeTierTransport(BunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := transport.(*BunTransport); !ok {
		t.Fatalf("off selected %T", transport)
	}
	t.Setenv("OPENCODE2API_BUN_BINARY", "missing-opencode2api-bun-runtime-for-test")
	if _, err := NewFreeTierTransport(BunOptions{}); err == nil {
		t.Fatal("explicit missing Bun runtime silently fell back")
	}
	proxy, _ := url.Parse("socks5://127.0.0.1:1080")
	transport, err = NewFreeTierTransport(BunOptions{Proxy: proxy})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := transport.(*BunTransport); !ok {
		t.Fatalf("SOCKS route selected %T", transport)
	}
}
