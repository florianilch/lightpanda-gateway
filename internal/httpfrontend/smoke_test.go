package httpfrontend_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/florianilch/lightpanda-gateway/internal/gateway"
	"github.com/florianilch/lightpanda-gateway/internal/httpfrontend"
)

const (
	testAPIKey  = "smoke-api-key"
	testTimeout = 5 * time.Second
)

func TestScriptWorkflow(t *testing.T) {
	frontend := newTestFrontend(t)
	script := `console.log("smoke-script");`

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+frontend.Addr().String()+"/scripts", bytes.NewBufferString(script))
	if err != nil {
		t.Fatalf("create script request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "text/plain")

	client := &http.Client{Timeout: testTimeout}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /scripts: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %s; body = %q", resp.Status, body)
	}

	var got struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode script response: %v", err)
	}
	wantStdout := "fake-lightpanda:" + script
	if got.Stdout != wantStdout {
		t.Fatalf("stdout = %q; want %q", got.Stdout, wantStdout)
	}
	if got.ExitCode != 0 {
		t.Fatalf("exit_code = %d; want 0", got.ExitCode)
	}
}

func TestCDPTunnel(t *testing.T) {
	frontend := newTestFrontend(t)

	conn, err := (&net.Dialer{Timeout: testTimeout}).DialContext(t.Context(), "tcp", frontend.Addr().String())
	if err != nil {
		t.Fatalf("dial frontend: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatalf("set CDP connection deadline: %v", err)
	}
	br := bufio.NewReader(conn)
	if _, err := fmt.Fprintf(conn,
		"GET /ws HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n",
		frontend.Addr().String(), testAPIKey); err != nil {
		t.Fatalf("write WebSocket upgrade: %v", err)
	}

	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	defer func() { _ = resp.Body.Close() }()
	if err != nil {
		t.Fatalf("read WebSocket upgrade: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %s; want 101 Switching Protocols", resp.Status)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("Sec-WebSocket-Accept = %q; want valid handshake", got)
	}

	if _, err := io.WriteString(conn, "smoke-cdp"); err != nil {
		t.Fatalf("write CDP tunnel: %v", err)
	}
	want := "fake-cdp!"
	got := make([]byte, len(want))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("read CDP tunnel: %v", err)
	}
	if string(got) != want {
		t.Fatalf("CDP tunnel response = %q; want %q", got, want)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close CDP connection: %v", err)
	}
}

func newTestFrontend(t *testing.T) *httpfrontend.Server {
	t.Helper()

	binary := buildFakeLightpanda(t)
	gw, err := gateway.New(&gateway.Config{
		MaxResources:      1,
		MaxQueueLength:    0,
		CDPStartupTimeout: testTimeout,
		BrowserBinary:     binary,
		MaxStdoutBytes:    1 << 20,
		MaxStderrBytes:    1 << 20,
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}

	logger := slog.New(slog.DiscardHandler)
	frontend, err := httpfrontend.New(&httpfrontend.Config{
		Addr:                "127.0.0.1:0",
		APIKey:              testAPIKey,
		MaxScriptBytes:      1 << 20,
		MaxClientOperations: 1,
	}, logger, gw, func() bool { return true })
	if err != nil {
		t.Fatalf("httpfrontend.New: %v", err)
	}
	if err := frontend.Listen(); err != nil {
		t.Fatalf("frontend.Listen: %v", err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- frontend.Serve() }()
	t.Cleanup(func() {
		if err := shutdownTestServices(frontend, gw); err != nil {
			t.Errorf("graceful shutdown: %v", err)

			gw.Stop()
			if closeErr := frontend.Close(); closeErr != nil {
				t.Errorf("frontend.Close: %v", closeErr)
			}
			if drainErr := shutdownTestServices(frontend, gw); drainErr != nil {
				t.Errorf("forced shutdown: %v", drainErr)
			}
		}
		select {
		case err := <-serveErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("frontend.Serve: %v", err)
			}
		case <-time.After(testTimeout):
			t.Error("frontend.Serve did not return")
		}
	})
	return frontend
}

func shutdownTestServices(frontend *httpfrontend.Server, gw *gateway.Gateway) error {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	var gatewayErr, frontendErr error
	var shutdowns sync.WaitGroup
	shutdowns.Go(func() { gatewayErr = gw.Shutdown(ctx) })
	shutdowns.Go(func() { frontendErr = frontend.Shutdown(ctx) })
	shutdowns.Wait()

	if gatewayErr != nil {
		gatewayErr = fmt.Errorf("Gateway.Shutdown: %w", gatewayErr)
	}
	if frontendErr != nil {
		frontendErr = fmt.Errorf("frontend.Shutdown: %w", frontendErr)
	}
	return errors.Join(gatewayErr, frontendErr)
}

func buildFakeLightpanda(t *testing.T) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "fakelightpanda")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", binary, "./testdata/fakelightpanda")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake Lightpanda: %v\n%s", err, output)
	}
	return binary
}
