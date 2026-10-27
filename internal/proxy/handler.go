package proxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-pass/internal/auth"
	debuglog "codex-pass/internal/debug"
	"codex-pass/internal/requestlog"
	"codex-pass/internal/sse"
)

const (
	defaultMaxRequestBody = 32 << 20
	defaultMaxAggregate   = 64 << 20
	maxErrorBody          = 4 << 20
)

type Config struct {
	APIKey            string
	EnableCodex       bool
	AuthPool          *auth.Pool
	HTTPClient        *http.Client
	UpstreamURL       string
	ModelsUpstreamURL string
	CodexUpstreamURL  string
	Logger            *debuglog.Logger
	RequestLog        requestlog.Recorder
	MaxBodyBytes      int64
	MaxAggregate      int64
	UserAgent         string
}

type Server struct {
	apiKey            string
	enableCodex       bool
	authPool          *auth.Pool
	httpClient        *http.Client
	upstreamURL       string
	modelsUpstreamURL string
	codexUpstreamURL  string
	logger            *debuglog.Logger
	requestLog        requestlog.Recorder
	maxBodyBytes      int64
	maxAggregate      int64
	userAgent         string
	requestSeq        atomic.Uint64
}

func New(config Config) (*Server, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("proxy API key is empty")
	}
	if config.AuthPool == nil {
		return nil, errors.New("auth pool is required")
	}
	if config.HTTPClient == nil {
		return nil, errors.New("HTTP client is required")
	}
	if config.UpstreamURL == "" {
		config.UpstreamURL = DefaultUpstreamURL
	}
	if config.ModelsUpstreamURL == "" {
		config.ModelsUpstreamURL = DefaultModelsUpstreamURL
	}
	if config.CodexUpstreamURL == "" {
		config.CodexUpstreamURL = DefaultCodexUpstreamURL
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = defaultMaxRequestBody
	}
	if config.MaxAggregate == 0 {
		config.MaxAggregate = defaultMaxAggregate
	}
	if config.UserAgent == "" {
		config.UserAgent = "codex-pass/0.1"
	}
	return &Server{
		apiKey: config.APIKey, authPool: config.AuthPool, httpClient: config.HTTPClient,
		enableCodex: config.EnableCodex,
		upstreamURL: config.UpstreamURL, modelsUpstreamURL: config.ModelsUpstreamURL,
		codexUpstreamURL: config.CodexUpstreamURL,
		logger:           config.Logger, requestLog: config.RequestLog, maxBodyBytes: config.MaxBodyBytes,
		maxAggregate: config.MaxAggregate, userAgent: config.UserAgent,
	}, nil
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	toolPath, toolRequest := codexToolPath(request.URL.Path)
	codex := request.URL.Path == "/codex/models" || request.URL.Path == "/codex/responses" || (toolRequest && strings.HasPrefix(request.URL.Path, "/codex/"))
	if codex && !s.enableCodex {
		writeError(writer, http.StatusNotFound, "not found")
		return
	}
	if codex && request.URL.Path == "/codex/responses" && request.Method == http.MethodGet && strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
		// Codex treats 426 as an immediate switch to HTTP/SSE. A generic 405
		// would exhaust its WebSocket retry budget before falling back.
		writeError(writer, http.StatusUpgradeRequired, "use HTTP POST with SSE")
		return
	}
	switch request.URL.Path {
	case "/healthz":
		if request.Method != http.MethodGet {
			writer.Header().Set("Allow", http.MethodGet)
			writeError(writer, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"ok"}\n`))
		return
	case "/v1/models", "/codex/models":
		if request.Method != http.MethodGet {
			writer.Header().Set("Allow", http.MethodGet)
			writeError(writer, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
	case "/v1/responses", "/codex/responses":
		if request.Method != http.MethodPost {
			writer.Header().Set("Allow", http.MethodPost)
			writeError(writer, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
	case "/v1/chat/completions":
		if request.Method != http.MethodPost {
			writer.Header().Set("Allow", http.MethodPost)
			writeError(writer, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
	case "/v1/alpha/search", "/codex/alpha/search", "/v1/images/generations", "/codex/images/generations", "/v1/images/edits", "/codex/images/edits":
		if request.Method != http.MethodPost {
			writer.Header().Set("Allow", http.MethodPost)
			writeError(writer, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
	default:
		writeError(writer, http.StatusNotFound, "not found")
		return
	}
	// The opt-in Codex entry receives the client's ChatGPT credentials, which
	// cannot carry our local API key. It is not a token-validation endpoint.
	if !codex && !s.authorized(request.Header.Get("Authorization")) {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeError(writer, http.StatusUnauthorized, "invalid API key")
		return
	}
	if s.logger != nil && s.logger.Err() != nil {
		writeError(writer, http.StatusInternalServerError, "debug logger is unavailable")
		return
	}
	if request.URL.Path == "/v1/models" || request.URL.Path == "/codex/models" {
		s.handleModels(writer, request)
		return
	}
	if s.requestLog != nil && s.requestLog.Err() != nil {
		writeError(writer, http.StatusInternalServerError, "request log is unavailable")
		return
	}
	if request.URL.Path == "/v1/chat/completions" {
		s.handleChatCompletions(writer, request)
		return
	}
	if toolRequest {
		s.handleCodexTool(writer, request, toolPath)
		return
	}
	s.handleResponsesMode(writer, request, codex)
}

func (s *Server) authorized(header string) bool {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.apiKey)) == 1
}

func (s *Server) handleResponsesMode(writer http.ResponseWriter, request *http.Request, codex bool) {
	started := time.Now()
	requestID := s.newRequestID()
	endpoint := "responses"
	if codex {
		endpoint = "codex/responses"
	}
	metrics, logErr := s.beginRequestLog(request, requestID, endpoint, started)
	if logErr != nil {
		s.log(requestID, "ERROR", map[string]any{"error": debuglog.Error(logErr), "operation": "start request log"})
		writeError(writer, http.StatusInternalServerError, "request log is unavailable")
		return
	}
	defer func() {
		if finishErr := metrics.finish(); finishErr != nil {
			s.log(requestID, "ERROR", map[string]any{"error": debuglog.Error(finishErr), "operation": "finish request log"})
			log.Printf("request usage log error for %s: %v", requestID, finishErr)
		}
	}()
	body, readErr := s.readBody(request.Body)
	if readErr != nil {
		metrics.failure("request_body_read")
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": http.StatusRequestEntityTooLarge, "error": debuglog.Error(readErr)})
		writeError(writer, http.StatusRequestEntityTooLarge, readErr.Error())
		return
	}
	metadataBody := body
	if codex {
		var err error
		metadataBody, err = s.decodeCodexBody(body, request.Header.Get("Content-Encoding"))
		if err != nil {
			metrics.failure("invalid_request_encoding")
			status := http.StatusBadRequest
			if errors.Is(err, errUnsupportedEncoding) {
				status = http.StatusUnsupportedMediaType
			}
			writeError(writer, status, err.Error())
			return
		}
	}
	if err := metrics.setRequestMetadata(metadataBody); err != nil {
		metrics.failure("request_log_metadata")
		s.log(requestID, "ERROR", map[string]any{"error": debuglog.Error(err), "operation": "update request log metadata"})
		writeError(writer, http.StatusInternalServerError, "request log is unavailable")
		return
	}
	var stream bool
	var promptCacheKey string
	var err error
	upstreamBody := body
	if codex {
		_, stream, promptCacheKey, err = parseResponseRequest(metadataBody)
	} else {
		stream, upstreamBody, promptCacheKey, err = prepareBody(body)
	}
	if err != nil {
		metrics.failure("invalid_request")
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": http.StatusBadRequest, "error": debuglog.Error(err)})
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	sessionID := request.Header.Get("session-id")
	if sessionID == "" && promptCacheKey != "" {
		sessionID = promptCacheKey
	}
	accountIndex, credential := s.authPool.Select(promptCacheKey, request.Header.Get("session-id"))
	target := s.upstreamURL
	if codex && request.URL.RawQuery != "" {
		parsed, parseErr := url.Parse(target)
		if parseErr != nil {
			metrics.failure("invalid_upstream_url")
			writeError(writer, http.StatusBadGateway, "invalid responses upstream URL")
			return
		}
		parsed.RawQuery = request.URL.RawQuery
		target = parsed.String()
	}
	if s.logger != nil {
		diffBody := upstreamBody
		if codex {
			diffBody = metadataBody
		}
		diff, err := DiffJSON(metadataBody, diffBody)
		if err != nil {
			metrics.failure("invalid_request")
			writeError(writer, http.StatusBadRequest, "invalid JSON request: "+err.Error())
			return
		}
		s.log(requestID, "CLIENT_REQUEST", map[string]any{
			"method": request.Method, "url": request.URL.String(), "headers": debuglog.Headers(request.Header),
			"body": string(metadataBody), "stream": stream, "mode": endpoint,
			"body_encoding": request.Header.Get("Content-Encoding"), "body_bytes": len(body),
		})
		s.log(requestID, "AUTH_SELECTION", map[string]any{
			"auth_file_index": accountIndex, "auth_file_format": credential.Format(),
			"prompt_cache_key": promptCacheKey, "session_id": sessionID,
		})
		s.log(requestID, "REQUEST_DIFF", map[string]any{
			"method": http.MethodPost, "url": target, "body": string(diffBody),
			"json_diff": diff, "prompt_cache_key": promptCacheKey, "session_id": sessionID,
		})
	}

	if err := s.authPool.EnsureFresh(request.Context(), credential); err != nil {
		metrics.failureForError("oauth_token_refresh", err)
		s.log(requestID, "ERROR", map[string]any{"error": debuglog.Error(err), "operation": "proactive token refresh"})
		writeError(writer, http.StatusBadGateway, "OAuth token refresh failed: "+debuglog.Error(err))
		return
	}

	upstreamHeaders := request.Header
	if !stream || codex || s.logger != nil || s.requestLog != nil {
		// Aggregation, debug tracing, and request accounting all parse the
		// upstream SSE stream. Request uncompressed bytes for those paths so a
		// client-negotiated content encoding is never parsed as SSE data.
		upstreamHeaders = request.Header.Clone()
		upstreamHeaders.Set("Accept-Encoding", "identity")
	}
	send := func() (*http.Response, error) {
		return s.sendUpstreamRequest(request.Context(), http.MethodPost, target, upstreamHeaders, upstreamBody, credential, sessionID, requestID)
	}
	upstream, err := send()
	if err != nil {
		metrics.failureForError("upstream_request", err)
		s.log(requestID, "ERROR", map[string]any{"error": debuglog.Error(err), "operation": "upstream request"})
		writeError(writer, http.StatusBadGateway, "upstream request failed: "+debuglog.Error(err))
		return
	}
	if upstream.StatusCode == http.StatusUnauthorized {
		upstream = s.retryUpstreamUnauthorized(request.Context(), credential, upstream, requestID, send)
		if upstream == nil {
			metrics.failureForError("upstream_authentication_refresh", request.Context().Err())
			writeError(writer, http.StatusBadGateway, "upstream authentication refresh failed")
			return
		}
	}
	defer upstream.Body.Close()
	if s.logger != nil {
		s.log(requestID, "UPSTREAM_RESPONSE_HEADERS", map[string]any{
			"status": upstream.StatusCode, "headers": debuglog.Headers(upstream.Header),
		})
	}

	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		metrics.failure("upstream_http_status")
		s.copyUpstreamResponse(writer, upstream)
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": upstream.StatusCode, "mode": "upstream_error"})
		s.logSummary(requestID, started, time.Time{}, nil, accountIndex, promptCacheKey, sessionID)
		return
	}

	if codex && strings.HasPrefix(strings.ToLower(upstream.Header.Get("Content-Type")), "application/json") {
		s.forwardCodexJSON(writer, upstream, metrics, requestID, started, accountIndex, promptCacheKey, sessionID)
		return
	}
	if stream || codex {
		firstEvent, final, terminalKind, streamErr := s.forwardStream(writer, upstream.Body, upstream.Header, upstream.StatusCode, requestID)
		if len(final) != 0 {
			metrics.terminal(final, terminalKind)
		}
		if streamErr != nil {
			metrics.failureForError("stream_forward", streamErr)
			s.log(requestID, "ERROR", map[string]any{"error": debuglog.Error(streamErr), "operation": "stream forward"})
		} else if len(final) == 0 {
			metrics.failure("stream_ended_without_completed")
		}
		if len(final) != 0 {
			s.log(requestID, "UPSTREAM_COMPLETED", map[string]any{"response": string(final)})
		}
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": upstream.StatusCode, "mode": "stream"})
		s.logSummary(requestID, started, firstEvent, final, accountIndex, promptCacheKey, sessionID)
		return
	}

	firstEvent := time.Time{}
	collector := sse.Collector{
		MaxBytes: s.maxAggregate,
		OnEvent: func(event sse.Event) {
			if firstEvent.IsZero() {
				firstEvent = time.Now()
			}
			if s.logger != nil {
				s.log(requestID, "UPSTREAM_SSE", map[string]any{"event": event.Event, "id": event.ID, "data": string(event.Data)})
			}
		},
	}
	final, collectErr := collector.Collect(upstream.Body)
	if collectErr != nil {
		terminalResponse, terminalKind := responseFromTerminalError(collectErr)
		if len(terminalResponse) != 0 {
			metrics.terminal(terminalResponse, terminalKind)
		}
		metrics.failureForError("stream_aggregation", collectErr)
		s.writeCollectError(writer, collectErr)
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": statusForCollectError(collectErr), "error": debuglog.Error(collectErr)})
		s.logSummary(requestID, started, firstEvent, terminalResponse, accountIndex, promptCacheKey, sessionID)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	if _, writeErr := writer.Write(final); writeErr != nil {
		metrics.failureForError("client_response_write", writeErr)
	} else {
		metrics.complete(final)
	}
	s.log(requestID, "UPSTREAM_COMPLETED", map[string]any{"response": string(final)})
	s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": http.StatusOK, "mode": "aggregated", "body": string(final)})
	s.logSummary(requestID, started, firstEvent, final, accountIndex, promptCacheKey, sessionID)
}

func (s *Server) retryUnauthorized(ctx context.Context, clientHeaders http.Header, body []byte, credential *auth.Credential, sessionID string, first *http.Response, requestID string) *http.Response {
	return s.retryUpstreamUnauthorized(ctx, credential, first, requestID, func() (*http.Response, error) {
		return s.sendUpstream(ctx, clientHeaders, body, credential, sessionID, requestID)
	})
}

func (s *Server) retryUpstreamUnauthorized(ctx context.Context, credential *auth.Credential, first *http.Response, requestID string, retry func() (*http.Response, error)) *http.Response {
	firstHeaders := first.Header.Clone()
	if s.logger != nil {
		s.log(requestID, "UPSTREAM_RESPONSE_HEADERS", map[string]any{
			"status": first.StatusCode, "headers": debuglog.Headers(first.Header), "attempt": 1,
		})
	}
	firstBody, _ := io.ReadAll(io.LimitReader(first.Body, maxErrorBody+1))
	first.Body.Close()
	s.log(requestID, "ERROR", map[string]any{"operation": "upstream returned 401; forcing one OAuth refresh"})
	if err := s.authPool.Refresh(ctx, credential); err != nil {
		s.log(requestID, "ERROR", map[string]any{"operation": "forced token refresh", "error": debuglog.Error(err)})
		if int64(len(firstBody)) <= maxErrorBody {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: firstHeaders, Body: io.NopCloser(bytes.NewReader(firstBody))}
		}
		return nil
	}
	second, err := retry()
	if err != nil {
		s.log(requestID, "ERROR", map[string]any{"operation": "retry after OAuth refresh", "error": debuglog.Error(err)})
		return nil
	}
	return second
}

func (s *Server) sendUpstream(ctx context.Context, clientHeaders http.Header, body []byte, credential *auth.Credential, sessionID, requestID string) (*http.Response, error) {
	return s.sendUpstreamRequest(ctx, http.MethodPost, s.upstreamURL, clientHeaders, body, credential, sessionID, requestID)
}

func (s *Server) sendUpstreamRequest(ctx context.Context, method, target string, clientHeaders http.Header, body []byte, credential *auth.Credential, sessionID, requestID string) (*http.Response, error) {
	accept := "text/event-stream"
	if method == http.MethodGet {
		accept = "application/json"
	}
	return s.sendUpstreamAccept(ctx, method, target, clientHeaders, body, credential, sessionID, requestID, accept)
}

func (s *Server) sendUpstreamAccept(ctx context.Context, method, target string, clientHeaders http.Header, body []byte, credential *auth.Credential, sessionID, requestID, accept string) (*http.Response, error) {
	snapshot := credential.Snapshot()
	request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header = cloneForwardHeaders(clientHeaders)
	request.Header.Set("Accept", accept)
	if method == http.MethodGet {
		request.Header.Del("Content-Type")
	}
	request.Header.Set("Authorization", "Bearer "+snapshot.Token)
	request.Header.Set("ChatGPT-Account-ID", snapshot.AccountID)
	if request.Header.Get("Originator") == "" {
		request.Header.Set("Originator", "codex-pass")
	}
	if request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", s.userAgent)
	}
	if sessionID != "" {
		request.Header.Set("session-id", sessionID)
	}
	if s.logger != nil {
		logBody := body
		if encoding := clientHeaders.Get("Content-Encoding"); encoding != "" {
			if decoded, decodeErr := s.decodeCodexBody(body, encoding); decodeErr == nil {
				logBody = decoded
			}
		}
		s.log(requestID, "UPSTREAM_REQUEST", map[string]any{
			"method": request.Method, "url": request.URL.String(), "headers": debuglog.Headers(request.Header), "body": string(logBody),
			"body_encoding": clientHeaders.Get("Content-Encoding"), "body_bytes": len(body),
		})
	}
	return s.httpClient.Do(request)
}

func (s *Server) forwardStream(writer http.ResponseWriter, body io.Reader, headers http.Header, status int, requestID string) (time.Time, json.RawMessage, string, error) {
	for key, values := range responseHeaders(headers) {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	writer.WriteHeader(status)
	flusher, _ := writer.(http.Flusher)
	var trace *streamTrace
	var pipeWriter *io.PipeWriter
	var wait sync.WaitGroup
	if s.logger != nil || s.requestLog != nil {
		trace = &streamTrace{}
		pipeReader, createdWriter := io.Pipe()
		pipeWriter = createdWriter
		wait.Add(1)
		go func() {
			defer wait.Done()
			trace.consume(pipeReader, s.maxAggregate, func(event sse.Event) {
				if s.logger != nil {
					s.log(requestID, "UPSTREAM_SSE", map[string]any{"event": event.Event, "id": event.ID, "data": string(event.Data)})
				}
			})
		}()
	}
	buffer := make([]byte, 32<<10)
	firstEvent := time.Time{}
	for {
		read, readErr := body.Read(buffer)
		if read > 0 {
			if firstEvent.IsZero() {
				firstEvent = time.Now()
			}
			chunk := buffer[:read]
			if _, writeErr := writer.Write(chunk); writeErr != nil {
				if pipeWriter != nil {
					_ = pipeWriter.CloseWithError(writeErr)
				}
				wait.Wait()
				response, kind := terminalResponseFromTrace(trace)
				return firstEvent, response, kind, writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
			if pipeWriter != nil {
				if _, writeErr := pipeWriter.Write(chunk); writeErr != nil {
					wait.Wait()
					response, kind := terminalResponseFromTrace(trace)
					return firstEvent, response, kind, writeErr
				}
			}
		}
		if readErr != nil {
			if pipeWriter != nil {
				_ = pipeWriter.CloseWithError(readErr)
				wait.Wait()
			}
			if errors.Is(readErr, io.EOF) {
				response, kind := terminalResponseFromTrace(trace)
				return firstEvent, response, kind, nil
			}
			response, kind := terminalResponseFromTrace(trace)
			return firstEvent, response, kind, readErr
		}
	}
}

func terminalResponseFromTrace(trace *streamTrace) (json.RawMessage, string) {
	if trace == nil {
		return nil, ""
	}
	return trace.terminalResponse()
}

func (s *Server) copyUpstreamResponse(writer http.ResponseWriter, upstream *http.Response) {
	for key, values := range responseHeaders(upstream.Header) {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	writer.WriteHeader(upstream.StatusCode)
	_, _ = io.Copy(writer, upstream.Body)
}

func (s *Server) writeCollectError(writer http.ResponseWriter, err error) {
	var terminal *sse.TerminalError
	if errors.As(err, &terminal) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadGateway)
		_, _ = writer.Write([]byte(`{"error":{"message":"upstream response did not complete","type":"upstream_sse_error","upstream":`))
		_, _ = writer.Write(terminal.Data)
		_, _ = writer.Write([]byte("}}"))
		return
	}
	writeError(writer, statusForCollectError(err), "upstream SSE aggregation failed: "+debuglog.Error(err))
}

func responseFromTerminalError(err error) (json.RawMessage, string) {
	var terminal *sse.TerminalError
	if !errors.As(err, &terminal) {
		return nil, ""
	}
	var envelope struct {
		Type     string          `json:"type"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(terminal.Data, &envelope) != nil || len(bytes.TrimSpace(envelope.Response)) == 0 {
		return nil, terminal.Kind
	}
	kind := envelope.Type
	if kind == "" {
		kind = terminal.Kind
	}
	return append(json.RawMessage(nil), envelope.Response...), kind
}

func statusForCollectError(err error) int {
	if errors.Is(err, sse.ErrTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadGateway
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]string{"message": message}})
}

func (s *Server) readBody(body io.ReadCloser) ([]byte, error) {
	defer body.Close()
	limited := io.LimitReader(body, s.maxBodyBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	if int64(len(data)) > s.maxBodyBytes {
		return nil, fmt.Errorf("request body exceeds %d-byte limit", s.maxBodyBytes)
	}
	return data, nil
}

func parseResponseRequest(body []byte) (object map[string]json.RawMessage, stream bool, promptCacheKey string, err error) {
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("request body must be a JSON object")
		}
		return nil, false, "", fmt.Errorf("invalid request JSON: %w", err)
	}
	if rawCacheKey, found := object["prompt_cache_key"]; found {
		_ = json.Unmarshal(rawCacheKey, &promptCacheKey)
	}
	rawStream, found := object["stream"]
	if found {
		if err := json.Unmarshal(rawStream, &stream); err != nil {
			return nil, false, "", errors.New("top-level stream must be a JSON boolean")
		}
	}
	return object, stream, promptCacheKey, nil
}

