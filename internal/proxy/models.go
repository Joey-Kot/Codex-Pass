package proxy

import (
	"net/http"
	"net/url"

	debuglog "codex-pass/internal/debug"
)

// handleModels forwards the complete Codex model catalog without converting it
// to the public OpenAI model-list format or caching account-specific metadata.
func (s *Server) handleModels(writer http.ResponseWriter, request *http.Request) {
	requestID := s.newRequestID()
	target, err := url.Parse(s.modelsUpstreamURL)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "invalid models upstream URL")
		return
	}
	target.RawQuery = request.URL.RawQuery
	sessionID := request.Header.Get("session-id")
	accountIndex, credential := s.authPool.Select("", sessionID)
	if s.logger != nil {
		s.log(requestID, "CLIENT_REQUEST", map[string]any{
			"method": request.Method, "url": request.URL.String(), "headers": debuglog.Headers(request.Header),
		})
		s.log(requestID, "AUTH_SELECTION", map[string]any{
			"auth_file_index": accountIndex, "auth_file_format": credential.Format(), "session_id": sessionID,
		})
	}
	if err := s.authPool.EnsureFresh(request.Context(), credential); err != nil {
		s.log(requestID, "ERROR", map[string]any{"error": debuglog.Error(err), "operation": "proactive token refresh"})
		writeError(writer, http.StatusBadGateway, "OAuth token refresh failed: "+debuglog.Error(err))
		return
	}
	send := func() (*http.Response, error) {
		return s.sendUpstreamRequest(request.Context(), http.MethodGet, target.String(), request.Header, nil, credential, sessionID, requestID)
	}
	upstream, err := send()
	if err != nil {
		s.log(requestID, "ERROR", map[string]any{"error": debuglog.Error(err), "operation": "models upstream request"})
		writeError(writer, http.StatusBadGateway, "upstream request failed: "+debuglog.Error(err))
		return
	}
	if upstream.StatusCode == http.StatusUnauthorized {
		upstream = s.retryUpstreamUnauthorized(request.Context(), credential, upstream, requestID, send)
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
	s.copyUpstreamResponse(writer, upstream)
	s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": upstream.StatusCode, "mode": "models"})
}
