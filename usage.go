package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

const maxResponseAnalyzeBytes = 8 << 20

func extractEndpointResponseFields(info endpointInfo, payload []byte) map[string]any {
	if len(payload) == 0 {
		return nil
	}
	switch info.Provider {
	case "anthropic":
		return extractAnthropicResponseFields(payload)
	case "gemini":
		return extractGeminiResponseFields(payload)
	default:
		return nil
	}
}

func extractAnthropicResponseFields(payload []byte) map[string]any {
	usage := map[string]any{}
	fields := map[string]any{}
	for _, raw := range responseJSONPayloads(payload) {
		var root map[string]any
		if json.Unmarshal(raw, &root) != nil {
			continue
		}
		mergeAnthropicRoot(fields, usage, root)
		if msg, ok := objectAt(root, "message"); ok {
			mergeAnthropicRoot(fields, usage, msg)
		}
	}
	if len(usage) > 0 {
		setTokenTotal(usage)
		fields["usage"] = usage
	}
	return nilIfEmptyMap(fields)
}

func mergeAnthropicRoot(fields map[string]any, usage map[string]any, root map[string]any) {
	if model, ok := stringAt(root, "model"); ok && fields["response_model"] == nil {
		fields["response_model"] = model
	}
	if stop, ok := stringAt(root, "stop_reason"); ok && stop != "" {
		fields["stopped_reason"] = stop
	}
	if delta, ok := objectAt(root, "delta"); ok {
		if stop, ok := stringAt(delta, "stop_reason"); ok && stop != "" {
			fields["stopped_reason"] = stop
		}
	}
	if u, ok := objectAt(root, "usage"); ok {
		for _, key := range []string{
			"input_tokens",
			"output_tokens",
			"cache_read_input_tokens",
			"cache_creation_input_tokens",
			"thinking_tokens",
		} {
			if v, ok := intAt(u, key); ok {
				usage[key] = v
			}
		}
	}
	if v, ok := intAt(root, "input_tokens"); ok {
		usage["input_tokens"] = v
	}
}

func extractGeminiResponseFields(payload []byte) map[string]any {
	usage := map[string]any{}
	fields := map[string]any{}
	for _, raw := range responseJSONPayloads(payload) {
		var root map[string]any
		if json.Unmarshal(raw, &root) != nil {
			continue
		}
		mergeGeminiRoot(fields, usage, root)
		if resp, ok := objectAt(root, "response"); ok {
			mergeGeminiRoot(fields, usage, resp)
		}
	}
	if len(usage) > 0 {
		setTokenTotal(usage)
		fields["usage"] = usage
	}
	return nilIfEmptyMap(fields)
}

func mergeGeminiRoot(fields map[string]any, usage map[string]any, root map[string]any) {
	if model, ok := stringAt(root, "modelVersion"); ok && fields["response_model"] == nil {
		fields["response_model"] = model
	}
	if responseID, ok := stringAt(root, "responseId"); ok && fields["response_id"] == nil {
		fields["response_id"] = responseID
	}
	if candidates, ok := root["candidates"].([]any); ok {
		for _, rawCandidate := range candidates {
			candidate, ok := rawCandidate.(map[string]any)
			if !ok {
				continue
			}
			if finish, ok := stringAt(candidate, "finishReason"); ok && finish != "" {
				fields["stopped_reason"] = finish
				break
			}
		}
	}
	if u, ok := objectAt(root, "usageMetadata"); ok {
		if v, ok := intAt(u, "promptTokenCount"); ok {
			usage["input_tokens"] = v
		}
		if v, ok := intAt(u, "candidatesTokenCount"); ok {
			usage["output_tokens"] = v
		}
		if v, ok := intAt(u, "totalTokenCount"); ok {
			usage["total_tokens"] = v
		}
		if v, ok := intAt(u, "cachedContentTokenCount"); ok {
			usage["cached_tokens"] = v
		}
		if v, ok := intAt(u, "thoughtsTokenCount"); ok {
			usage["reasoning_tokens"] = v
		}
	}
	if v, ok := intAt(root, "totalTokens"); ok {
		usage["total_tokens"] = v
	}
}

func responseJSONPayloads(payload []byte) [][]byte {
	var out [][]byte
	appendJSONPayload := func(raw []byte) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || bytes.Equal(raw, []byte("[DONE]")) || !json.Valid(raw) {
			return
		}
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) == nil {
			for _, item := range arr {
				out = append(out, append([]byte(nil), item...))
			}
			return
		}
		out = append(out, append([]byte(nil), raw...))
	}

	appendJSONPayload(payload)

	scanner := bufio.NewScanner(bytes.NewReader(payload))
	scanner.Buffer(make([]byte, 0, 64*1024), maxResponseAnalyzeBytes)
	var eventData []string
	flushEvent := func() {
		if len(eventData) == 0 {
			return
		}
		appendJSONPayload([]byte(strings.Join(eventData, "\n")))
		eventData = nil
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			flushEvent()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			eventData = append(eventData, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
		appendJSONPayload([]byte(strings.TrimSpace(line)))
	}
	flushEvent()
	return out
}

func objectAt(m map[string]any, key string) (map[string]any, bool) {
	v, ok := m[key].(map[string]any)
	return v, ok
}

func stringAt(m map[string]any, key string) (string, bool) {
	v, ok := m[key].(string)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(v), true
}

func intAt(m map[string]any, key string) (int64, bool) {
	switch v := m[key].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

func setTokenTotal(usage map[string]any) {
	if _, ok := usage["total_tokens"]; ok {
		return
	}
	in, inOK := usage["input_tokens"].(int64)
	out, outOK := usage["output_tokens"].(int64)
	if inOK || outOK {
		usage["total_tokens"] = in + out
	}
}

func nilIfEmptyMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	return m
}