func prepareBody(body []byte) (stream bool, upstream []byte, promptCacheKey string, err error) {
	object, stream, promptCacheKey, err := parseResponseRequest(body)
	if err != nil {
		return false, nil, "", err
	}
	inputWrapped := false
	if rawInput, found := object["input"]; found {
		var inputText string
		if json.Unmarshal(rawInput, &inputText) == nil {
			wrapped, wrapErr := json.Marshal([]any{map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{map[string]string{
					"type": "input_text",
					"text": inputText,
				}},
			}})
			if wrapErr != nil {
				return false, nil, "", fmt.Errorf("wrap string input: %w", wrapErr)
			}
			// Codex's upstream endpoint requires an input item list. This is the
			// canonical user-message shape emitted by the official Codex client.
			object["input"] = wrapped
			inputWrapped = true
		}
	}
	fastTierMapped := false
	if rawTier, found := object["service_tier"]; found {
		var tier string
		if json.Unmarshal(rawTier, &tier) == nil && tier == "fast" {
			// The current Codex upstream rejects "fast" but accepts "priority".
			// This is intentionally a narrow, observable compatibility mapping.
			object["service_tier"] = json.RawMessage(`"priority"`)
			fastTierMapped = true
		}
	}
	storeForced := false
	if rawStore, found := object["store"]; found {
		var store bool
		if json.Unmarshal(rawStore, &store) != nil || store {
			object["store"] = json.RawMessage("false")
			storeForced = true
		}
	} else {
		// The Codex upstream currently requires an explicit false value.
		object["store"] = json.RawMessage("false")
		storeForced = true
	}
	temperatureRemoved := false
	if _, found := object["temperature"]; found {
		// The Codex upstream currently rejects the Responses API temperature field.
		delete(object, "temperature")
		temperatureRemoved = true
	}
	cacheOptionsRemoved := false
	if _, found := object["prompt_cache_options"]; found {
		delete(object, "prompt_cache_options")
		cacheOptionsRemoved = true
	}
	cacheBreakpointsRemoved := false
	if _, found := object["prompt_cache_breakpoint"]; found {
		delete(object, "prompt_cache_breakpoint")
		cacheBreakpointsRemoved = true
	}
	for key, rawValue := range object {
		rewritten, removed, rewriteErr := removeJSONField(rawValue, "prompt_cache_breakpoint")
		if rewriteErr != nil {
			return false, nil, "", fmt.Errorf("remove prompt_cache_breakpoint: %w", rewriteErr)
		}
		if removed {
			object[key] = rewritten
			cacheBreakpointsRemoved = true
		}
	}
	if stream && !inputWrapped && !fastTierMapped && !storeForced && !temperatureRemoved && !cacheOptionsRemoved && !cacheBreakpointsRemoved {
		return true, append([]byte(nil), body...), promptCacheKey, nil
	}
	if !stream {
		object["stream"] = json.RawMessage("true")
	}
	upstream, err = json.Marshal(object)
	if err != nil {
		return false, nil, "", fmt.Errorf("encode upstream request: %w", err)
	}
	return stream, upstream, promptCacheKey, nil
}

