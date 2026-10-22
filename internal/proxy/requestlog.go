package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-pass/internal/requestlog"
)

// requestLogState keeps one optional request-log row in step with one proxy
// request. It intentionally contains no request or response content beyond
// the final metadata needed for accounting.
type requestLogState struct {
	recorder requestlog.Recorder
	request  *http.Request

	completion requestlog.Completion
	once       sync.Once
	finishErr  error
}

func (s *Server) beginRequestLog(request *http.Request, requestID, endpoint string, started time.Time) (*requestLogState, error) {
	state := &requestLogState{
		recorder: s.requestLog,
		request:  request,
		completion: requestlog.Completion{
			RequestID: requestID,
			Outcome:   requestlog.OutcomeFailure,
			ErrorKind: "request_not_completed",
		},
	}
	if state.recorder == nil {
		return state, nil
	}
	if err := state.recorder.Begin(requestlog.Request{
		RequestID: requestID,
		StartedAt: started,
		Endpoint:  endpoint,
	}); err != nil {
		return nil, err
	}
	return state, nil
}

// setRequestMetadata adds fields that become available after the request body
// has been read. The row itself is already durable, so a process interruption
// during a slow upload remains visible as in-progress.
func (state *requestLogState) setRequestMetadata(body []byte) error {
	model, reasoningEffort := requestLogRequestMetadata(body)
	if model != "" {
		state.completion.Model = model
	}
	if reasoningEffort != "" {
		state.completion.ReasoningEffort = reasoningEffort
	}
	if state.recorder == nil || (model == "" && reasoningEffort == "") {
		return nil
	}
	return state.recorder.UpdateMetadata(requestlog.RequestMetadata{
		RequestID:       state.completion.RequestID,
		Model:           model,
		ReasoningEffort: reasoningEffort,
	})
}

func (state *requestLogState) complete(response json.RawMessage) {
	accounting, valid := responseAccounting(response)
	if !valid {
		state.failure("invalid_completed_response")
		return
	}
	if accounting.Model != "" {
		state.completion.Model = accounting.Model
	}
	state.completion.InputTokens = accounting.InputTokens
	state.completion.CacheWriteTokens = accounting.CacheWriteTokens
	state.completion.CacheReadTokens = accounting.CacheReadTokens
	state.completion.OutputTokens = accounting.OutputTokens
	state.completion.ReasoningTokens = accounting.ReasoningTokens
	switch strings.ToLower(accounting.Status) {
	case "", "completed":
		state.completion.Outcome = requestlog.OutcomeSuccess
		state.completion.ErrorKind = ""
	case "canceled", "cancelled":
		state.completion.Outcome = requestlog.OutcomeCanceled
		state.completion.ErrorKind = "upstream_canceled"
	default:
		state.completion.Outcome = requestlog.OutcomeFailure
		state.completion.ErrorKind = "upstream_non_completed"
	}
}

// terminal records a terminal Responses event that is not necessarily a
// response.completed event. Failed and incomplete terminal events can still
// carry useful usage accounting in their response object.
func (state *requestLogState) terminal(response json.RawMessage, kind string) {
	state.complete(response)
	switch kind {
	case "", "response.completed":
		return
	case "response.failed", "response.incomplete", "error":
		state.failure("upstream_terminal")
	default:
		state.failure("upstream_terminal")
	}
}

func (state *requestLogState) failure(kind string) {
	if state.completion.Outcome == requestlog.OutcomeCanceled {
		return
	}
	if state.request != nil && errors.Is(state.request.Context().Err(), context.Canceled) {
		state.completion.Outcome = requestlog.OutcomeCanceled
		state.completion.ErrorKind = "client_canceled"
		return
	}
	state.completion.Outcome = requestlog.OutcomeFailure
	state.completion.ErrorKind = safeErrorKind(kind)
}

func (state *requestLogState) failureForError(kind string, err error) {
	if state.completion.Outcome == requestlog.OutcomeCanceled {
		return
	}
	if errors.Is(err, context.Canceled) || (state.request != nil && errors.Is(state.request.Context().Err(), context.Canceled)) {
		state.completion.Outcome = requestlog.OutcomeCanceled
		state.completion.ErrorKind = "client_canceled"
		return
	}
	state.failure(kind)
}

func (state *requestLogState) finish() error {
	if state.recorder == nil {
		return nil
	}
	state.once.Do(func() {
		state.completion.FinishedAt = time.Now()
		state.finishErr = state.recorder.Finish(state.completion)
	})
	return state.finishErr
}

func requestLogRequestMetadata(body []byte) (model, reasoningEffort string) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return "", ""
	}
	if raw, found := object["model"]; found {
		_ = json.Unmarshal(raw, &model)
	}
	if raw, found := object["reasoning"]; found {
		var reasoning map[string]json.RawMessage
		if json.Unmarshal(raw, &reasoning) == nil {
			if effort, found := reasoning["effort"]; found {
				_ = json.Unmarshal(effort, &reasoningEffort)
			}
		}
	}
	if reasoningEffort == "" {
		if raw, found := object["reasoning_effort"]; found {
			_ = json.Unmarshal(raw, &reasoningEffort)
		}
	}
	return model, reasoningEffort
}

type responseAccountingFields struct {
	Model            string
	Status           string
	InputTokens      *int64
	CacheWriteTokens *int64
	CacheReadTokens  *int64
	OutputTokens     *int64
	ReasoningTokens  *int64
}

func responseAccounting(response json.RawMessage) (responseAccountingFields, bool) {
	var object map[string]json.RawMessage
	if len(bytes.TrimSpace(response)) == 0 || json.Unmarshal(response, &object) != nil || object == nil {
		return responseAccountingFields{}, false
	}
	var result responseAccountingFields
	if raw, found := object["model"]; found {
		_ = json.Unmarshal(raw, &result.Model)
	}
	if raw, found := object["status"]; found {
		_ = json.Unmarshal(raw, &result.Status)
	}
	usage, found := jsonObject(object["usage"])
	if !found {
		return result, true
	}
	result.InputTokens = tokenCount(usage["input_tokens"])
	result.OutputTokens = tokenCount(usage["output_tokens"])
	if inputDetails, found := jsonObject(usage["input_tokens_details"]); found {
		result.CacheReadTokens = tokenCount(inputDetails["cached_tokens"])
		result.CacheWriteTokens = tokenCount(inputDetails["cache_write_tokens"])
	}
	if outputDetails, found := jsonObject(usage["output_tokens_details"]); found {
		result.ReasoningTokens = tokenCount(outputDetails["reasoning_tokens"])
	}
	return result, true
}

func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, false
	}
	return object, true
}

func tokenCount(raw json.RawMessage) *int64 {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil
	}
	number, ok := value.(json.Number)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil || parsed < 0 {
		return nil
	}
	return &parsed
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func safeErrorKind(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "unknown"
	}
	var result strings.Builder
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			result.WriteRune(character)
		default:
			result.WriteByte('_')
		}
	}
	cleaned := strings.Trim(result.String(), "_")
	if cleaned == "" {
		return "unknown"
	}
	return cleaned
}
