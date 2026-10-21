package proxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-pass/internal/auth"
	debuglog "codex-pass/internal/debug"
	"codex-pass/internal/sse"
)

const (
	defaultMaxRequestBody = 32 << 20
	defaultMaxAggregate   = 64 << 20
	maxErrorBody          = 4 << 20
)

type Config struct {
	APIKey       string
	AuthPool     *auth.Pool
	HTTPClient   *http.Client
	UpstreamURL  string
	Logger       *debuglog.Logger
	MaxBodyBytes int64
	MaxAggregate int64
	UserAgent    string
}

type Server struct {
	apiKey       string
	authPool     *auth.Pool
	httpClient   *http.Client
	upstreamURL  string
	logger       *debuglog.Logger
	maxBodyBytes int64
	maxAggregate int64
	userAgent    string
	requestSeq   atomic.Uint64
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
		upstreamURL: config.UpstreamURL, logger: config.Logger, maxBodyBytes: config.MaxBodyBytes,
		maxAggregate: config.MaxAggregate, userAgent: config.UserAgent,
	}, nil
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
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
	case "/v1/responses":
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
	default:
		writeError(writer, http.StatusNotFound, "not found")
		return
	}
	if !s.authorized(request.Header.Get("Authorization")) {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeError(writer, http.StatusUnauthorized, "invalid API key")
		return
	}
	if s.logger != nil && s.logger.Err() != nil {
		writeError(writer, http.StatusInternalServerError, "debug logger is unavailable")
		return
	}
	if request.URL.Path == "/v1/chat/completions" {
		s.handleChatCompletions(writer, request)
		return
	}
	s.handleResponses(writer, request)
}

func (s *Server) authorized(header string) bool {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.apiKey)) == 1
}