// removeJSONField removes every matching object field while preserving raw JSON
// for subtrees which do not contain the field. This avoids number coercion in
// otherwise unknown request fields.
func removeJSONField(value json.RawMessage, field string) (json.RawMessage, bool, error) {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 {
		return nil, false, errors.New("empty JSON value")
	}
	switch trimmed[0] {
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return nil, false, err
		}
		changed := false
		if _, found := object[field]; found {
			delete(object, field)
			changed = true
		}
		for key, rawChild := range object {
			rewritten, childChanged, err := removeJSONField(rawChild, field)
			if err != nil {
				return nil, false, err
			}
			if childChanged {
				object[key] = rewritten
				changed = true
			}
		}
		if !changed {
			return value, false, nil
		}
		encoded, err := json.Marshal(object)
		return encoded, true, err
	case '[':
		var array []json.RawMessage
		if err := json.Unmarshal(trimmed, &array); err != nil {
			return nil, false, err
		}
		changed := false
		for index, rawChild := range array {
			rewritten, childChanged, err := removeJSONField(rawChild, field)
			if err != nil {
				return nil, false, err
			}
			if childChanged {
				array[index] = rewritten
				changed = true
			}
		}
		if !changed {
			return value, false, nil
		}
		encoded, err := json.Marshal(array)
		return encoded, true, err
	default:
		return value, false, nil
	}
}

