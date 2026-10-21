package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	debuglog "codex-pass/internal/debug"
	"codex-pass/internal/sse"
)

// chatRequest retains the few client-facing values which cannot be recovered
// from the Responses request sent to the Codex upstream.
type chatRequest struct {
	stream       bool
	includeUsage bool
	model        string
	upstream     []byte
	promptCache  string
}

// handleChatCompletions adapts the public Chat Completions surface to the
// Responses-only Codex upstream. Upstream requests always stream: a
// non-streaming client request is aggregated before it is converted back to a
// Chat Completion object.
func (s *Server) handleChatCompletions(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	requestID := s.newRequestID()
	body, err := s.readBody(request.Body)
	if err != nil {
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": http.StatusRequestEntityTooLarge, "error": debuglog.Error(err)})
		writeError(writer, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	chat, err := prepareChatCompletionBody(body)
	if err != nil {
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": http.StatusBadRequest, "error": debuglog.Error(err)})
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	_, upstreamBody, promptCacheKey, err := prepareBody(chat.upstream)
	if err != nil {
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": http.StatusBadRequest, "error": debuglog.Error(err)})
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if promptCacheKey == "" {
		promptCacheKey = chat.promptCache
	}
	sessionID := request.Header.Get("session-id")
	if sessionID == "" && promptCacheKey != "" {
		sessionID = promptCacheKey
	}
	accountIndex, credential := s.authPool.Select(promptCacheKey, request.Header.Get("session-id"))
	if s.logger != nil {
		diff, diffErr := DiffJSON(body, upstreamBody)
		if diffErr != nil {
			writeError(writer, http.StatusBadRequest, "invalid JSON request: "+diffErr.Error())
			return
		}
		s.log(requestID, "CLIENT_REQUEST", map[string]any{
			"method": request.Method, "url": request.URL.String(), "headers": debuglog.Headers(request.Header),
			"body": string(body), "stream": chat.stream, "api": "chat_completions",
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
	// Chat completions must parse the upstream SSE body before responding. Do
	// not let a client-negotiated content encoding turn that body into a gzip
	// byte stream which the SSE parser cannot read.
	upstreamHeaders := request.Header.Clone()
	upstreamHeaders.Set("Accept-Encoding", "identity")
	upstream, err := s.sendUpstream(request.Context(), upstreamHeaders, upstreamBody, credential, sessionID, requestID)
	if err != nil {
		s.log(requestID, "ERROR", map[string]any{"error": debuglog.Error(err), "operation": "upstream request"})
		writeError(writer, http.StatusBadGateway, "upstream request failed: "+debuglog.Error(err))
		return
	}
	if upstream.StatusCode == http.StatusUnauthorized {
		upstream = s.retryUnauthorized(request.Context(), upstreamHeaders, upstreamBody, credential, sessionID, upstream, requestID)
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
	if upstream.StatusCode < http.StatusOK || upstream.StatusCode >= http.StatusMultipleChoices {
		s.copyUpstreamResponse(writer, upstream)
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": upstream.StatusCode, "mode": "upstream_error", "api": "chat_completions"})
		s.logSummary(requestID, started, time.Time{}, nil, accountIndex, promptCacheKey, sessionID)
		return
	}

	if chat.stream {
		firstEvent, final, streamErr := s.forwardChatCompletionStream(writer, upstream.Body, upstream.Header, chat, requestID)
		if streamErr != nil {
			s.log(requestID, "ERROR", map[string]any{"error": debuglog.Error(streamErr), "operation": "chat completion stream conversion"})
		}
		if len(final) != 0 {
			s.log(requestID, "UPSTREAM_COMPLETED", map[string]any{"response": string(final)})
		}
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": http.StatusOK, "mode": "chat_stream"})
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
		s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": statusForCollectError(collectErr), "error": debuglog.Error(collectErr), "api": "chat_completions"})
		s.logSummary(requestID, started, firstEvent, nil, accountIndex, promptCacheKey, sessionID)
		return
	}
	converted, err := responseToChatCompletion(final, chat.model)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "upstream response conversion failed: "+debuglog.Error(err))
		s.log(requestID, "ERROR", map[string]any{"error": debuglog.Error(err), "operation": "chat completion conversion"})
		s.logSummary(requestID, started, firstEvent, final, accountIndex, promptCacheKey, sessionID)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(converted)
	s.log(requestID, "UPSTREAM_COMPLETED", map[string]any{"response": string(final)})
	s.log(requestID, "CLIENT_RESPONSE", map[string]any{"status": http.StatusOK, "mode": "chat_aggregated", "body": string(converted)})
	s.logSummary(requestID, started, firstEvent, final, accountIndex, promptCacheKey, sessionID)
}

// prepareChatCompletionBody converts the portable Chat Completions request
// fields that have a direct Responses API equivalent. Fields without a safe
// equivalent are intentionally not copied to the upstream request.
func prepareChatCompletionBody(body []byte) (chatRequest, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("request body must be a JSON object")
		}
		return chatRequest{}, fmt.Errorf("invalid chat completion request JSON: %w", err)
	}

	var result chatRequest
	if rawStream, found := object["stream"]; found {
		if err := json.Unmarshal(rawStream, &result.stream); err != nil {
			return chatRequest{}, errors.New("top-level stream must be a JSON boolean")
		}
	}
	if rawModel, found := object["model"]; found {
		_ = json.Unmarshal(rawModel, &result.model)
	}
	if rawCache, found := object["prompt_cache_key"]; found {
		_ = json.Unmarshal(rawCache, &result.promptCache)
	}
	if rawOptions, found := object["stream_options"]; found {
		var options map[string]json.RawMessage
		if err := json.Unmarshal(rawOptions, &options); err != nil || options == nil {
			return chatRequest{}, errors.New("stream_options must be a JSON object")
		}
		if rawUsage, found := options["include_usage"]; found {
			if err := json.Unmarshal(rawUsage, &result.includeUsage); err != nil {
				return chatRequest{}, errors.New("stream_options.include_usage must be a JSON boolean")
			}
		}
	}
	if rawN, found := object["n"]; found {
		var n int
		if err := json.Unmarshal(rawN, &n); err != nil || n != 1 {
			return chatRequest{}, errors.New("only n: 1 is supported by the Codex Responses upstream")
		}
	}

	rawMessages, found := object["messages"]
	if !found {
		return chatRequest{}, errors.New("chat completion request requires messages")
	}
	input, err := chatMessagesToInput(rawMessages)
	if err != nil {
		return chatRequest{}, err
	}
	upstream := map[string]json.RawMessage{
		"input":  input,
		"stream": json.RawMessage("true"),
	}
	for _, key := range []string{
		"model", "tools", "tool_choice", "parallel_tool_calls", "service_tier",
		"metadata", "prompt_cache_key", "store", "reasoning", "include",
		"background", "safety_identifier", "top_p",
	} {
		if value, found := object[key]; found {
			upstream[key] = append(json.RawMessage(nil), value...)
		}
	}
	if value, found := object["max_completion_tokens"]; found {
		upstream["max_output_tokens"] = append(json.RawMessage(nil), value...)
	} else if value, found := object["max_tokens"]; found {
		upstream["max_output_tokens"] = append(json.RawMessage(nil), value...)
	}
	// prepareBody applies the proxy's existing, explicit compatibility rules to
	// this value before it reaches the Codex upstream.
	if value, found := object["temperature"]; found {
		upstream["temperature"] = append(json.RawMessage(nil), value...)
	}
	encoded, err := json.Marshal(upstream)
	if err != nil {
		return chatRequest{}, fmt.Errorf("encode Responses request: %w", err)
	}
	result.upstream = encoded
	return result, nil
}

func chatMessagesToInput(rawMessages json.RawMessage) (json.RawMessage, error) {
	var messages []json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil || messages == nil {
		return nil, errors.New("messages must be a JSON array")
	}
	input := make([]json.RawMessage, 0, len(messages))
	for index, rawMessage := range messages {
		var message map[string]json.RawMessage
		if err := json.Unmarshal(rawMessage, &message); err != nil || message == nil {
			return nil, fmt.Errorf("messages[%d] must be a JSON object", index)
		}
		var role string
		if rawRole, found := message["role"]; !found || json.Unmarshal(rawRole, &role) != nil {
			return nil, fmt.Errorf("messages[%d].role must be a string", index)
		}
		switch role {
		case "system", "developer", "user", "assistant":
			var toolCalls []json.RawMessage
			if role == "assistant" {
				if rawCalls, found := message["tool_calls"]; found {
					calls, err := chatToolCallsToInput(rawCalls, index)
					if err != nil {
						return nil, err
					}
					toolCalls = calls
				}
			}
			rawContent, hasContent := message["content"]
			if !hasContent || bytes.Equal(bytes.TrimSpace(rawContent), []byte("null")) {
				if role == "assistant" && len(toolCalls) != 0 {
					input = append(input, toolCalls...)
					continue
				}
				return nil, fmt.Errorf("messages[%d].content is required", index)
			}
			content, err := chatContentToResponseParts(rawContent, role == "assistant")
			if err != nil {
				return nil, fmt.Errorf("messages[%d].content: %w", index, err)
			}
			responseRole := role
			if responseRole == "system" {
				responseRole = "developer"
			}
			item, err := json.Marshal(map[string]json.RawMessage{
				"type":    json.RawMessage(`"message"`),
				"role":    json.RawMessage(strconv.Quote(responseRole)),
				"content": content,
			})
			if err != nil {
				return nil, fmt.Errorf("encode messages[%d]: %w", index, err)
			}
			input = append(input, item)
			input = append(input, toolCalls...)
		case "tool":
			rawCallID, found := message["tool_call_id"]
			if !found {
				return nil, fmt.Errorf("messages[%d].tool_call_id is required for tool messages", index)
			}
			var callID string
			if json.Unmarshal(rawCallID, &callID) != nil || callID == "" {
				return nil, fmt.Errorf("messages[%d].tool_call_id must be a non-empty string", index)
			}
			rawContent, found := message["content"]
			if !found {
				return nil, fmt.Errorf("messages[%d].content is required", index)
			}
			output, err := chatToolOutput(rawContent)
			if err != nil {
				return nil, fmt.Errorf("messages[%d].content: %w", index, err)
			}
			item, err := json.Marshal(map[string]string{"type": "function_call_output", "call_id": callID, "output": output})
			if err != nil {
				return nil, fmt.Errorf("encode messages[%d]: %w", index, err)
			}
			input = append(input, item)
		default:
			return nil, fmt.Errorf("messages[%d].role %q is not supported", index, role)
		}
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode input: %w", err)
	}
	return encoded, nil
}

func chatContentToResponseParts(raw json.RawMessage, assistant bool) (json.RawMessage, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		partType := "input_text"
		if assistant {
			partType = "output_text"
		}
		return json.Marshal([]map[string]string{{"type": partType, "text": text}})
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil || parts == nil {
		return nil, errors.New("must be a string or a JSON array")
	}
	converted := make([]json.RawMessage, 0, len(parts))
	for index, rawPart := range parts {
		var part map[string]json.RawMessage
		if err := json.Unmarshal(rawPart, &part); err != nil || part == nil {
			return nil, fmt.Errorf("parts[%d] must be a JSON object", index)
		}
		var kind string
		if rawType, found := part["type"]; !found || json.Unmarshal(rawType, &kind) != nil {
			return nil, fmt.Errorf("parts[%d].type must be a string", index)
		}
		switch kind {
		case "text":
			var value string
			if rawText, found := part["text"]; !found || json.Unmarshal(rawText, &value) != nil {
				return nil, fmt.Errorf("parts[%d].text must be a string", index)
			}
			partType := "input_text"
			if assistant {
				partType = "output_text"
			}
			encoded, _ := json.Marshal(map[string]string{"type": partType, "text": value})
			converted = append(converted, encoded)
		case "image_url":
			if assistant {
				return nil, fmt.Errorf("parts[%d] image_url is not valid for an assistant message", index)
			}
			image, found := part["image_url"]
			if !found {
				return nil, fmt.Errorf("parts[%d].image_url is required", index)
			}
			url, detail, err := chatImageURL(image)
			if err != nil {
				return nil, fmt.Errorf("parts[%d].image_url: %w", index, err)
			}
			item := map[string]string{"type": "input_image", "image_url": url}
			if detail != "" {
				item["detail"] = detail
			}
			encoded, _ := json.Marshal(item)
			converted = append(converted, encoded)
		case "input_audio":
			if assistant {
				return nil, fmt.Errorf("parts[%d] input_audio is not valid for an assistant message", index)
			}
			audio, found := part["input_audio"]
			if !found {
				return nil, fmt.Errorf("parts[%d].input_audio is required", index)
			}
			encoded, _ := json.Marshal(map[string]json.RawMessage{"type": json.RawMessage(`"input_audio"`), "input_audio": audio})
			converted = append(converted, encoded)
		case "refusal":
			if !assistant {
				return nil, fmt.Errorf("parts[%d] refusal is only valid for an assistant message", index)
			}
			var refusal string
			if rawRefusal, found := part["refusal"]; !found || json.Unmarshal(rawRefusal, &refusal) != nil {
				return nil, fmt.Errorf("parts[%d].refusal must be a string", index)
			}
			encoded, _ := json.Marshal(map[string]string{"type": "refusal", "refusal": refusal})
			converted = append(converted, encoded)
		default:
			return nil, fmt.Errorf("parts[%d].type %q is not supported", index, kind)
		}
	}
	return json.Marshal(converted)
}

func chatImageURL(raw json.RawMessage) (string, string, error) {
	var url string
	if json.Unmarshal(raw, &url) == nil {
		return url, "", nil
	}
	var image map[string]json.RawMessage
	if err := json.Unmarshal(raw, &image); err != nil || image == nil {
		return "", "", errors.New("must be a string or a JSON object")
	}
	if rawURL, found := image["url"]; !found || json.Unmarshal(rawURL, &url) != nil || url == "" {
		return "", "", errors.New("url must be a non-empty string")
	}
	var detail string
	if rawDetail, found := image["detail"]; found && json.Unmarshal(rawDetail, &detail) != nil {
		return "", "", errors.New("detail must be a string")
	}
	return url, detail, nil
}

func chatToolCallsToInput(raw json.RawMessage, messageIndex int) ([]json.RawMessage, error) {
	var calls []json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil || calls == nil {
		return nil, fmt.Errorf("messages[%d].tool_calls must be a JSON array", messageIndex)
	}
	result := make([]json.RawMessage, 0, len(calls))
	for index, rawCall := range calls {
		var call struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if err := json.Unmarshal(rawCall, &call); err != nil || call.ID == "" || call.Type != "function" || call.Function.Name == "" {
			return nil, fmt.Errorf("messages[%d].tool_calls[%d] must be a function call with id and name", messageIndex, index)
		}
		encoded, err := json.Marshal(map[string]string{
			"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments,
		})
		if err != nil {
			return nil, err
		}
		result = append(result, encoded)
	}
	return result, nil
}

func chatToolOutput(raw json.RawMessage) (string, error) {
	var output string
	if json.Unmarshal(raw, &output) == nil {
		return output, nil
	}
	parts, err := chatContentToResponseParts(raw, false)
	if err != nil {
		return "", err
	}
	var decoded []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(parts, &decoded); err != nil {
		return "", err
	}
	var builder strings.Builder
	for _, part := range decoded {
		builder.WriteString(part.Text)
	}
	return builder.String(), nil
}

func responseToChatCompletion(response json.RawMessage, fallbackModel string) ([]byte, error) {
	metadata, message, finishReason, usage, err := chatResponseParts(response, fallbackModel)
	if err != nil {
		return nil, err
	}
	choice := map[string]any{
		"index":         0,
		"message":       message,
		"logprobs":      nil,
		"finish_reason": finishReason,
	}
	result := map[string]any{
		"id":      metadata.id,
		"object":  "chat.completion",
		"created": metadata.created,
		"model":   metadata.model,
		"choices": []any{choice},
	}
	if usage != nil {
		result["usage"] = usage
	}
	return json.Marshal(result)
}

type chatResponseMetadata struct {
	id      string
	created int64
	model   string
}

func chatResponseParts(response json.RawMessage, fallbackModel string) (chatResponseMetadata, map[string]any, string, map[string]any, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(response, &object); err != nil {
		return chatResponseMetadata{}, nil, "", nil, fmt.Errorf("decode Responses response: %w", err)
	}
	if object == nil {
		return chatResponseMetadata{}, nil, "", nil, errors.New("Responses response must be a JSON object")
	}
	metadata := responseMetadata(object, fallbackModel)
	message, hasToolCalls, err := responseOutputToChatMessage(object["output"])
	if err != nil {
		return chatResponseMetadata{}, nil, "", nil, err
	}
	finishReason := "stop"
	if hasToolCalls {
		finishReason = "tool_calls"
	}
	usage, err := responseUsageToChatUsage(object["usage"])
	if err != nil {
		return chatResponseMetadata{}, nil, "", nil, err
	}
	return metadata, message, finishReason, usage, nil
}

func responseMetadata(object map[string]json.RawMessage, fallbackModel string) chatResponseMetadata {
	metadata := chatResponseMetadata{created: time.Now().Unix(), model: fallbackModel}
	_ = json.Unmarshal(object["id"], &metadata.id)
	_ = json.Unmarshal(object["model"], &metadata.model)
	var created json.Number
	decoder := json.NewDecoder(bytes.NewReader(object["created_at"]))
	decoder.UseNumber()
	if decoder.Decode(&created) == nil {
		if value, err := created.Int64(); err == nil {
			metadata.created = value
		}
	}
	return metadata
}

func responseOutputToChatMessage(rawOutput json.RawMessage) (map[string]any, bool, error) {
	message := map[string]any{"role": "assistant"}
	if len(bytes.TrimSpace(rawOutput)) == 0 || bytes.Equal(bytes.TrimSpace(rawOutput), []byte("null")) {
		message["content"] = ""
		return message, false, nil
	}
	var output []json.RawMessage
	if err := json.Unmarshal(rawOutput, &output); err != nil {
		return nil, false, fmt.Errorf("decode Responses output: %w", err)
	}
	var content strings.Builder
	var refusal strings.Builder
	toolCalls := make([]map[string]any, 0)
	for index, rawItem := range output {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(rawItem, &item); err != nil || item == nil {
			return nil, false, fmt.Errorf("decode Responses output[%d]", index)
		}
		var kind string
		_ = json.Unmarshal(item["type"], &kind)
		switch kind {
		case "message":
			var parts []json.RawMessage
			if err := json.Unmarshal(item["content"], &parts); err != nil {
				return nil, false, fmt.Errorf("decode Responses output[%d].content: %w", index, err)
			}
			for partIndex, rawPart := range parts {
				var part map[string]json.RawMessage
				if err := json.Unmarshal(rawPart, &part); err != nil || part == nil {
					return nil, false, fmt.Errorf("decode Responses output[%d].content[%d]", index, partIndex)
				}
				var partType string
				_ = json.Unmarshal(part["type"], &partType)
				switch partType {
				case "output_text", "text":
					var text string
					if json.Unmarshal(part["text"], &text) == nil {
						content.WriteString(text)
					}
				case "refusal":
					var text string
					if json.Unmarshal(part["refusal"], &text) == nil {
						refusal.WriteString(text)
					}
				}
			}
		case "function_call":
			var callID, name, arguments string
			_ = json.Unmarshal(item["call_id"], &callID)
			_ = json.Unmarshal(item["name"], &name)
			_ = json.Unmarshal(item["arguments"], &arguments)
			if callID == "" {
				_ = json.Unmarshal(item["id"], &callID)
			}
			if name == "" {
				return nil, false, fmt.Errorf("Responses function_call output[%d] has no name", index)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": callID, "type": "function", "function": map[string]string{"name": name, "arguments": arguments},
			})
		}
	}
	if len(toolCalls) > 0 {
		if content.Len() == 0 {
			message["content"] = nil
		} else {
			message["content"] = content.String()
		}
		message["tool_calls"] = toolCalls
	} else {
		message["content"] = content.String()
	}
	if refusal.Len() > 0 {
		message["refusal"] = refusal.String()
	}
	return message, len(toolCalls) > 0, nil
}

func responseUsageToChatUsage(rawUsage json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(rawUsage)) == 0 || bytes.Equal(bytes.TrimSpace(rawUsage), []byte("null")) {
		return nil, nil
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(rawUsage, &usage); err != nil || usage == nil {
		return nil, fmt.Errorf("decode Responses usage: %w", err)
	}
	result := make(map[string]any, 3)
	for source, destination := range map[string]string{
		"input_tokens": "prompt_tokens", "output_tokens": "completion_tokens", "total_tokens": "total_tokens",
	} {
		if rawValue, found := usage[source]; found {
			var value any
			if err := json.Unmarshal(rawValue, &value); err != nil {
				return nil, fmt.Errorf("decode Responses usage.%s: %w", source, err)
			}
			result[destination] = value
		}
	}
	if _, found := result["total_tokens"]; !found {
		prompt, promptOK := numericUsageValue(result["prompt_tokens"])
		completion, completionOK := numericUsageValue(result["completion_tokens"])
		if promptOK && completionOK {
			result["total_tokens"] = prompt + completion
		}
	}
	return result, nil
}

func numericUsageValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		result, err := typed.Float64()
		return result, err == nil
	default:
		return 0, false
	}
}

type chatStreamConverter struct {
	metadata       chatResponseMetadata
	includeUsage   bool
	roleSent       bool
	toolCalls      map[string]chatStreamToolCall
	hasToolCalls   bool
	completed      bool
	contentWasSent bool
}

type chatStreamToolCall struct {
	index             int
	callID            string
	argumentsWereSent bool
}

func newChatStreamConverter(model string, includeUsage bool) *chatStreamConverter {
	return &chatStreamConverter{
		metadata:     chatResponseMetadata{created: time.Now().Unix(), model: model},
		includeUsage: includeUsage,
		toolCalls:    make(map[string]chatStreamToolCall),
	}
}

func (s *Server) forwardChatCompletionStream(writer http.ResponseWriter, body io.Reader, headers http.Header, request chatRequest, requestID string) (time.Time, json.RawMessage, error) {
	for key, values := range responseHeaders(headers) {
		if strings.EqualFold(key, "Content-Type") {
			continue
		}
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.WriteHeader(http.StatusOK)
	flusher, _ := writer.(http.Flusher)
	write := func(value any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := writer.Write([]byte("data: ")); err != nil {
			return err
		}
		if _, err := writer.Write(encoded); err != nil {
			return err
		}
		if _, err := writer.Write([]byte("\n\n")); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	writeDone := func() error {
		if _, err := writer.Write([]byte("data: [DONE]\n\n")); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	converter := newChatStreamConverter(request.model, request.includeUsage)
	// Unlike the transparent Responses route, this path must parse each event
	// before emitting its Chat Completion equivalent. Apply the configured
	// aggregation limit as a per-event ceiling to bound parser memory.
	parser := sse.NewParser(body, s.maxAggregate)
	firstEvent := time.Time{}
	var final json.RawMessage
	for {
		event, err := parser.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !converter.completed {
					_ = write(map[string]any{"error": map[string]string{"message": "upstream SSE stream ended before response.completed", "type": "upstream_sse_error"}})
					return firstEvent, final, sse.ErrIncomplete
				}
				return firstEvent, final, nil
			}
			_ = write(map[string]any{"error": map[string]string{"message": "upstream SSE conversion failed: " + err.Error(), "type": "upstream_sse_error"}})
			return firstEvent, final, err
		}
		if firstEvent.IsZero() {
			firstEvent = time.Now()
		}
		if s.logger != nil {
			s.log(requestID, "UPSTREAM_SSE", map[string]any{"event": event.Event, "id": event.ID, "data": string(event.Data)})
		}
		chunks, completedResponse, terminal, convertErr := converter.consume(event)
		if convertErr != nil {
			_ = write(map[string]any{"error": map[string]string{"message": "upstream SSE conversion failed: " + convertErr.Error(), "type": "upstream_sse_error"}})
			return firstEvent, final, convertErr
		}
		for _, chunk := range chunks {
			if err := write(chunk); err != nil {
				return firstEvent, final, err
			}
		}
		if len(completedResponse) != 0 {
			final = completedResponse
		}
		if terminal {
			if converter.completed {
				if err := writeDone(); err != nil {
					return firstEvent, final, err
				}
				return firstEvent, final, nil
			}
			_ = write(map[string]any{"error": map[string]string{"message": "upstream Responses request did not complete", "type": "upstream_sse_error"}})
			return firstEvent, final, errors.New("upstream Responses request did not complete")
		}
	}
}

func (c *chatStreamConverter) consume(event sse.Event) ([]map[string]any, json.RawMessage, bool, error) {
	if bytes.Equal(bytes.TrimSpace(event.Data), []byte("[DONE]")) {
		return nil, nil, true, nil
	}
	if len(bytes.TrimSpace(event.Data)) == 0 {
		return nil, nil, false, nil
	}
	var envelope struct {
		Type     string          `json:"type"`
		Response json.RawMessage `json:"response"`
		Item     json.RawMessage `json:"item"`
		ItemID   string          `json:"item_id"`
		Delta    json.RawMessage `json:"delta"`
	}
	if err := json.Unmarshal(event.Data, &envelope); err != nil {
		return nil, nil, false, fmt.Errorf("decode SSE event %q: %w", event.Event, err)
	}
	kind := envelope.Type
	if kind == "" {
		kind = event.Event
	}
	chunks := make([]map[string]any, 0, 2)
	switch kind {
	case "response.created", "response.in_progress":
		if len(envelope.Response) != 0 {
			var response map[string]json.RawMessage
			if json.Unmarshal(envelope.Response, &response) == nil {
				c.metadata = responseMetadata(response, c.metadata.model)
			}
		}
	case "response.output_item.added":
		chunk, err := c.addToolCall(envelope.Item)
		if err != nil {
			return nil, nil, false, err
		}
		if chunk != nil {
			chunks = c.appendRole(chunks)
			chunks = append(chunks, chunk)
		}
	case "response.output_item.done":
		chunk, err := c.finishToolCall(envelope.Item)
		if err != nil {
			return nil, nil, false, err
		}
		if chunk != nil {
			chunks = c.appendRole(chunks)
			chunks = append(chunks, chunk)
		}
	case "response.output_text.delta":
		var delta string
		if err := json.Unmarshal(envelope.Delta, &delta); err != nil {
			return nil, nil, false, fmt.Errorf("decode output text delta: %w", err)
		}
		chunks = c.appendRole(chunks)
		chunks = append(chunks, c.chunk(map[string]any{"content": delta}, nil))
		c.contentWasSent = true
	case "response.refusal.delta":
		var delta string
		if err := json.Unmarshal(envelope.Delta, &delta); err != nil {
			return nil, nil, false, fmt.Errorf("decode refusal delta: %w", err)
		}
		chunks = c.appendRole(chunks)
		chunks = append(chunks, c.chunk(map[string]any{"refusal": delta}, nil))
	case "response.function_call_arguments.delta":
		chunk, err := c.addFunctionArguments(envelope.Item, envelope.ItemID, envelope.Delta)
		if err != nil {
			return nil, nil, false, err
		}
		if chunk != nil {
			chunks = c.appendRole(chunks)
			chunks = append(chunks, chunk)
		}
	case "response.completed":
		if len(envelope.Response) == 0 {
			return nil, nil, false, errors.New("response.completed has no response")
		}
		metadata, message, finish, usage, err := chatResponseParts(envelope.Response, c.metadata.model)
		if err != nil {
			return nil, nil, false, err
		}
		c.metadata = metadata
		if c.hasToolCalls {
			finish = "tool_calls"
		}
		chunks = c.appendRole(chunks)
		if !c.contentWasSent {
			if content, found := message["content"].(string); found && content != "" {
				chunks = append(chunks, c.chunk(map[string]any{"content": content}, nil))
			}
		}
		chunks = append(chunks, c.chunk(map[string]any{}, &finish))
		if c.includeUsage && usage != nil {
			chunks = append(chunks, c.usageChunk(usage))
		}
		c.completed = true
		return chunks, append(json.RawMessage(nil), envelope.Response...), true, nil
	case "response.failed", "response.incomplete", "error":
		return nil, nil, true, nil
	}
	return chunks, nil, false, nil
}

func (c *chatStreamConverter) roleChunk() map[string]any {
	if c.roleSent {
		return nil
	}
	c.roleSent = true
	return c.chunk(map[string]any{"role": "assistant"}, nil)
}

func (c *chatStreamConverter) appendRole(chunks []map[string]any) []map[string]any {
	if chunk := c.roleChunk(); chunk != nil {
		return append(chunks, chunk)
	}
	return chunks
}

func (c *chatStreamConverter) chunk(delta map[string]any, finishReason *string) map[string]any {
	choice := map[string]any{"index": 0, "delta": delta, "logprobs": nil, "finish_reason": nil}
	if finishReason != nil {
		choice["finish_reason"] = *finishReason
	}
	return map[string]any{
		"id": c.metadata.id, "object": "chat.completion.chunk", "created": c.metadata.created,
		"model": c.metadata.model, "choices": []any{choice},
	}
}

func (c *chatStreamConverter) usageChunk(usage map[string]any) map[string]any {
	return map[string]any{
		"id": c.metadata.id, "object": "chat.completion.chunk", "created": c.metadata.created,
		"model": c.metadata.model, "choices": []any{}, "usage": usage,
	}
}

func (c *chatStreamConverter) addToolCall(rawItem json.RawMessage) (map[string]any, error) {
	if len(rawItem) == 0 {
		return nil, nil
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(rawItem, &item); err != nil || item == nil {
		return nil, errors.New("decode response.output_item.added item")
	}
	var kind string
	_ = json.Unmarshal(item["type"], &kind)
	if kind != "function_call" {
		return nil, nil
	}
	var itemID, callID, name string
	_ = json.Unmarshal(item["id"], &itemID)
	_ = json.Unmarshal(item["call_id"], &callID)
	_ = json.Unmarshal(item["name"], &name)
	if itemID == "" {
		itemID = callID
	}
	if itemID == "" || name == "" {
		return nil, errors.New("function_call output item has no id or name")
	}
	if callID == "" {
		callID = itemID
	}
	toolIndex := len(c.toolCalls)
	c.toolCalls[itemID] = chatStreamToolCall{index: toolIndex, callID: callID}
	c.hasToolCalls = true
	return c.chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": toolIndex, "id": callID, "type": "function", "function": map[string]string{"name": name, "arguments": ""},
	}}}, nil), nil
}

