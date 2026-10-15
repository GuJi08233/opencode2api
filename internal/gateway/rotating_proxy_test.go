package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

// This proxy counts real TCP tunnels, including TLS and HTTP/2 traffic.
func newSOCKSProxy(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	connections := new(atomic.Int32)
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer client.Close()
				var greeting [2]byte
				if _, err := io.ReadFull(client, greeting[:]); err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, client, int64(greeting[1])); err != nil {
					return
				}
				if _, err := client.Write([]byte{5, 0}); err != nil {
					return
				}
				var request [4]byte
				if _, err := io.ReadFull(client, request[:]); err != nil || request[1] != 1 {
					return
				}
				size := 0
				switch request[3] {
				case 1:
					size = net.IPv4len
				case 4:
					size = net.IPv6len
				case 3:
					var length [1]byte
					if _, err := io.ReadFull(client, length[:]); err != nil {
						return
					}
					size = int(length[0])
				default:
					return
				}
				address := make([]byte, size+2)
				if _, err := io.ReadFull(client, address); err != nil {
					return
				}
				host := string(address[:size])
				if request[3] != 3 {
					host = net.IP(address[:size]).String()
				}
				port := strconv.Itoa(int(binary.BigEndian.Uint16(address[size:])))
				upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 2*time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()
				if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
					return
				}
				done := make(chan struct{})
				go func() {
					io.Copy(upstream, client)
					upstream.Close()
					close(done)
				}()
				io.Copy(client, upstream)
				client.Close()
				<-done
			}()
		}
	}()
	return "socks5h://" + listener.Addr().String(), connections
}

func testPerformance(fresh bool) config.PerformanceConfig {
	return config.PerformanceConfig{
		DisableProxyKeepAlives: fresh, MaxIdleConns: 8, MaxIdleConnsPerHost: 4,
		IdleConnTimeoutSeconds: 10, ConnectTimeoutSeconds: 2, FailureCooldownSeconds: 15,
	}
}

func TestProxyConnectionReuse(t *testing.T) {
	for _, tc := range []struct {
		name, protocol string
		fresh, direct  bool
		connections    int32
	}{
		{name: "default SOCKS tunnel reused", protocol: "HTTP/2.0", connections: 1},
		{name: "fresh SOCKS tunnel per request", protocol: "HTTP/1.1", fresh: true, connections: 2},
		{name: "direct HTTP2 remains reusable", protocol: "HTTP/2.0", fresh: true, direct: true, connections: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connections := new(atomic.Int32)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Test-Protocol", r.Proto)
				io.WriteString(w, "ok")
			}))
			upstream.EnableHTTP2 = true
			upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			upstream.StartTLS()
			t.Cleanup(upstream.Close)
			proxyURL, tunnels := newSOCKSProxy(t)
			if tc.direct {
				proxyURL = "direct"
			}
			pool, err := newTransportPool([]string{proxyURL}, testPerformance(tc.fresh), 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			client := pool.items[0].client
			transport := client.Transport.(*http.Transport)
			t.Cleanup(transport.CloseIdleConnections)
			roots := x509.NewCertPool()
			roots.AddCert(upstream.Certificate())
			transport.TLSClientConfig = &tls.Config{RootCAs: roots}
			for range 2 {
				resp, err := client.Get(upstream.URL)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if got := resp.Header.Get("X-Test-Protocol"); got != tc.protocol {
					t.Fatalf("protocol = %s, want %s", got, tc.protocol)
				}
			}
			if got := connections.Load(); got != tc.connections {
				t.Fatalf("upstream TCP connections = %d, want %d", got, tc.connections)
			}
			if !tc.direct && tunnels.Load() != tc.connections {
				t.Fatalf("SOCKS tunnels = %d, want %d", tunnels.Load(), tc.connections)
			}
		})
	}
}

func TestAnonymousRotatingProxyRetries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statuses   []int
		budget     int
		attempts   int
		wantStatus int
	}{
		{name: "quota then server failure then success", statuses: []int{429, 503, 200}, budget: 3, attempts: 3, wantStatus: 200},
		{name: "dropped tunnel then success", statuses: []int{0, 200}, budget: 3, attempts: 2, wantStatus: 200},
		{name: "finite retry budget", statuses: []int{429}, budget: 3, attempts: 3, wantStatus: 429},
		{name: "invalid request not replayed", statuses: []int{400, 200}, budget: 3, attempts: 1, wantStatus: 400},
		{name: "legacy single proxy attempted once", statuses: []int{429, 200}, budget: 0, attempts: 1, wantStatus: 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, connections := new(atomic.Int32), new(atomic.Int32)
			proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				i := int(calls.Add(1)) - 1
				status := tc.statuses[min(i, len(tc.statuses)-1)]
				if status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						conn.Close()
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(status)
				io.WriteString(w, `{"ok":true}`)
			}))
			proxy.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			proxy.Start()
			t.Cleanup(proxy.Close)
			fresh := tc.budget > 0
			pool, err := newTransportPool([]string{proxy.URL}, testPerformance(fresh), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.items[0].client.CloseIdleConnections)
			g := &Gateway{
				cfg:    config.Config{Retry: config.RetryConfig{AnonymousMaxAttempts: tc.budget}, Upstream: config.UpstreamConfig{Anonymous: "http://upstream.invalid"}},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)), transports: pool,
				anonymous: newAnonymousPool(true, pool, 15*time.Second, fresh),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if !fresh {
				// Avoid real connectivity probes while checking the legacy budget.
				ctx = WithDiagnosticRequest(ctx)
			}
			route := models.Route{ID: "test-free", Tier: config.TierZen, Protocol: wire.Chat, Anonymous: true}
			body := []byte(`{"model":"test-free","messages":[{"role":"user","content":"hello"}]}`)
			resp, err, attempts := g.doAnonymousUpstream(ctx, route, map[config.Tier][]byte{config.TierZen: body}, identity.RequestIDs{}, 0)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if attempts != tc.attempts || int(calls.Load()) != tc.attempts || resp.StatusCode != tc.wantStatus {
				t.Fatalf("attempts=%d calls=%d status=%d; want %d attempts, status %d", attempts, calls.Load(), resp.StatusCode, tc.attempts, tc.wantStatus)
			}
			if fresh && int(connections.Load()) != tc.attempts {
				t.Fatalf("connections=%d, want one per attempt (%d)", connections.Load(), tc.attempts)
			}
			if fresh && (!pool.items[0].healthy.Load() || g.anonymous.nodes[0].cooldownUntil.Load() != 0) {
				t.Fatal("one exit failure cooled down the entire rotating gateway")
			}
		})
	}
}

func TestAnonymousFixedExitStillCoolsDown(t *testing.T) {
	proxy := &proxyTransport{}
	proxy.healthy.Store(true)
	pool := newAnonymousPool(true, &transportPool{items: []*proxyTransport{proxy}}, time.Second, true)
	node := pool.nodes[0]
	pool.MarkFailure(node, &http.Response{StatusCode: 429, Header: make(http.Header)}, nil)
	cursor := pool.CursorFor("")
	if cursor.Next() != nil {
		t.Fatal("fixed exit in cooldown was selected")
	}
}