func (s *Server) newRequestID() string {
	sequence := s.requestSeq.Add(1)
	return fmt.Sprintf("req_%d_%d", time.Now().UTC().UnixNano(), sequence)
}

func (s *Server) log(requestID, stage string, fields map[string]any) {
	if s.logger != nil {
		s.logger.Log(requestID, stage, fields)
	}
}

func (s *Server) logSummary(requestID string, started, firstEvent time.Time, final json.RawMessage, authIndex int, cacheKey, sessionID string) {
	if s.logger == nil {
		return
	}
	fields := map[string]any{
		"duration_ms": time.Since(started).Milliseconds(), "auth_file_index": authIndex,
		"prompt_cache_key": cacheKey, "session_id": sessionID,
	}
	if !firstEvent.IsZero() {
		fields["first_sse_event_ms"] = firstEvent.Sub(started).Milliseconds()
	}
	if usage := responseUsage(final); usage != nil {
		fields["usage"] = usage
	}
	if cacheUsage := responseCacheUsage(final); cacheUsage != nil {
		fields["cache_usage"] = cacheUsage
	}
	s.log(requestID, "REQUEST_SUMMARY", fields)
}

func responseUsage(response json.RawMessage) any {
	if len(response) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(response, &object) != nil {
		return nil
	}
	raw, found := object["usage"]
	if !found {
		return map[string]any{"state": "absent"}
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return map[string]any{"state": "null"}
	}
	var usage any
	if json.Unmarshal(raw, &usage) != nil {
		return string(raw)
	}
	return usage
}