func (s *Server) handleResponses(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	requestID := s.newRequestID()
	body, err := s.readBody(request.Body)
	if err != nil {
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": http.StatusRequestEntityTooLarge, "error": debuglog.Error(err)})
		writeError(writer, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	stream, upstreamBody, promptCacheKey, err := prepareBody(body)
	if err != nil {
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": http.StatusBadRequest, "error": debuglog.Error(err)})
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	sessionID := request.Header.Get("session-id")
	if sessionID == "" && promptCacheKey != "" {
		sessionID = promptCacheKey
	}
	accountIndex, credential := s.authPool.Select(promptCacheKey, request.Header.Get("session-id"))
	if s.logger != nil {
		diff, err := DiffJSON(body, upstreamBody)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "invalid JSON request: "+err.Error())
			return
		}
		s.log(requestID, "CLIENT_REQUEST", map[string]any{
			"method": request.Method, "url": request.URL.String(), "headers": debuglog.Headers(request.Header),
			"body": string(body), "stream": stream,
		})
		s.log(requestID, "AUTH_SELECTION", map[string]any{
			"auth_file_index": accountIndex, "auth_file_format": credential.Format(),
			"prompt_cache_key": promptCacheKey, "session_id": sessionID,
		})
		s.log(requestID, "REQUEST_DIFF", map[string]any{
			"method": http.MethodPost, "url": s.upstreamURL, "body": string(upstreamBody),
			"json_diff": diff, "prompt_cache_key": promptCacheKey, "session_id": sessionID,
		})
	}

	if err := s.authPool.EnsureFresh(request.Context(), credential); err != nil {
		s.log(requestID, "ERROR", map[string]any{"error": debuglog.Error(err), "operation": "proactive token refresh"})
		writeError(writer, http.StatusBadGateway, "OAuth token refresh failed: "+debuglog.Error(err))
		return
	}

	upstream, err := s.sendUpstream(request.Context(), request.Header, upstreamBody, credential, sessionID, requestID)
	if err != nil {
		s.log(requestID, "ERROR", map[string]any{"error": debuglog.Error(err), "operation": "upstream request"})
		writeError(writer, http.StatusBadGateway, "upstream request failed: "+debuglog.Error(err))
		return
	}
	if upstream.StatusCode == http.StatusUnauthorized {
		upstream = s.retryUnauthorized(request.Context(), request.Header, upstreamBody, credential, sessionID, upstream, requestID)
		if upstream == nil {
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
		s.copyUpstreamResponse(writer, upstream)
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": upstream.StatusCode, "mode": "upstream_error"})
		s.logSummary(requestID, started, time.Time{}, nil, accountIndex, promptCacheKey, sessionID)
		return
	}

	if stream {
		firstEvent, final, streamErr := s.forwardStream(writer, upstream.Body, upstream.Header, upstream.StatusCode, requestID)
		if streamErr != nil {
			s.log(requestID, "ERROR", map[string]any{"error": debuglog.Error(streamErr), "operation": "stream forward"})
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
		s.writeCollectError(writer, collectErr)
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": statusForCollectError(collectErr), "error": debuglog.Error(collectErr)})
		s.logSummary(requestID, started, firstEvent, nil, accountIndex, promptCacheKey, sessionID)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(final)
	s.log(requestID, "UPSTREAM_COMPLETED", map[string]any{"response": string(final)})
	s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": http.StatusOK, "mode": "aggregated", "body": string(final)})
	s.logSummary(requestID, started, firstEvent, final, accountIndex, promptCacheKey, sessionID)
}

func (s *Server) retryUnauthorized(ctx context.Context, clientHeaders http.Header, body []byte, credential *auth.Credential, sessionID string, first *http.Response, requestID string) *http.Response {
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
	second, err := s.sendUpstream(ctx, clientHeaders, body, credential, sessionID, requestID)
	if err != nil {
		s.log(requestID, "ERROR", map[string]any{"operation": "retry after OAuth refresh", "error": debuglog.Error(err)})
		return nil
	}
	return second
}

func (s *Server) sendUpstream(ctx context.Context, clientHeaders http.Header, body []byte, credential *auth.Credential, sessionID, requestID string) (*http.Response, error) {
	snapshot := credential.Snapshot()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header = cloneForwardHeaders(clientHeaders)
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
		s.log(requestID, "UPSTREAM_REQUEST", map[string]any{
			"method": request.Method, "url": request.URL.String(), "headers": debuglog.Headers(request.Header), "body": string(body),
		})
	}
	return s.httpClient.Do(request)
}

func (s *Server) forwardStream(writer http.ResponseWriter, body io.Reader, headers http.Header, status int, requestID string) (time.Time, json.RawMessage, error) {
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
	if s.logger != nil {
		trace = &streamTrace{}
		pipeReader, createdWriter := io.Pipe()
		pipeWriter = createdWriter
		wait.Add(1)
		go func() {
			defer wait.Done()
			trace.consume(pipeReader, func(event sse.Event) {
				s.log(requestID, "UPSTREAM_SSE", map[string]any{"event": event.Event, "id": event.ID, "data": string(event.Data)})
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
				return firstEvent, trace.finalResponse(), writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
			if pipeWriter != nil {
				if _, writeErr := pipeWriter.Write(chunk); writeErr != nil {
					wait.Wait()
					return firstEvent, trace.finalResponse(), writeErr
				}
			}
		}
		if readErr != nil {
			if pipeWriter != nil {
				_ = pipeWriter.CloseWithError(readErr)
				wait.Wait()
			}
			if errors.Is(readErr, io.EOF) {
				if trace != nil {
					return firstEvent, trace.finalResponse(), nil
				}
				return firstEvent, nil, nil
			}
			if trace != nil {
				return firstEvent, trace.finalResponse(), readErr
			}
			return firstEvent, nil, readErr
		}
	}
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

func prepareBody(body []byte) (stream bool, upstream []byte, promptCacheKey string, err error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("request body must be a JSON object")
		}
		return false, nil, "", fmt.Errorf("invalid request JSON: %w", err)
	}
	if rawCacheKey, found := object["prompt_cache_key"]; found {
		_ = json.Unmarshal(rawCacheKey, &promptCacheKey)
	}
	rawStream, found := object["stream"]
	if found {
		if err := json.Unmarshal(rawStream, &stream); err != nil {
			return false, nil, "", errors.New("top-level stream must be a JSON boolean")
		}
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
	mu        sync.Mutex
	completed json.RawMessage
}

func (t *streamTrace) consume(reader io.Reader, onEvent func(sse.Event)) {
	parser := sse.NewParser(reader, 0)
	for {
		event, err := parser.Next()
		if err != nil {
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
			if kind == "response.completed" && len(envelope.Response) > 0 {
				t.mu.Lock()
				t.completed = append(t.completed[:0], envelope.Response...)
				t.mu.Unlock()
			}
		}
	}
}

func (t *streamTrace) finalResponse() json.RawMessage {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append(json.RawMessage(nil), t.completed...)
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
