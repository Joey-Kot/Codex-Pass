package proxy

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

var errUnsupportedEncoding = errors.New("unsupported Content-Encoding")

// decodeCodexBody produces a bounded inspection copy. The original wire bytes
// and Content-Encoding are retained for forwarding, including on a 401 retry.
func (s *Server) decodeCodexBody(body []byte, encoding string) ([]byte, error) {
	var reader io.Reader
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body, nil
	case "zstd":
		decoder, err := zstd.NewReader(bytes.NewReader(body),
			zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(max(s.maxBodyBytes, 1<<20))))
		if err != nil {
			return nil, fmt.Errorf("decode zstd request: %w", err)
		}
		defer decoder.Close()
		reader = decoder
	case "gzip":
		decoder, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("decode gzip request: %w", err)
		}
		defer decoder.Close()
		reader = decoder
	default:
		return nil, errUnsupportedEncoding
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, s.maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("decode compressed request: %w", err)
	}
	if int64(len(decoded)) > s.maxBodyBytes {
		return nil, fmt.Errorf("decoded request body exceeds %d-byte limit", s.maxBodyBytes)
	}
	return decoded, nil
}

// Codex normally streams SSE. Preserve JSON responses too, without introducing
// the compatibility endpoint's SSE aggregation or changing the response body.
func (s *Server) forwardCodexJSON(writer http.ResponseWriter, upstream *http.Response, metrics *requestLogState, requestID string, started time.Time, accountIndex int, cacheKey, sessionID string) {
	body, err := io.ReadAll(io.LimitReader(upstream.Body, s.maxAggregate+1))
	if err != nil || int64(len(body)) > s.maxAggregate {
		metrics.failureForError("upstream_response_read", err)
		writeError(writer, http.StatusBadGateway, "could not read upstream JSON response within the response limit")
		return
	}
	for name, values := range responseHeaders(upstream.Header) {
		writer.Header()[name] = values
	}
	writer.WriteHeader(upstream.StatusCode)
	if _, err := writer.Write(body); err != nil {
		metrics.failureForError("client_response_write", err)
	} else {
		metrics.complete(body)
	}
	s.log(requestID, "UPSTREAM_COMPLETED", map[string]any{"response": string(body)})
	mode := strings.TrimPrefix(strings.TrimPrefix(metrics.request.URL.Path, "/"), "v1/")
	s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": upstream.StatusCode, "mode": mode, "body": string(body)})
	s.logSummary(requestID, started, time.Time{}, body, accountIndex, cacheKey, sessionID)
}