func (c *chatStreamConverter) addFunctionArguments(rawItem json.RawMessage, itemID string, rawDelta json.RawMessage) (map[string]any, error) {
	if len(rawItem) != 0 {
		var item map[string]json.RawMessage
		if json.Unmarshal(rawItem, &item) == nil {
			_ = json.Unmarshal(item["id"], &itemID)
		}
	}
	if itemID == "" {
		return nil, nil
	}
	call, found := c.toolCalls[itemID]
	if !found {
		return nil, nil
	}
	var delta string
	if err := json.Unmarshal(rawDelta, &delta); err != nil {
		return nil, fmt.Errorf("decode function call arguments delta: %w", err)
	}
	call.argumentsWereSent = true
	c.toolCalls[itemID] = call
	return c.chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": call.index, "function": map[string]string{"arguments": delta},
	}}}, nil), nil
}

func (c *chatStreamConverter) finishToolCall(rawItem json.RawMessage) (map[string]any, error) {
	if len(rawItem) == 0 {
		return nil, nil
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(rawItem, &item); err != nil || item == nil {
		return nil, errors.New("decode response.output_item.done item")
	}
	var kind, itemID, arguments string
	_ = json.Unmarshal(item["type"], &kind)
	_ = json.Unmarshal(item["id"], &itemID)
	_ = json.Unmarshal(item["arguments"], &arguments)
	if kind != "function_call" || itemID == "" {
		return nil, nil
	}
	call, found := c.toolCalls[itemID]
	if !found || call.argumentsWereSent || arguments == "" {
		return nil, nil
	}
	return c.chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": call.index, "function": map[string]string{"arguments": arguments},
	}}}, nil), nil
}
