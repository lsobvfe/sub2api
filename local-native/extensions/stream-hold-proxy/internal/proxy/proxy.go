package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	"sync"
	"time"
)

type Proxy struct {
	cfg        Config
	logger     *slog.Logger
	client     *http.Client
	transport  *http.Transport
	reverse    *httputil.ReverseProxy
	controller *enableController
	registry   *holdRegistry
	randomMu   sync.Mutex
	random     *mathrand.Rand
}

func New(cfg Config, logger *slog.Logger) (*Proxy, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	controller, err := newEnableController(cfg.Enabled, cfg.StateFile)
	if err != nil {
		return nil, err
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout
	transport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
	transport.MaxIdleConns = cfg.MaxIdleConnsPerHost * 2

	reverse := httputil.NewSingleHostReverseProxy(cfg.UpstreamURL)
	reverse.Transport = transport
	reverse.FlushInterval = -1
	reverse.ErrorHandler = func(w http.ResponseWriter, request *http.Request, err error) {
		logger.Warn("passthrough_failed",
			"method", request.Method,
			"path", request.URL.Path,
			"error", err,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	return &Proxy{
		cfg:        cfg,
		logger:     logger,
		client:     &http.Client{Transport: transport, CheckRedirect: noRedirect},
		transport:  transport,
		reverse:    reverse,
		controller: controller,
		registry:   newHoldRegistry(),
		random:     mathrand.New(mathrand.NewSource(time.Now().UnixNano())),
	}, nil
}

func (p *Proxy) Close() {
	if p != nil && p.transport != nil {
		p.transport.CloseIdleConnections()
	}
}

func (p *Proxy) Enabled() bool {
	return p != nil && p.controller.Enabled()
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if strings.HasPrefix(request.URL.Path, controlPrefix) {
		p.serveControl(w, request)
		return
	}
	if !p.controller.Enabled() || request.Method != http.MethodPost || !p.cfg.protects(request.URL.Path) {
		p.reverse.ServeHTTP(w, request)
		return
	}

	body, err := readReplayableBody(request, p.cfg.MaxRequestBodyBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	if !isStreamingRequest(body) {
		p.reverse.ServeHTTP(w, request)
		return
	}
	p.serveHeldStream(w, request, body)
}

func (p *Proxy) serveHeldStream(w http.ResponseWriter, request *http.Request, body []byte) {
	requestID := requestIdentity(request)
	startedAt := time.Now()
	p.registry.start(requestID, request.Method, request.URL.Path, startedAt)
	defer p.registry.finish(requestID)

	p.logger.Info("hold_request_started",
		"request_id", requestID,
		"method", request.Method,
		"path", request.URL.Path,
	)

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	header.Set("X-Stream-Hold-Proxy", "active")
	w.WriteHeader(http.StatusOK)

	controller := http.NewResponseController(w)
	_ = controller.EnableFullDuplex()
	if err := writeKeepalive(w, controller, "connected"); err != nil {
		p.logger.Info("hold_client_disconnected",
			"request_id", requestID,
			"attempts", 0,
			"held_for", time.Since(startedAt),
			"error", err,
		)
		return
	}

	keepaliveTicker := time.NewTicker(p.cfg.KeepaliveInterval)
	defer keepaliveTicker.Stop()

	for attempt := 1; ; attempt++ {
		if request.Context().Err() != nil {
			p.logClientCancellation(requestID, attempt-1, startedAt, request.Context().Err())
			return
		}
		p.registry.attempt(requestID, attempt, time.Now())
		p.logger.Info("hold_attempt_started",
			"request_id", requestID,
			"attempt", attempt,
			"path", request.URL.Path,
		)

		result, connected := p.waitForAttempt(w, controller, keepaliveTicker, request, body)
		if !connected {
			p.logClientCancellation(requestID, attempt, startedAt, request.Context().Err())
			return
		}
		if result.successful() {
			if err := replaySpool(w, controller, result.SpoolPath); err != nil {
				_ = os.Remove(result.SpoolPath)
				p.logger.Info("hold_client_disconnected",
					"request_id", requestID,
					"attempts", attempt,
					"held_for", time.Since(startedAt),
					"error", err,
				)
				return
			}
			_ = os.Remove(result.SpoolPath)
			p.logger.Info("hold_request_recovered",
				"request_id", requestID,
				"attempts", attempt,
				"held_for", time.Since(startedAt),
				"attempt_duration", result.Duration,
				"response_bytes", result.Bytes,
				"upstream_request_id", result.UpstreamRequestID,
			)
			return
		}

		delay := p.retryDelay(attempt)
		nextRetry := time.Now().Add(delay)
		p.registry.failed(requestID, result.Status, result.Message, nextRetry)
		p.logger.Warn("hold_attempt_failed",
			"request_id", requestID,
			"attempt", attempt,
			"status", result.Status,
			"error", result.Message,
			"attempt_duration", result.Duration,
			"upstream_request_id", result.UpstreamRequestID,
			"retry_in", delay,
		)
		if !p.waitForRetry(w, controller, keepaliveTicker, request.Context(), delay) {
			p.logClientCancellation(requestID, attempt, startedAt, request.Context().Err())
			return
		}
	}
}

func (p *Proxy) waitForAttempt(
	w http.ResponseWriter,
	controller *http.ResponseController,
	keepaliveTicker *time.Ticker,
	request *http.Request,
	body []byte,
) (attemptResult, bool) {
	attemptCtx, cancel := context.WithCancel(request.Context())
	defer cancel()

	resultCh := make(chan attemptResult, 1)
	go func() {
		resultCh <- p.performAttempt(attemptCtx, request, body)
	}()

	for {
		select {
		case result := <-resultCh:
			return result, true
		case <-request.Context().Done():
			cancel()
			return attemptResult{}, false
		case <-keepaliveTicker.C:
			if err := writeKeepalive(w, controller, "waiting"); err != nil {
				cancel()
				return attemptResult{}, false
			}
		}
	}
}

func (p *Proxy) waitForRetry(
	w http.ResponseWriter,
	controller *http.ResponseController,
	keepaliveTicker *time.Ticker,
	ctx context.Context,
	delay time.Duration,
) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case <-keepaliveTicker.C:
			if err := writeKeepalive(w, controller, "waiting"); err != nil {
				return false
			}
		}
	}
}

func (p *Proxy) retryDelay(attempt int) time.Duration {
	delay := p.cfg.RetryMinInterval
	for step := 1; step < attempt && delay < p.cfg.RetryMaxInterval; step++ {
		if delay > p.cfg.RetryMaxInterval/2 {
			delay = p.cfg.RetryMaxInterval
			break
		}
		delay *= 2
	}
	if delay > p.cfg.RetryMaxInterval {
		delay = p.cfg.RetryMaxInterval
	}
	if delay <= 0 || p.cfg.RetryJitterRatio == 0 {
		return delay
	}

	p.randomMu.Lock()
	factor := 1 + ((p.random.Float64()*2)-1)*p.cfg.RetryJitterRatio
	p.randomMu.Unlock()
	jittered := time.Duration(float64(delay) * factor)
	if jittered < 0 {
		return 0
	}
	return jittered
}

func (p *Proxy) logClientCancellation(requestID string, attempts int, startedAt time.Time, err error) {
	p.logger.Info("hold_client_disconnected",
		"request_id", requestID,
		"attempts", attempts,
		"held_for", time.Since(startedAt),
		"error", err,
	)
}

func writeKeepalive(w http.ResponseWriter, controller *http.ResponseController, phase string) error {
	if _, err := fmt.Fprintf(w, ": stream-hold %s\n\n", phase); err != nil {
		return err
	}
	return controller.Flush()
}

func replaySpool(w http.ResponseWriter, controller *http.ResponseController, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	buffer := make([]byte, 64*1024)
	if _, err := io.CopyBuffer(w, file, buffer); err != nil {
		return err
	}
	return controller.Flush()
}

func readReplayableBody(request *http.Request, limit int64) ([]byte, error) {
	if request.Body == nil {
		return nil, nil
	}
	defer request.Body.Close()
	body, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("request body exceeds %d bytes", limit)
	}
	return body, nil
}

func isStreamingRequest(body []byte) bool {
	var request struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &request) == nil && request.Stream
}

func requestIdentity(request *http.Request) string {
	for _, header := range []string{"x-client-request-id", "x-request-id", "traceparent"} {
		if value := strings.TrimSpace(request.Header.Get(header)); value != "" {
			return truncate(value, 128)
		}
	}
	randomBytes := make([]byte, 12)
	if _, err := rand.Read(randomBytes); err == nil {
		return hex.EncodeToString(randomBytes)
	}
	return fmt.Sprintf("hold-%d", time.Now().UnixNano())
}

func noRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
