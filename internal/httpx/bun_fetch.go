package httpx

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

//go:embed bun_fetch.mjs
var bunFetchScript string

// NewFreeTierTransport prefers a real Bun runtime when one is installed.
// Docker pins OPENCODE2API_BUN_BINARY explicitly, so a missing runtime fails
// startup instead of silently changing the request's network fingerprint.
// "off" selects the Go-compatible transport. WARP's custom dial and SOCKS
// routes retain that transport because fetch cannot use their Go dialer.
func NewFreeTierTransport(options BunOptions) (http.RoundTripper, error) {
	configured := strings.TrimSpace(os.Getenv("OPENCODE2API_BUN_BINARY"))
	if configured == "off" || options.DialContext != nil || (options.Proxy != nil && options.Proxy.Scheme != "http" && options.Proxy.Scheme != "https") {
		return NewBunTransport(options)
	}
	binary := configured
	if binary == "" {
		binary = "bun"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		if configured != "" {
			return nil, errors.New("configured Bun runtime is unavailable")
		}
		return NewBunTransport(options)
	}
	return &BunFetchTransport{binary: resolved, proxy: options.Proxy, headerTimeout: options.ResponseHeaderTimeout}, nil
}

// BunFetchTransport sends free-tier requests with Bun's own fetch, including
// its TLS and HTTP behavior. Each response owns one child process; closing or
// canceling the response terminates that process and releases its sockets.
// No auxiliary port or listener is exposed, and calls may run concurrently.
type BunFetchTransport struct {
	binary        string
	proxy         *url.URL
	headerTimeout time.Duration
}

type bunFetchInput struct {
	URL             string      `json:"url"`
	Method          string      `json:"method"`
	Headers         http.Header `json:"headers"`
	Body            []byte      `json:"body"`
	Proxy           string      `json:"proxy,omitempty"`
	HeaderTimeoutMS int64       `json:"headerTimeoutMs"`
}

type bunFetchOutput struct {
	Status     int         `json:"status"`
	StatusText string      `json:"statusText"`
	Headers    [][2]string `json:"headers"`
	Decoded    bool        `json:"decoded"`
	Error      string      `json:"error"`
}

func (t *BunFetchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if cause := context.Cause(req.Context()); cause != nil {
		return nil, cause
	}
	if req.URL == nil || req.URL.Host == "" || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return nil, errors.New("Bun fetch requires an HTTP(S) URL")
	}
	body, err := readRequestBody(req)
	if err != nil {
		return nil, err
	}
	input := bunFetchInput{URL: req.URL.String(), Method: req.Method, Headers: req.Header, Body: body, HeaderTimeoutMS: t.headerTimeout.Milliseconds()}
	if t.proxy != nil {
		input.Proxy = t.proxy.String()
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(req.Context())
	cmd := exec.CommandContext(ctx, t.binary, "--eval", bunFetchScript)
	cmd.Stdin = bytes.NewReader(encoded)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		stdout.Close()
		if cause := context.Cause(req.Context()); cause != nil {
			return nil, cause
		}
		return nil, errors.New("could not start Bun fetch runtime")
	}
	reader := bufio.NewReaderSize(stdout, 64<<10)
	line, err := reader.ReadSlice('\n')
	var output bunFetchOutput
	if err == nil {
		err = json.Unmarshal(line, &output)
	}
	if err != nil || output.Error != "" || output.Status < 100 || output.Status > 599 {
		cancel()
		stdout.Close()
		_ = cmd.Wait()
		if req.Context().Err() != nil {
			return nil, context.Cause(req.Context())
		}
		if output.Error == "timeout" {
			return nil, fmt.Errorf("Bun fetch response headers: %w", context.DeadlineExceeded)
		}
		return nil, &net.OpError{Op: "fetch", Net: "tcp", Err: errors.New("Bun upstream request failed")}
	}
	headers := make(http.Header, len(output.Headers))
	for _, pair := range output.Headers {
		headers.Add(pair[0], pair[1])
	}
	return &http.Response{
		StatusCode: output.Status, Status: fmt.Sprintf("%d %s", output.Status, output.StatusText),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: headers, ContentLength: -1, Uncompressed: output.Decoded,
		Request: req, Body: &bunFetchBody{reader: reader, stdout: stdout, cmd: cmd, cancel: cancel, ctx: req.Context()},
	}, nil
}

// Each fetch process has no connections shared with another request.
func (t *BunFetchTransport) CloseIdleConnections() {}

type bunFetchBody struct {
	reader  *bufio.Reader
	stdout  io.ReadCloser
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	ctx     context.Context
	once    sync.Once
	waitErr error
}

func (b *bunFetchBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if err != nil {
		b.finish(err != io.EOF)
		if cause := context.Cause(b.ctx); cause != nil {
			return n, cause
		}
		if err == io.EOF && b.waitErr != nil {
			return n, io.ErrUnexpectedEOF
		}
	}
	return n, err
}

func (b *bunFetchBody) Close() error {
	b.finish(true)
	return nil
}

func (b *bunFetchBody) finish(terminate bool) {
	b.once.Do(func() {
		if terminate {
			b.cancel()
			b.stdout.Close()
		}
		b.waitErr = b.cmd.Wait()
		b.stdout.Close()
		b.cancel()
	})
}