// responseCacheUsage represents absence, null, zero, and positive values
// explicitly. It reports upstream values only and never infers cache behavior.
func responseCacheUsage(response json.RawMessage) map[string]any {
	if len(response) == 0 {
		return nil
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(response, &top) != nil {
		return nil
	}
	usage, found := top["usage"]
	if !found {
		return map[string]any{"usage": map[string]any{"state": "absent"}}
	}
	if bytes.Equal(bytes.TrimSpace(usage), []byte("null")) {
		return map[string]any{"usage": map[string]any{"state": "null"}}
	}
	var usageObject map[string]json.RawMessage
	if json.Unmarshal(usage, &usageObject) != nil {
		return map[string]any{"usage": map[string]any{"state": "invalid"}}
	}
	result := map[string]any{"input_tokens": usageFieldState(usageObject, "input_tokens")}
	details, found := usageObject["input_tokens_details"]
	if !found {
		result["input_tokens_details"] = map[string]any{"state": "absent"}
		return result
	}
	if bytes.Equal(bytes.TrimSpace(details), []byte("null")) {
		result["input_tokens_details"] = map[string]any{"state": "null"}
		return result
	}
	var detailObject map[string]json.RawMessage
	if json.Unmarshal(details, &detailObject) != nil {
		result["input_tokens_details"] = map[string]any{"state": "invalid"}
		return result
	}
	result["cached_tokens"] = usageFieldState(detailObject, "cached_tokens")
	result["cache_write_tokens"] = usageFieldState(detailObject, "cache_write_tokens")
	return result
}

func usageFieldState(values map[string]json.RawMessage, key string) map[string]any {
	raw, found := values[key]
	if !found {
		return map[string]any{"state": "absent"}
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return map[string]any{"state": "null"}
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&number) == nil {
		if numeric, err := number.Float64(); err == nil {
			switch {
			case numeric == 0:
				return map[string]any{"state": "zero", "value": number}
			case numeric > 0:
				return map[string]any{"state": "positive", "value": number}
			default:
				return map[string]any{"state": "negative", "value": number}
			}
		}
		if number.String() == "0" || number.String() == "0.0" {
			return map[string]any{"state": "zero", "value": number}
		}
		return map[string]any{"state": "number", "value": number}
	}
	return map[string]any{"state": "non_number", "value": string(raw)}
}

type streamTrace struct {
	mu           sync.Mutex
	terminal     json.RawMessage
	terminalKind string
}

func (t *streamTrace) consume(reader io.Reader, maxEventBytes int64, onEvent func(sse.Event)) {
	parser := sse.NewParser(reader, maxEventBytes)
	for {
		event, err := parser.Next()
		if err != nil {
			if errors.Is(err, sse.ErrEventTooLarge) {
				// Parser.Next has consumed this event through its blank-line
				// boundary, so tracing can safely resume at the next event.
				continue
			}
			// Tracing is observational. If the side-channel parser cannot
			// understand an event, keep draining its pipe so a malformed event
			// never interrupts the byte-for-byte client stream.
			if !errors.Is(err, io.EOF) {
				_, _ = io.Copy(io.Discard, reader)
			}
			return
		}
		onEvent(event)
		kind := event.Event
		var envelope struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(event.Data, &envelope) == nil {
			if envelope.Type != "" {
				kind = envelope.Type
			}
			if isTerminalResponseEvent(kind) && len(envelope.Response) > 0 {
				t.mu.Lock()
				t.terminal = append(t.terminal[:0], envelope.Response...)
				t.terminalKind = kind
				t.mu.Unlock()
			}
		}
	}
}

func isTerminalResponseEvent(kind string) bool {
	switch kind {
	case "response.completed", "response.failed", "response.incomplete", "error":
		return true
	default:
		return false
	}
}

func (t *streamTrace) terminalResponse() (json.RawMessage, string) {
	if t == nil {
		return nil, ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append(json.RawMessage(nil), t.terminal...), t.terminalKind
}

// NewHTTPClient is shared by the command and uses transport settings suitable
// for many concurrent long-lived SSE responses.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 1024
	transport.MaxIdleConnsPerHost = 256
	transport.MaxConnsPerHost = 0
	transport.ForceAttemptHTTP2 = true
	return &http.Client{Transport: transport}
}

// ContextWithCancel exists to make cancellation behaviour easy to exercise in
// callers and tests without introducing a request-wide timeout.
func ContextWithCancel(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}
