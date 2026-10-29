package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"passport-backend/internal/httpapi"
	"passport-backend/internal/passport"
)

const testPipelineVersion = "server-test-v1"

func validServerConfig() Config {
	return Config{
		Address:           "127.0.0.1:0",
		HTTP:              httpapi.Config{MaxUploadBytes: 1024, OCRTimeout: 500 * time.Millisecond, MaxConcurrent: 1, PipelineVersion: testPipelineVersion, AllowedClientNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}},
		ReadHeaderTimeout: 200 * time.Millisecond,
		ReadTimeout:       400 * time.Millisecond,
		WriteTimeout:      2 * time.Second,
		IdleTimeout:       time.Second,
		ShutdownTimeout:   300 * time.Millisecond,
		MaxHeaderBytes:    1024,
	}
}

type testRecognizer struct {
	calls     atomic.Int32
	recognize func(context.Context, httpapi.Image) (passport.Extraction, error)
}

func (r *testRecognizer) Recognize(ctx context.Context, image httpapi.Image) (passport.Extraction, error) {
	r.calls.Add(1)
	if r.recognize != nil {
		return r.recognize(ctx, image)
	}
	return passport.Extraction{Fields: map[string]passport.FieldResult{}}, nil
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	base := validServerConfig()
	tests := []struct {
		name   string
		mutate func(*Config)
		nilRec bool
	}{
		{"blank address", func(c *Config) { c.Address = " \t" }, false},
		{"zero upload limit", func(c *Config) { c.HTTP.MaxUploadBytes = 0 }, false},
		{"negative upload limit", func(c *Config) { c.HTTP.MaxUploadBytes = -1 }, false},
		{"negative OCR timeout", func(c *Config) { c.HTTP.OCRTimeout = -time.Nanosecond }, false},
		{"negative concurrency", func(c *Config) { c.HTTP.MaxConcurrent = -1 }, false},
		{"zero OCR timeout", func(c *Config) { c.HTTP.OCRTimeout = 0 }, false},
		{"zero concurrency", func(c *Config) { c.HTTP.MaxConcurrent = 0 }, false},
		{"blank pipeline version", func(c *Config) { c.HTTP.PipelineVersion = " \t" }, false},
		{"zero read header timeout", func(c *Config) { c.ReadHeaderTimeout = 0 }, false},
		{"negative read header timeout", func(c *Config) { c.ReadHeaderTimeout = -time.Nanosecond }, false},
		{"zero read timeout", func(c *Config) { c.ReadTimeout = 0 }, false},
		{"negative read timeout", func(c *Config) { c.ReadTimeout = -time.Nanosecond }, false},
		{"zero write timeout", func(c *Config) { c.WriteTimeout = 0 }, false},
		{"negative write timeout", func(c *Config) { c.WriteTimeout = -time.Nanosecond }, false},
		{"zero idle timeout", func(c *Config) { c.IdleTimeout = 0 }, false},
		{"negative idle timeout", func(c *Config) { c.IdleTimeout = -time.Nanosecond }, false},
		{"zero shutdown timeout", func(c *Config) { c.ShutdownTimeout = 0 }, false},
		{"negative shutdown timeout", func(c *Config) { c.ShutdownTimeout = -time.Nanosecond }, false},
		{"zero max header bytes", func(c *Config) { c.MaxHeaderBytes = 0 }, false},
		{"negative max header bytes", func(c *Config) { c.MaxHeaderBytes = -1 }, false},
		{"header timeout exceeds read timeout", func(c *Config) { c.ReadHeaderTimeout = c.ReadTimeout + time.Nanosecond }, false},
		{"write timeout leaves no OCR response window", func(c *Config) { c.WriteTimeout = c.ReadTimeout + c.HTTP.OCRTimeout }, false},
		{"duration sum overflow is rejected", func(c *Config) {
			c.ReadTimeout = time.Duration(1<<63 - 1)
			c.HTTP.OCRTimeout = time.Nanosecond
			c.ReadHeaderTimeout = time.Duration(1<<63 - 2)
			c.WriteTimeout = time.Duration(1<<63 - 1)
		}, false},
		{"nil recognizer", func(*Config) {}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.mutate(&cfg)
			var recognizer httpapi.Recognizer = &testRecognizer{}
			if tt.nilRec {
				recognizer = nil
			}
			if _, err := New(cfg, recognizer); err == nil {
				t.Fatal("New() error = nil, want invalid configuration error")
			}
		})
	}
}

