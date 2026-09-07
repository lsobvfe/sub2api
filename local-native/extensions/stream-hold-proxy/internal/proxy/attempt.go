package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type attemptResult struct {
	SpoolPath         string
	Bytes             int64
	Status            int
	Message           string
	FailureClass      string
	TerminalOutcome   string
	TerminalType      string
	UpstreamRequestID string
	Duration          time.Duration
}

func (r attemptResult) successful() bool {
	return r.SpoolPath != ""
}

type lineResult struct {
	raw []byte
	err error
}

func (p *Proxy) performAttempt(ctx context.Context, source *http.Request, body []byte, protocol streamProtocol) attemptResult {
	startedAt := time.Now()
	attemptCtx, cancel := context.WithTimeout(ctx, p.cfg.AttemptMaxDuration)
	defer cancel()

	target := joinTargetURL(p.cfg.UpstreamURL, source.URL)
	request, err := http.NewRequestWithContext(
		attemptCtx,
		source.Method,
		target.String(),
		bytes.NewReader(body),
	)
	if err != nil {
		return failedAttempt(0, "build_upstream_request", fmt.Sprintf("build upstream request: %v", err), "", startedAt)
	}
	copyRequestHeaders(request.Header, source.Header)
	request.Host = p.cfg.UpstreamURL.Host

	response, err := p.client.Do(request)
	if err != nil {
		return failedAttempt(0, "upstream_transport", fmt.Sprintf("upstream transport: %v", err), "", startedAt)
	}
	defer response.Body.Close()

	upstreamRequestID := firstString(
		response.Header.Get("x-request-id"),
		response.Header.Get("request-id"),
	)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message := readErrorSnippet(attemptCtx, response.Body, p.cfg.StreamIdleTimeout)
		if message == "" {
			message = response.Status
		}
		return failedAttempt(response.StatusCode, "upstream_http_status", message, upstreamRequestID, startedAt)
	}

	if err := os.MkdirAll(p.cfg.SpoolDir, 0o700); err != nil {
		return failedAttempt(response.StatusCode, "spool_directory", fmt.Sprintf("create spool directory: %v", err), upstreamRequestID, startedAt)
	}
	spool, err := os.CreateTemp(p.cfg.SpoolDir, "attempt-*.sse")
	if err != nil {
		return failedAttempt(response.StatusCode, "spool_create", fmt.Sprintf("create attempt spool: %v", err), upstreamRequestID, startedAt)
	}
	spoolPath := spool.Name()
	keepSpool := false
	defer func() {
		_ = spool.Close()
		if !keepSpool {
			_ = os.Remove(spoolPath)
		}
	}()

	decoder := newSSEDecoder(protocol, p.cfg.MaxSSELineBytes)
	readCtx, cancelRead := context.WithCancel(attemptCtx)
	defer cancelRead()
	lines := make(chan lineResult, 1)
	go readResponseLines(readCtx, response.Body, p.cfg.MaxSSELineBytes, lines)

	idleTimer := time.NewTimer(p.cfg.StreamIdleTimeout)
	defer idleTimer.Stop()
	var written int64

	for {
		select {
		case <-attemptCtx.Done():
			_ = response.Body.Close()
			return annotateAttempt(
				failedAttempt(response.StatusCode, "attempt_context", attemptContextMessage(attemptCtx), upstreamRequestID, startedAt),
				written,
				terminalEvent{},
			)
		case <-idleTimer.C:
			cancelRead()
			_ = response.Body.Close()
			return annotateAttempt(
				failedAttempt(
					response.StatusCode,
					"upstream_stream_idle",
					fmt.Sprintf("upstream stream idle for %s", p.cfg.StreamIdleTimeout),
					upstreamRequestID,
					startedAt,
				),
				written,
				terminalEvent{},
			)
		case line := <-lines:
			if len(line.raw) > 0 {
				written += int64(len(line.raw))
				if written > p.cfg.MaxAttemptBodyBytes {
					cancelRead()
					_ = response.Body.Close()
					return annotateAttempt(
						failedAttempt(
							response.StatusCode,
							"upstream_stream_size_limit",
							fmt.Sprintf("upstream stream exceeds %d bytes", p.cfg.MaxAttemptBodyBytes),
							upstreamRequestID,
							startedAt,
						),
						written,
						terminalEvent{},
					)
				}
				if _, err := spool.Write(line.raw); err != nil {
					cancelRead()
					_ = response.Body.Close()
					return annotateAttempt(
						failedAttempt(response.StatusCode, "spool_write", fmt.Sprintf("write attempt spool: %v", err), upstreamRequestID, startedAt),
						written,
						terminalEvent{},
					)
				}
				terminal, err := decoder.Feed(line.raw)
				if err != nil {
					cancelRead()
					_ = response.Body.Close()
					return annotateAttempt(
						failedAttempt(response.StatusCode, "sse_decode", err.Error(), upstreamRequestID, startedAt),
						written,
						terminalEvent{},
					)
				}
				switch terminal.Kind {
				case terminalSuccess:
					cancelRead()
					_ = response.Body.Close()
					if err := spool.Sync(); err != nil {
						return annotateAttempt(
							failedAttempt(response.StatusCode, "spool_sync", fmt.Sprintf("sync attempt spool: %v", err), upstreamRequestID, startedAt),
							written,
							terminal,
						)
					}
					if err := spool.Close(); err != nil {
						return annotateAttempt(
							failedAttempt(response.StatusCode, "spool_close", fmt.Sprintf("close attempt spool: %v", err), upstreamRequestID, startedAt),
							written,
							terminal,
						)
					}
					keepSpool = true
					return attemptResult{
						SpoolPath:         spoolPath,
						Bytes:             written,
						Status:            response.StatusCode,
						TerminalOutcome:   terminal.Kind.String(),
						TerminalType:      terminal.Type,
						UpstreamRequestID: upstreamRequestID,
						Duration:          time.Since(startedAt),
					}
				case terminalFailure:
					cancelRead()
					_ = response.Body.Close()
					message := firstString(terminal.Message, terminal.Type, "upstream stream failed")
					return annotateAttempt(
						failedAttempt(response.StatusCode, "sse_terminal_failure", message, upstreamRequestID, startedAt),
						written,
						terminal,
					)
				}
				resetTimer(idleTimer, p.cfg.StreamIdleTimeout)
			}
			if line.err == nil {
				continue
			}
			if !errors.Is(line.err, io.EOF) {
				return annotateAttempt(
					failedAttempt(response.StatusCode, "upstream_stream_read", fmt.Sprintf("read upstream stream: %v", line.err), upstreamRequestID, startedAt),
					written,
					terminalEvent{},
				)
			}
			terminal, err := decoder.Finish()
			if err != nil {
				return annotateAttempt(
					failedAttempt(response.StatusCode, "sse_decode", err.Error(), upstreamRequestID, startedAt),
					written,
					terminalEvent{},
				)
			}
			if terminal.Kind == terminalSuccess {
				if err := spool.Sync(); err != nil {
					return annotateAttempt(
						failedAttempt(response.StatusCode, "spool_sync", fmt.Sprintf("sync attempt spool: %v", err), upstreamRequestID, startedAt),
						written,
						terminal,
					)
				}
				if err := spool.Close(); err != nil {
					return annotateAttempt(
						failedAttempt(response.StatusCode, "spool_close", fmt.Sprintf("close attempt spool: %v", err), upstreamRequestID, startedAt),
						written,
						terminal,
					)
				}
				keepSpool = true
				return attemptResult{
					SpoolPath:         spoolPath,
					Bytes:             written,
					Status:            response.StatusCode,
					TerminalOutcome:   terminal.Kind.String(),
					TerminalType:      terminal.Type,
					UpstreamRequestID: upstreamRequestID,
					Duration:          time.Since(startedAt),
				}
			}
			if terminal.Kind == terminalFailure {
				return annotateAttempt(
					failedAttempt(
						response.StatusCode,
						"sse_terminal_failure",
						firstString(terminal.Message, terminal.Type, "upstream stream failed"),
						upstreamRequestID,
						startedAt,
					),
					written,
					terminal,
				)
			}
			return annotateAttempt(
				failedAttempt(
					response.StatusCode,
					"upstream_missing_terminal",
					"upstream stream ended without a successful terminal event",
					upstreamRequestID,
					startedAt,
				),
				written,
				terminal,
			)
		}
	}
}

