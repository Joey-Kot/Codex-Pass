package proxy

import (
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	debuglog "codex-pass/internal/debug"
)

// Keep the upstream paths explicit: the Codex entry must not become an
// unrestricted forward proxy for arbitrary backend endpoints.
func codexToolPath(path string) (string, bool) {
	switch path {
	case "/v1/alpha/search", "/codex/alpha/search":
		return "alpha/search", true
	case "/v1/images/generations", "/codex/images/generations":
		return "images/generations", true
	case "/v1/images/edits", "/codex/images/edits":
		return "images/edits", true
	default:
		return "", false
	}
}

// Standalone search and Codex image generation/editing use JSON request and
// response bodies. Inspect copies for accounting; never rewrite their payloads.
func (s *Server) handleCodexTool(writer http.ResponseWriter, request *http.Request, path string) {
	started := time.Now()
	requestID := s.newRequestID()
	endpoint := strings.TrimPrefix(request.URL.Path, "/")
	endpoint = strings.TrimPrefix(endpoint, "v1/")
	metrics, err := s.beginRequestLog(request, requestID, endpoint, started)
	if err != nil {
		s.log(requestID, "ERROR", map[string]any{"operation": "start request log", "error": debuglog.Error(err)})
		writeError(writer, http.StatusInternalServerError, "request log is unavailable")
		return
	}
	defer func() {
		if err := metrics.finish(); err != nil {
			s.log(requestID, "ERROR", map[string]any{"operation": "finish request log", "error": debuglog.Error(err)})
			log.Printf("request usage log error for %s: %v", requestID, err)
		}
	}()
	if contentType := request.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/json" {
			metrics.failure("unsupported_content_type")
			writeError(writer, http.StatusUnsupportedMediaType, "Codex tools require application/json request bodies")
			return
		}
	}
	body, err := s.readBody(request.Body)
	if err != nil {
		metrics.failureForError("request_body_read", err)
		writeError(writer, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	inspection, err := s.decodeCodexBody(body, request.Header.Get("Content-Encoding"))
	if err != nil {
		metrics.failure("invalid_request_encoding")
		status := http.StatusBadRequest
		if errors.Is(err, errUnsupportedEncoding) {
			status = http.StatusUnsupportedMediaType
		}
		writeError(writer, status, err.Error())
		return
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(inspection, &object); err != nil || object == nil {
		metrics.failure("invalid_request")
		writeError(writer, http.StatusBadRequest, "request body must be a JSON object")
		return
	}
	if err := metrics.setRequestMetadata(inspection); err != nil {
		metrics.failure("request_log_metadata")
		writeError(writer, http.StatusInternalServerError, "request log is unavailable")
		return
	}
	var cacheKey string
	_ = json.Unmarshal(object["prompt_cache_key"], &cacheKey)
	sessionID := request.Header.Get("session-id")
	if sessionID == "" {
		sessionID = cacheKey
	}
	accountIndex, credential := s.authPool.Select(cacheKey, request.Header.Get("session-id"))
	target, err := url.Parse(strings.TrimRight(s.codexUpstreamURL, "/") + "/" + path)
	if err != nil {
		metrics.failure("invalid_upstream_url")
		writeError(writer, http.StatusBadGateway, "invalid Codex upstream URL")
		return
	}
	target.RawQuery = request.URL.RawQuery
	s.log(requestID, "CLIENT_REQUEST", map[string]any{
		"method": request.Method, "url": request.URL.String(), "headers": debuglog.Headers(request.Header),
		"body": string(inspection), "body_encoding": request.Header.Get("Content-Encoding"), "body_bytes": len(body), "mode": endpoint,
	})
	s.log(requestID, "AUTH_SELECTION", map[string]any{
		"auth_file_index": accountIndex, "auth_file_format": credential.Format(), "prompt_cache_key": cacheKey, "session_id": sessionID,
	})
	s.log(requestID, "REQUEST_DIFF", map[string]any{
		"method": http.MethodPost, "url": target.String(), "body": string(inspection),
		"json_diff":        JSONDiff{ChangedFields: []FieldChange{}, RemovedFields: []FieldChange{}, AddedFields: []FieldChange{}},
		"prompt_cache_key": cacheKey, "session_id": sessionID,
	})
	if err := s.authPool.EnsureFresh(request.Context(), credential); err != nil {
		metrics.failureForError("oauth_token_refresh", err)
		s.log(requestID, "ERROR", map[string]any{"operation": "proactive token refresh", "error": debuglog.Error(err)})
		writeError(writer, http.StatusBadGateway, "OAuth token refresh failed: "+debuglog.Error(err))
		return
	}
	headers := request.Header.Clone()
	headers.Set("Accept-Encoding", "identity")
	send := func() (*http.Response, error) {
		return s.sendUpstreamAccept(request.Context(), http.MethodPost, target.String(), headers, body, credential, sessionID, requestID, "application/json")
	}
	upstream, err := send()
	if err != nil {
		metrics.failureForError("upstream_request", err)
		s.log(requestID, "ERROR", map[string]any{"operation": "Codex tool upstream request", "error": debuglog.Error(err)})
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
	s.log(requestID, "UPSTREAM_RESPONSE_HEADERS", map[string]any{"status": upstream.StatusCode, "headers": debuglog.Headers(upstream.Header)})
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		metrics.failure("upstream_http_status")
		s.copyUpstreamResponse(writer, upstream)
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": upstream.StatusCode, "mode": endpoint})
		return
	}
	s.forwardCodexJSON(writer, upstream, metrics, requestID, started, accountIndex, cacheKey, sessionID)
}