func TestNewRejectsMaxHeaderBytesOverflow(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("net/http read-limit overflow applies to 64-bit int")
	}
	cfg := validServerConfig()
	cfg.MaxHeaderBytes = int(^uint(0) >> 1)
	if _, err := New(cfg, &testRecognizer{}); err == nil {
		t.Fatal("New() error = nil, want MaxHeaderBytes overflow rejection")
	}
}

func TestNewLANRequiresAndValidatesClientNetworks(t *testing.T) {
	tests := []struct {
		name     string
		networks []netip.Prefix
	}{
		{name: "empty allowlist"},
		{name: "invalid prefix", networks: []netip.Prefix{{}}},
		{name: "public address", networks: []netip.Prefix{netip.MustParsePrefix("8.8.8.8/32")}},
		{name: "unspecified address", networks: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/32")}},
		{name: "prefix spills beyond private range", networks: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/7")}},
		{name: "IPv4-mapped prefix", networks: []netip.Prefix{netip.MustParsePrefix("::ffff:192.168.1.0/120")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validServerConfig()
			config.HTTP.AllowedClientNetworks = test.networks
			if _, err := New(config, &testRecognizer{}); err == nil {
				t.Fatal("New() error = nil, want invalid LAN configuration error")
			}
		})
	}
}

func TestNewLANAcceptsPrivateClientNetworks(t *testing.T) {
	validNetworks := []string{
		"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"fc00::/7", "169.254.0.0/16", "fe80::/10",
	}
	for _, network := range validNetworks {
		t.Run(network, func(t *testing.T) {
			config := validServerConfig()
			config.HTTP.AllowedClientNetworks = []netip.Prefix{netip.MustParsePrefix(network)}
			if _, err := New(config, &testRecognizer{}); err != nil {
				t.Fatalf("New() with private network %s error = %v", network, err)
			}
		})
	}
}

func TestNewLANRequiresExplicitSafeNumericBindAddress(t *testing.T) {
	t.Run("rejects unsafe and nonnumeric addresses", func(t *testing.T) {
		for _, address := range []string{
			"localhost:0", ":0", "0.0.0.0:0", "[::]:0", "8.8.8.8:0",
			"[::ffff:127.0.0.1]:0", "127.0.0.1:http", "127.0.0.1:65536",
		} {
			t.Run(address, func(t *testing.T) {
				config := validServerConfig()
				config.Address = address
				if _, err := New(config, &testRecognizer{}); err == nil {
					t.Fatalf("New() for bind address %q error = nil, want rejection", address)
				}
			})
		}
	})

	t.Run("accepts explicit numeric private addresses", func(t *testing.T) {
		for _, address := range []string{
			"127.0.0.1:0", "10.20.30.40:8080", "172.20.1.1:0", "192.168.1.20:0",
			"[::1]:0", "[fc00::12]:0", "169.254.2.3:0", "[fe80::12%en0]:0",
		} {
			t.Run(address, func(t *testing.T) {
				config := validServerConfig()
				config.Address = address
				if _, err := New(config, &testRecognizer{}); err != nil {
					t.Fatalf("New() for private bind address %q error = %v", address, err)
				}
			})
		}
	})
}

func TestServeLANRejectsDisallowedActualListenerBeforeAcceptAndCloses(t *testing.T) {
	for _, address := range []string{"8.8.8.8:1234", "0.0.0.0:1234"} {
		t.Run(address, func(t *testing.T) {
			listener := &fixedAddressListener{addr: &net.TCPAddr{IP: net.ParseIP(strings.Split(address, ":")[0]), Port: 1234}}
			server, err := New(validServerConfig(), &testRecognizer{})
			if err != nil {
				t.Fatal(err)
			}
			if err := server.Serve(context.Background(), listener); err == nil {
				t.Fatal("Serve() error = nil, want disallowed actual listener error")
			}
			if listener.acceptCalls.Load() != 0 {
				t.Fatalf("listener Accept calls = %d, want 0 before rejecting actual listener", listener.acceptCalls.Load())
			}
			if !listener.closed.Load() {
				t.Fatal("Serve() did not close the disallowed actual listener")
			}
		})
	}
}

func TestServeRunsExistingHTTPRouteAndReturnsMappedJSON(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	recognizer := &testRecognizer{}
	server, err := New(validServerConfig(), recognizer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	run := serveServer(t, server, ctx, cancel, listener)

	response, err := postSyntheticUpload(ctx, "http://"+run.listener.Addr().String()+"/v1/passport/recognize")
	if err != nil {
		t.Fatalf("POST existing route: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 200: %s", response.StatusCode, body)
	}
	var payload struct {
		RequestID string `json:"request_id"`
		Status    string `json:"status"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Status != "completed" || payload.RequestID == "" || response.Header.Get("X-Request-ID") != payload.RequestID {
		t.Fatalf("unexpected mapped response: payload=%+v header request ID=%q", payload, response.Header.Get("X-Request-ID"))
	}
	if recognizer.calls.Load() != 1 {
		t.Fatalf("recognizer calls = %d, want 1", recognizer.calls.Load())
	}
	if err := run.stop(); err != nil {
		t.Fatalf("Serve() after cancellation = %v, want nil", err)
	}
}

func TestRecognizerPanicDoesNotLeakPrivateDataToLogsOrResponse(t *testing.T) {
	const sentinel = "PRIVATE-PASSPORT-SENTINEL"
	var capturedLogs lockedBuffer
	originalLogOutput := log.Writer()
	log.SetOutput(&capturedLogs)
	t.Cleanup(func() { log.SetOutput(originalLogOutput) })

	recognizer := &testRecognizer{recognize: func(context.Context, httpapi.Image) (passport.Extraction, error) {
		panic(sentinel)
	}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server, err := New(validServerConfig(), recognizer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	run := serveServer(t, server, ctx, cancel, listener)

	response, requestErr := postSyntheticUpload(ctx, "http://"+listener.Addr().String()+"/v1/passport/recognize")
	var responseBody []byte
	if response != nil {
		responseBody, err = io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatalf("read response after recognizer panic: %v", err)
		}
		if response.StatusCode == http.StatusInternalServerError {
			t.Fatal("recognizer panic unexpectedly produced a 500 response")
		}
	}
	if stopErr := run.stop(); stopErr != nil {
		t.Fatalf("Serve() after recognizer panic = %v, want nil", stopErr)
	}
	if requestErr == nil || response != nil {
		t.Fatalf("request after unhandled recognizer panic = response %v, error %v; want connection error/EOF", response, requestErr)
	}
	if strings.Contains(requestErr.Error(), sentinel) || strings.Contains(string(responseBody), sentinel) {
		t.Fatal("recognizer panic sentinel leaked through the HTTP response or client error")
	}
	if strings.Contains(capturedLogs.String(), sentinel) {
		t.Fatal("recognizer panic sentinel leaked to the default HTTP server logger")
	}
}

func TestRunPropagatesAddressBindError(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	cfg := validServerConfig()
	cfg.Address = occupied.Addr().String()
	server, err := New(cfg, &testRecognizer{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Run(ctx); err == nil {
		t.Fatal("Run() error = nil, want address bind error")
	}
}

func TestPreCancelledRunSkipsBindAndRecognizer(t *testing.T) {
	recognizer := &testRecognizer{}
	server, err := New(validServerConfig(), recognizer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := server.Run(ctx); err != nil {
		t.Fatalf("Run() with pre-cancelled context = %v, want nil", err)
	}
	if recognizer.calls.Load() != 0 {
		t.Fatalf("recognizer calls = %d, want 0", recognizer.calls.Load())
	}
}

func TestServePreCancelledContextClosesListener(t *testing.T) {
	listener := newObservedListener(t)
	t.Cleanup(func() { _ = listener.Close() })
	server, err := New(validServerConfig(), &testRecognizer{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	defer cancel()
	if err := server.Serve(ctx, listener); err != nil {
		t.Fatalf("Serve() with pre-cancelled context = %v, want nil", err)
	}
	if !listener.closed.Load() {
		t.Fatal("Serve() did not close caller listener for pre-cancelled context")
	}
}

func TestServeCancellationDuringAcceptStopsCleanly(t *testing.T) {
	baseListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = baseListener.Close() })
	listener := &acceptGateListener{
		Listener: baseListener,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	server, err := New(validServerConfig(), &testRecognizer{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := serveServer(t, server, ctx, cancel, listener)
	waitSignal(t, listener.entered, "listener Accept to begin")
	if err := run.stop(); err != nil {
		t.Fatalf("Serve() after cancellation during Accept = %v, want nil", err)
	}
	if !listener.closed.Load() {
		t.Fatal("Serve() did not close listener while unblocking Accept")
	}
}

func TestNilServerInputsReturnErrorsWithoutPanicking(t *testing.T) {
	t.Run("Run rejects nil context", func(t *testing.T) {
		server, err := New(validServerConfig(), &testRecognizer{})
		if err != nil {
			t.Fatal(err)
		}
		if err := server.Run(nil); err == nil {
			t.Fatal("Run(nil) error = nil, want validation error")
		}
	})

	t.Run("Serve rejects nil listener", func(t *testing.T) {
		server, err := New(validServerConfig(), &testRecognizer{})
		if err != nil {
			t.Fatal(err)
		}
		if err := server.Serve(context.Background(), nil); err == nil {
			t.Fatal("Serve(context.Background(), nil) error = nil, want validation error")
		}
	})

	t.Run("Serve rejects nil context and closes listener", func(t *testing.T) {
		listener := newObservedListener(t)
		t.Cleanup(func() { _ = listener.Close() })
		server, err := New(validServerConfig(), &testRecognizer{})
		if err != nil {
			t.Fatal(err)
		}
		if err := server.Serve(nil, listener); err == nil {
			t.Fatal("Serve(nil, listener) error = nil, want validation error")
		}
		if !listener.closed.Load() {
			t.Fatal("Serve(nil, listener) did not close the owned listener")
		}
	})
}

func TestParentCancellationStopsAcceptingAndDrainsActiveRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	recognizer := &testRecognizer{recognize: func(ctx context.Context, _ httpapi.Image) (passport.Extraction, error) {
		close(started)
		select {
		case <-release:
			return passport.Extraction{Fields: map[string]passport.FieldResult{}}, nil
		case <-ctx.Done():
			cancelled <- struct{}{}
			return passport.Extraction{}, ctx.Err()
		}
	}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server, err := New(validServerConfig(), recognizer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := serveServer(t, server, ctx, cancel, listener)
	responseCh := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	requestDone := make(chan struct{})
	t.Cleanup(func() {
		run.cancel()
		select {
		case <-requestDone:
		case <-time.After(2 * time.Second):
			t.Error("active client request goroutine did not finish during cleanup")
		}
	})
	go func() {
		defer close(requestDone)
		response, err := postSyntheticUpload(context.Background(), "http://"+listener.Addr().String()+"/v1/passport/recognize")
		if err != nil {
			requestErr <- err
			return
		}
		responseCh <- response
	}()
	waitSignal(t, started, "recognizer to start")
	run.cancel()
	if err := waitDialFailure(listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
		t.Fatal("active recognizer context was cancelled by graceful parent cancellation")
	default:
	}
	close(release)
	select {
	case response := <-responseCh:
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("drained request status = %d, want 200", response.StatusCode)
		}
	case err := <-requestErr:
		t.Fatalf("active request failed before drain: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("active request did not finish after recognizer release")
	}
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("active client request goroutine did not finish")
	}
	if err := run.wait(); err != nil {
		t.Fatalf("Serve() after graceful drain = %v, want nil", err)
	}
}

func TestShutdownTimeoutForceClosesActiveRequest(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	recognizer := &testRecognizer{recognize: func(ctx context.Context, _ httpapi.Image) (passport.Extraction, error) {
		close(started)
		<-ctx.Done()
		cancelled <- struct{}{}
		return passport.Extraction{}, ctx.Err()
	}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	cfg := validServerConfig()
	cfg.HTTP.OCRTimeout = 2 * time.Second
	cfg.ReadTimeout = 400 * time.Millisecond
	cfg.ReadHeaderTimeout = 200 * time.Millisecond
	cfg.ShutdownTimeout = 100 * time.Millisecond
	cfg.WriteTimeout = 4 * time.Second
	server, err := New(cfg, recognizer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := serveServer(t, server, ctx, cancel, listener)
	requestDone := make(chan struct{})
	t.Cleanup(func() {
		run.cancel()
		select {
		case <-requestDone:
		case <-time.After(2 * time.Second):
			t.Error("active client request goroutine did not finish during cleanup")
		}
	})
	go func() {
		response, err := postSyntheticUpload(context.Background(), "http://"+listener.Addr().String()+"/v1/passport/recognize")
		if err == nil {
			response.Body.Close()
		}
		close(requestDone)
	}()
	waitSignal(t, started, "recognizer to start")
	run.cancel()
	select {
	case <-run.done:
		if err := run.err; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Serve() forced shutdown error = %v, want wrapped DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve() did not force close after shutdown timeout")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("forced connection close did not cancel active request context")
	}
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("active client request did not finish after forced close")
	}
}

func TestServePropagatesUnexpectedAcceptErrorAndClosesListener(t *testing.T) {
	wantErr := errors.New("accept sentinel")
	listener := newObservedListener(t)
	t.Cleanup(func() { _ = listener.Close() })
	listener.acceptErr = wantErr
	server, err := New(validServerConfig(), &testRecognizer{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Serve(ctx, listener); !errors.Is(err, wantErr) {
		t.Fatalf("Serve() error = %v, want accept sentinel", err)
	}
	if !listener.closed.Load() {
		t.Fatal("Serve() did not close listener after unexpected Accept error")
	}
}

func TestServerIsSingleUse(t *testing.T) {
	server, err := New(validServerConfig(), &testRecognizer{})
	if err != nil {
		t.Fatal(err)
	}
	firstListener := newObservedListener(t)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	run := serveServer(t, server, firstCtx, cancelFirst, firstListener)
	if err := run.stop(); err != nil {
		t.Fatalf("first Serve() = %v, want nil", err)
	}
	if !firstListener.closed.Load() {
		t.Fatal("first Serve() did not close listener")
	}

	secondListener := newObservedListener(t)
	t.Cleanup(func() { _ = secondListener.Close() })
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelSecond()
	if err := server.Serve(secondCtx, secondListener); err == nil {
		t.Fatal("second Serve() error = nil, want single-use error")
	}
	if !secondListener.closed.Load() {
		t.Fatal("rejected second Serve() did not close caller listener")
	}

	runCtx, cancelRun := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelRun()
	if err := server.Run(runCtx); err == nil {
		t.Fatal("Run() after Serve() error = nil, want single-use error")
	}
}

func TestServerEnforcesHeaderLimitAndHeaderReadTimeout(t *testing.T) {
	t.Run("oversized headers return 431", func(t *testing.T) {
		cfg := validServerConfig()
		cfg.ReadTimeout = time.Second
		cfg.ReadHeaderTimeout = time.Second
		cfg.WriteTimeout = 2 * time.Second
		cfg.ShutdownTimeout = time.Second
		run := startServer(t, cfg, &testRecognizer{})
		conn, err := net.Dial("tcp", run.listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		request := "GET /v1/passport/recognize HTTP/1.1\r\nHost: localhost\r\nX-Large: " + strings.Repeat("a", cfg.MaxHeaderBytes+4096) + "\r\n\r\n"
		if _, err := io.WriteString(conn, request); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		response, err := io.ReadAll(conn)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(response, []byte("431 Request Header Fields Too Large")) {
			t.Fatalf("response does not contain 431 status: %q", response)
		}
		_ = conn.Close()
		stopServer(t, run)
	})

	t.Run("incomplete headers are closed at configured timeout", func(t *testing.T) {
		cfg := validServerConfig()
		run := startServer(t, cfg, &testRecognizer{})
		conn, err := net.Dial("tcp", run.listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\nX-Stall: "); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buffer := make([]byte, 1)
		_, err = conn.Read(buffer)
		if err == nil {
			t.Fatal("Read() succeeded for incomplete header; expected connection close")
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatalf("connection remained open past header read timeout: %v", err)
		}
		_ = conn.Close()
		stopServer(t, run)
	})
}

func TestBodyReadTimeoutDoesNotInvokeRecognizer(t *testing.T) {
	cfg := validServerConfig()
	cfg.ShutdownTimeout = time.Second
	recognizer := &testRecognizer{}
	run := startServer(t, cfg, recognizer)
	conn, err := net.Dial("tcp", run.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := "POST /v1/passport/recognize HTTP/1.1\r\nHost: localhost\r\nContent-Type: multipart/form-data; boundary=body-stall\r\nContent-Length: 200\r\n\r\n--body-stall\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	clientDeadline := 2 * time.Second
	startedAt := time.Now()
	_ = conn.SetReadDeadline(startedAt.Add(clientDeadline))
	_, err = io.Copy(io.Discard, conn)
	if elapsed := time.Since(startedAt); elapsed >= clientDeadline {
		t.Fatalf("server did not close stalled body before client deadline: elapsed=%v", elapsed)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("stalled body read ended on client deadline, not server timeout: %v", err)
	}
	if got := recognizer.calls.Load(); got != 0 {
		t.Fatalf("recognizer calls = %d after timed-out body read, want 0", got)
	}
	_ = conn.Close()
	stopServer(t, run)
}

type fixedAddressListener struct {
	addr        net.Addr
	closed      atomic.Bool
	acceptCalls atomic.Int32
}

func (l *fixedAddressListener) Accept() (net.Conn, error) {
	l.acceptCalls.Add(1)
	return nil, net.ErrClosed
}

func (l *fixedAddressListener) Close() error {
	l.closed.Store(true)
	return nil
}

func (l *fixedAddressListener) Addr() net.Addr { return l.addr }

type observedListener struct {
	net.Listener
	closed    atomic.Bool
	acceptErr error
}

type acceptGateListener struct {
	net.Listener
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	closed  atomic.Bool
}

func (l *acceptGateListener) Accept() (net.Conn, error) {
	close(l.entered)
	<-l.release
	return nil, net.ErrClosed
}

func (l *acceptGateListener) Close() error {
	l.closed.Store(true)
	l.once.Do(func() { close(l.release) })
	return l.Listener.Close()
}

func newObservedListener(t *testing.T) *observedListener {
	t.Helper()
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return &observedListener{Listener: base}
}

func (l *observedListener) Accept() (net.Conn, error) {
	if l.acceptErr != nil {
		return nil, l.acceptErr
	}
	return l.Listener.Accept()
}

func (l *observedListener) Close() error {
	l.closed.Store(true)
	return l.Listener.Close()
}

func postSyntheticUpload(ctx context.Context, url string) (*http.Response, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="synthetic.png"`)
	header.Set("Content-Type", "image/png")
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write([]byte("synthetic-image-fixture")); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return (&http.Client{Timeout: 2 * time.Second}).Do(request)
}

type runningServer struct {
	listener net.Listener
	cancel   context.CancelFunc
	done     chan struct{}
	err      error
}

func (r *runningServer) wait() error {
	select {
	case <-r.done:
		return r.err
	case <-time.After(2 * time.Second):
		return errors.New("Serve() did not stop before test deadline")
	}
}

func (r *runningServer) stop() error {
	r.cancel()
	return r.wait()
}

type serverRunner interface {
	Serve(context.Context, net.Listener) error
}

func serveServer(t *testing.T, server serverRunner, ctx context.Context, cancel context.CancelFunc, listener net.Listener) *runningServer {
	t.Helper()
	run := &runningServer{listener: listener, cancel: cancel, done: make(chan struct{})}
	go func() {
		run.err = server.Serve(ctx, listener)
		close(run.done)
	}()
	t.Cleanup(func() {
		run.cancel()
		_ = run.listener.Close()
		select {
		case <-run.done:
		case <-time.After(2 * time.Second):
			t.Error("Serve() goroutine did not stop during cleanup")
		}
	})
	return run
}

func startServer(t *testing.T, cfg Config, recognizer httpapi.Recognizer) *runningServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(cfg, recognizer)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return serveServer(t, server, ctx, cancel, listener)
}

func stopServer(t *testing.T, run *runningServer) {
	t.Helper()
	if err := run.stop(); err != nil {
		t.Fatalf("Serve() after test cancellation = %v, want nil", err)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func waitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitDialFailure(address string) error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err != nil {
			return nil
		}
		conn.Close()
	}
	return fmt.Errorf("listener %s continued accepting connections after cancellation", address)
}