func readResponseLines(ctx context.Context, body io.Reader, maxLineBytes int, output chan<- lineResult) {
	reader := bufio.NewReaderSize(body, 64*1024)
	for {
		line, err := readBoundedLine(reader, maxLineBytes)
		result := lineResult{raw: line, err: err}
		select {
		case output <- result:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func readBoundedLine(reader *bufio.Reader, maxLineBytes int) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > maxLineBytes {
			return nil, fmt.Errorf("SSE line exceeds %d bytes", maxLineBytes)
		}
		line = append(line, fragment...)
		if err == nil {
			return line, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

func readErrorSnippet(ctx context.Context, body io.ReadCloser, timeout time.Duration) string {
	type result struct {
		body []byte
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		raw, err := io.ReadAll(io.LimitReader(body, 16*1024))
		resultCh <- result{body: raw, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		_ = body.Close()
		return attemptContextMessage(ctx)
	case <-timer.C:
		_ = body.Close()
		return fmt.Sprintf("upstream error body idle for %s", timeout)
	case result := <-resultCh:
		text := strings.TrimSpace(string(result.body))
		if text != "" {
			return truncate(text, 2048)
		}
		if result.err != nil {
			return result.err.Error()
		}
		return ""
	}
}

func failedAttempt(status int, failureClass, message, requestID string, startedAt time.Time) attemptResult {
	return attemptResult{
		Status:            status,
		Message:           strings.TrimSpace(message),
		FailureClass:      failureClass,
		UpstreamRequestID: requestID,
		Duration:          time.Since(startedAt),
	}
}

func annotateAttempt(result attemptResult, bytes int64, terminal terminalEvent) attemptResult {
	result.Bytes = bytes
	result.TerminalOutcome = terminal.Kind.String()
	result.TerminalType = terminal.Type
	return result
}

func attemptContextMessage(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "upstream attempt deadline exceeded"
	}
	if err := ctx.Err(); err != nil {
		return err.Error()
	}
	return "upstream attempt cancelled"
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func joinTargetURL(base *url.URL, requestURL *url.URL) *url.URL {
	target := *base
	target.Path = singleJoiningSlash(base.Path, requestURL.Path)
	target.RawPath = ""
	switch {
	case base.RawQuery == "":
		target.RawQuery = requestURL.RawQuery
	case requestURL.RawQuery == "":
		target.RawQuery = base.RawQuery
	default:
		target.RawQuery = base.RawQuery + "&" + requestURL.RawQuery
	}
	return &target
}

func singleJoiningSlash(left, right string) string {
	leftSlash := strings.HasSuffix(left, "/")
	rightSlash := strings.HasPrefix(right, "/")
	switch {
	case leftSlash && rightSlash:
		return left + right[1:]
	case !leftSlash && !rightSlash:
		return left + "/" + right
	default:
		return left + right
	}
}

func copyRequestHeaders(destination, source http.Header) {
	for key, values := range source {
		if isHopByHopHeader(key) ||
			strings.EqualFold(key, "Accept-Encoding") ||
			strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}
