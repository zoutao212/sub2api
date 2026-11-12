package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// Meta MaaS models on Vertex (e.g. muse-ember-1.0) are served ONLY through the
// publisher Responses route (.../publishers/meta/v1/responses, model in body).
// The OpenAI-compatible endpoints/openapi/chat/completions route used for
// DeepSeek returns 404 for them, so they get their own branch here. The wire
// protocol is the OpenAI Responses API; every client format is translated to
// Responses on the way out and back to the client's format on the way in.

const vertexMetaResponsesEndpoint = "/publishers/meta/v1/responses"

func isVertexMetaMaaSModel(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(lower, "publishers/meta/") || strings.HasPrefix(lower, "meta/")
}

// normalizeVertexMetaModelID reduces a mapped model id to the bare name the
// Meta Responses endpoint expects in the body:
//
//	publishers/meta/models/muse-ember-1.0 -> muse-ember-1.0
//	meta/muse-ember-1.0                   -> muse-ember-1.0
func normalizeVertexMetaModelID(model string) string {
	trimmed := strings.TrimSpace(model)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "publishers/") {
		rest := trimmed[len("publishers/"):]
		if idx := strings.Index(strings.ToLower(rest), "/models/"); idx != -1 {
			return rest[idx+len("/models/"):]
		}
		return rest
	}
	if strings.HasPrefix(lower, "meta/") {
		return trimmed[len("meta/"):]
	}
	return trimmed
}

func buildVertexMetaResponsesURL(projectID, location string) (string, error) {
	projectID = strings.TrimSpace(projectID)
	location = strings.TrimSpace(location)
	if projectID == "" {
		return "", errors.New("vertex project_id is required for third-party maas models")
	}
	if location == "" {
		location = vertexDefaultLocation
	}
	if !vertexLocationPattern.MatchString(location) {
		return "", fmt.Errorf("invalid vertex location: %s", location)
	}
	host := fmt.Sprintf("%s-aiplatform.googleapis.com", location)
	if location == "global" {
		host = vertexGlobalHost
	}
	return fmt.Sprintf(
		"https://%s/v1/projects/%s/locations/%s%s",
		host, projectID, location, vertexMetaResponsesEndpoint,
	), nil
}

func claudeUsageFromResponsesUsage(u *apicompat.ResponsesUsage) ClaudeUsage {
	var cu ClaudeUsage
	if u == nil {
		return cu
	}
	cu.InputTokens = u.InputTokens
	cu.OutputTokens = u.OutputTokens
	if u.InputTokensDetails != nil {
		cu.CacheReadInputTokens = u.InputTokensDetails.CachedTokens
	}
	return cu
}

// vertexMetaRoundTrip shares the transport machinery (auth, retry, failover,
// upstream error accounting) across the four Meta entry points. writeTransportErr
// renders transport-level failures in the client's protocol. On a non-failover
// upstream error the response is returned with its body re-buffered so the caller
// can write its own protocol-specific mapped error.
func (s *GeminiMessagesCompatService) vertexMetaRoundTrip(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	targetURL string,
	body []byte,
	writeTransportErr func(status int, errType, message string) error,
) (*http.Response, string, error) {
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	buildReq := func(reqCtx context.Context) (*http.Request, string, error) {
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, bytes.NewReader(body))
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Content-Type", "application/json")
		if err := setVertexOpenAIAuth(reqCtx, req, account, s.tokenProvider); err != nil {
			return nil, "", err
		}
		return req, "x-request-id", nil
	}

	var resp *http.Response
	var requestIDHeader string
	maxAttempts := s.geminiMaxAttemptsPerAccount()
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		upstreamReq, idHeader, err := buildReq(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, "", err
			}
			if failoverErr := s.handleVertexServiceAccountNotFound(ctx, c, account, err); failoverErr != nil {
				return nil, "", failoverErr
			}
			return nil, "", writeTransportErr(http.StatusBadGateway, "upstream_error", err.Error())
		}
		requestIDHeader = idHeader

		resp, err = s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
		if err != nil {
			return nil, "", s.handleUpstreamTransportError(ctx, c, account, err)
		}

		if resp.StatusCode >= 400 && s.shouldRetryGeminiUpstreamError(account, resp.StatusCode) {
			respBody := s.readUpstreamErrorBody(resp)
			_ = resp.Body.Close()
			if attempt < maxAttempts {
				sleepGeminiBackoff(attempt)
				continue
			}
			resp = &http.Response{
				StatusCode: resp.StatusCode,
				Header:     resp.Header.Clone(),
				Body:       io.NopCloser(bytes.NewReader(respBody)),
			}
			break
		}
		break
	}

	requestID := resp.Header.Get(requestIDHeader)
	if requestID == "" {
		requestID = resp.Header.Get("x-goog-request-id")
	}
	if requestID != "" {
		c.Header("x-request-id", requestID)
	}

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		s.handleGeminiUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
		if s.shouldFailoverGeminiUpstreamError(resp.StatusCode) {
			return nil, "", &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}
	return resp, requestID, nil
}

func (s *GeminiMessagesCompatService) vertexMetaTargetURL(c *gin.Context, account *Account, mappedModel string, writeErr func(int, string, string) error) (string, string, error) {
	projectID := account.VertexProjectID()
	if strings.TrimSpace(projectID) == "" {
		return "", "", writeErr(http.StatusBadRequest, "invalid_request_error",
			"Vertex third-party models require project_id to be configured on the account")
	}
	location := account.VertexLocation(mappedModel)
	targetURL, err := buildVertexMetaResponsesURL(projectID, location)
	if err != nil {
		return "", "", writeErr(http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	SetActualOpenAIUpstreamEndpoint(c, vertexMetaResponsesEndpoint)
	return targetURL, normalizeVertexMetaModelID(mappedModel), nil
}

// scanVertexMetaSSE reads upstream Responses SSE events, invoking onEvent for each
// parsed event. It stops at [DONE] or EOF.
func scanVertexMetaSSE(body io.Reader, onEvent func(evt *apicompat.ResponsesStreamEvent)) error {
	scanner := bufio.NewScanner(body)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)
	for scanner.Scan() {
		payload, ok := extractOpenAISSEDataLine(scanner.Text())
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(payload)
		if trimmed == "[DONE]" {
			break
		}
		var evt apicompat.ResponsesStreamEvent
		if err := json.Unmarshal([]byte(trimmed), &evt); err != nil {
			continue
		}
		onEvent(&evt)
	}
	return scanner.Err()
}

func responsesTerminalUsage(evt *apicompat.ResponsesStreamEvent) *apicompat.ResponsesUsage {
	if evt.Response != nil && evt.Response.Usage != nil {
		return evt.Response.Usage
	}
	return evt.Usage
}

// ---------------------------------------------------------------------------
// /v1/responses -> Meta Responses (near passthrough)
// ---------------------------------------------------------------------------

func (s *GeminiMessagesCompatService) forwardVertexMetaResponses(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	mappedModel string,
	clientToolMapping apicompat.ResponsesClientToolMapping,
	startTime time.Time,
) (*ForwardResult, error) {
	writeErr := func(st int, et, msg string) error { return writeResponsesCompatError(c, st, et, msg) }

	var responsesReq apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &responsesReq); err != nil {
		return nil, writeErr(http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	}
	clientStream := responsesReq.Stream

	targetURL, upstreamModel, err := s.vertexMetaTargetURL(c, account, mappedModel, writeErr)
	if err != nil {
		return nil, err
	}
	responsesReq.Model = upstreamModel
	outBody, err := json.Marshal(&responsesReq)
	if err != nil {
		return nil, fmt.Errorf("marshal responses request: %w", err)
	}

	resp, requestID, err := s.vertexMetaRoundTrip(ctx, c, account, targetURL, outBody, writeErr)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		logger.L().Warn("vertex meta responses upstream error",
			zap.Int64("account_id", account.ID), zap.Int("status_code", resp.StatusCode),
			zap.String("target_url", targetURL), zap.String("upstream_model", upstreamModel),
			zap.String("upstream_resp", string(respBody)))
		return nil, s.writeGeminiResponsesMappedError(c, account, resp.StatusCode, requestID, respBody)
	}

	var usage ClaudeUsage
	var firstTokenMs *int
	clientDisconnect := false

	if clientStream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)

		restorer := apicompat.NewResponsesClientToolStreamRestorer(clientToolMapping)
		writeEvent := func(evt *apicompat.ResponsesStreamEvent) {
			if clientDisconnect {
				return
			}
			if evt.Response != nil && evt.Response.Model != "" {
				evt.Response.Model = originalModel
			}
			s.cacheReasoningItemsFromEvents([]apicompat.ResponsesStreamEvent{*evt})
			payload, err := json.Marshal(evt)
			if err != nil {
				return
			}
			payload = reverseToolNamesIfPresent(c, payload)
			payloads, _, err := restorer.RestoreEvent(payload)
			if err != nil {
				return
			}
			for _, restored := range payloads {
				eventType := gjson.GetBytes(restored, "type").String()
				if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", eventType, restored); err != nil {
					clientDisconnect = true
					return
				}
			}
			c.Writer.Flush()
		}

		scanErr := scanVertexMetaSSE(resp.Body, func(evt *apicompat.ResponsesStreamEvent) {
			if u := responsesTerminalUsage(evt); u != nil {
				usage = claudeUsageFromResponsesUsage(u)
			}
			if firstTokenMs == nil && evt.Type == "response.output_text.delta" && evt.Delta != "" {
				elapsed := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &elapsed
			}
			writeEvent(evt)
		})
		if scanErr != nil {
			return nil, fmt.Errorf("vertex meta responses stream read error: %w", scanErr)
		}
		if !clientDisconnect {
			if _, err := fmt.Fprint(c.Writer, "data: [DONE]\n\n"); err != nil {
				clientDisconnect = true
			} else {
				c.Writer.Flush()
			}
		}
	} else {
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, writeErr(http.StatusBadGateway, "upstream_error", "Failed to read upstream response")
		}
		var responsesResp apicompat.ResponsesResponse
		if err := json.Unmarshal(respBody, &responsesResp); err != nil {
			return nil, writeErr(http.StatusBadGateway, "upstream_error", "Failed to parse upstream response")
		}
		responsesResp.Model = originalModel
		usage = claudeUsageFromResponsesUsage(responsesResp.Usage)
		s.cacheReasoningItemsFromOutput(responsesResp.Output)
		writeResponsesJSON(c, s, resp, &responsesResp, clientToolMapping)
	}

	return &ForwardResult{
		RequestID:        requestID,
		UpstreamHeaders:  resp.Header,
		Usage:            usage,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		Stream:           clientStream,
		Duration:         time.Since(startTime),
		FirstTokenMs:     firstTokenMs,
		ClientDisconnect: clientDisconnect,
	}, nil
}

// ---------------------------------------------------------------------------
// /v1/chat/completions -> Meta Responses -> chat.completion
// ---------------------------------------------------------------------------

func (s *GeminiMessagesCompatService) forwardVertexMetaChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	mappedModel string,
	clientStream bool,
	startTime time.Time,
) (*ForwardResult, error) {
	writeErr := func(st int, et, msg string) error { return s.writeChatCompletionsError(c, st, et, msg) }

	var chatReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &chatReq); err != nil {
		return nil, writeErr(http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	}
	targetURL, upstreamModel, err := s.vertexMetaTargetURL(c, account, mappedModel, writeErr)
	if err != nil {
		return nil, err
	}
	responsesReq, err := apicompat.ChatCompletionsToResponses(&chatReq)
	if err != nil {
		return nil, writeErr(http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	responsesReq.Model = upstreamModel
	responsesReq.Stream = clientStream
	outBody, err := json.Marshal(responsesReq)
	if err != nil {
		return nil, fmt.Errorf("marshal responses request: %w", err)
	}

	resp, requestID, err := s.vertexMetaRoundTrip(ctx, c, account, targetURL, outBody, writeErr)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		logger.L().Warn("vertex meta chat_completions upstream error",
			zap.Int64("account_id", account.ID), zap.Int("status_code", resp.StatusCode),
			zap.String("target_url", targetURL), zap.String("upstream_model", upstreamModel),
			zap.String("upstream_resp", string(respBody)))
		return nil, s.writeGeminiChatCompletionsMappedError(c, account, resp.StatusCode, requestID, respBody)
	}

	var usage ClaudeUsage
	var firstTokenMs *int
	clientDisconnect := false

	if clientStream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)

		state := apicompat.NewResponsesEventToChatState()
		state.Model = originalModel
		state.IncludeUsage = true
		writeChunk := func(chunk apicompat.ChatCompletionsChunk) {
			if clientDisconnect {
				return
			}
			sse, err := apicompat.ChatChunkToSSE(chunk)
			if err != nil {
				return
			}
			if _, err := c.Writer.WriteString(sse); err != nil {
				clientDisconnect = true
				return
			}
			c.Writer.Flush()
		}
		scanErr := scanVertexMetaSSE(resp.Body, func(evt *apicompat.ResponsesStreamEvent) {
			if u := responsesTerminalUsage(evt); u != nil {
				usage = claudeUsageFromResponsesUsage(u)
			}
			if firstTokenMs == nil && evt.Type == "response.output_text.delta" && evt.Delta != "" {
				elapsed := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &elapsed
			}
			for _, chunk := range apicompat.ResponsesEventToChatChunks(evt, state) {
				writeChunk(chunk)
			}
		})
		if scanErr != nil {
			return nil, fmt.Errorf("vertex meta chat_completions stream read error: %w", scanErr)
		}
		for _, chunk := range apicompat.FinalizeResponsesChatStream(state) {
			writeChunk(chunk)
		}
		if !clientDisconnect {
			if _, err := fmt.Fprint(c.Writer, "data: [DONE]\n\n"); err != nil {
				clientDisconnect = true
			} else {
				c.Writer.Flush()
			}
		}
	} else {
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, writeErr(http.StatusBadGateway, "upstream_error", "Failed to read upstream response")
		}
		var responsesResp apicompat.ResponsesResponse
		if err := json.Unmarshal(respBody, &responsesResp); err != nil {
			return nil, writeErr(http.StatusBadGateway, "upstream_error", "Failed to parse upstream response")
		}
		usage = claudeUsageFromResponsesUsage(responsesResp.Usage)
		ccResp := apicompat.ResponsesToChatCompletions(&responsesResp, originalModel)
		c.JSON(http.StatusOK, ccResp)
	}

	return &ForwardResult{
		RequestID:        requestID,
		UpstreamHeaders:  resp.Header,
		Usage:            usage,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		Stream:           clientStream,
		Duration:         time.Since(startTime),
		FirstTokenMs:     firstTokenMs,
		ClientDisconnect: clientDisconnect,
	}, nil
}

// ---------------------------------------------------------------------------
// /v1/messages (Claude) -> Meta Responses -> Anthropic message
// ---------------------------------------------------------------------------

func (s *GeminiMessagesCompatService) forwardVertexMetaClaudeMessages(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	mappedModel string,
	clientStream bool,
	startTime time.Time,
) (*ForwardResult, error) {
	writeErr := func(st int, et, msg string) error { return s.writeClaudeError(c, st, et, msg) }

	var anthropicReq apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		return nil, writeErr(http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	}
	targetURL, upstreamModel, err := s.vertexMetaTargetURL(c, account, mappedModel, writeErr)
	if err != nil {
		return nil, err
	}
	responsesReq, err := apicompat.AnthropicToResponses(&anthropicReq)
	if err != nil {
		return nil, writeErr(http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	responsesReq.Model = upstreamModel
	responsesReq.Stream = clientStream
	outBody, err := json.Marshal(responsesReq)
	if err != nil {
		return nil, fmt.Errorf("marshal responses request: %w", err)
	}

	resp, requestID, err := s.vertexMetaRoundTrip(ctx, c, account, targetURL, outBody, writeErr)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		logger.L().Warn("vertex meta claude_messages upstream error",
			zap.Int64("account_id", account.ID), zap.Int("status_code", resp.StatusCode),
			zap.String("target_url", targetURL), zap.String("upstream_model", upstreamModel),
			zap.String("upstream_resp", string(respBody)))
		return nil, s.writeClaudeError(c, resp.StatusCode, "upstream_error", string(respBody))
	}

	var usage ClaudeUsage
	var firstTokenMs *int
	clientDisconnect := false

	if clientStream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)

		state := apicompat.NewResponsesEventToAnthropicState()
		state.Model = originalModel
		scanErr := scanVertexMetaSSE(resp.Body, func(evt *apicompat.ResponsesStreamEvent) {
			if u := responsesTerminalUsage(evt); u != nil {
				usage = claudeUsageFromResponsesUsage(u)
			}
			if firstTokenMs == nil && evt.Type == "response.output_text.delta" && evt.Delta != "" {
				elapsed := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &elapsed
			}
			for _, ev := range apicompat.ResponsesEventToAnthropicEvents(evt, state) {
				if clientDisconnect {
					return
				}
				sse, err := apicompat.ResponsesAnthropicEventToSSE(ev)
				if err != nil {
					continue
				}
				if _, err := c.Writer.WriteString(sse); err != nil {
					clientDisconnect = true
					return
				}
				c.Writer.Flush()
			}
		})
		if scanErr != nil {
			return nil, fmt.Errorf("vertex meta claude_messages stream read error: %w", scanErr)
		}
		for _, ev := range apicompat.FinalizeResponsesAnthropicStream(state) {
			if clientDisconnect {
				break
			}
			sse, err := apicompat.ResponsesAnthropicEventToSSE(ev)
			if err != nil {
				continue
			}
			if _, err := c.Writer.WriteString(sse); err != nil {
				clientDisconnect = true
				break
			}
			c.Writer.Flush()
		}
	} else {
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, writeErr(http.StatusBadGateway, "upstream_error", "Failed to read upstream response")
		}
		var responsesResp apicompat.ResponsesResponse
		if err := json.Unmarshal(respBody, &responsesResp); err != nil {
			return nil, writeErr(http.StatusBadGateway, "upstream_error", "Failed to parse upstream response")
		}
		usage = claudeUsageFromResponsesUsage(responsesResp.Usage)
		anthropicResp := apicompat.ResponsesToAnthropic(&responsesResp, originalModel)
		c.JSON(http.StatusOK, anthropicResp)
	}

	return &ForwardResult{
		RequestID:        requestID,
		UpstreamHeaders:  resp.Header,
		Usage:            usage,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		Stream:           clientStream,
		Duration:         time.Since(startTime),
		FirstTokenMs:     firstTokenMs,
		ClientDisconnect: clientDisconnect,
	}, nil
}

// ---------------------------------------------------------------------------
// Gemini native (:generateContent) -> Meta Responses -> Gemini response
// ---------------------------------------------------------------------------

func (s *GeminiMessagesCompatService) forwardVertexMetaNative(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	mappedModel string,
	action string,
	stream bool,
	startTime time.Time,
) (*ForwardResult, error) {
	writeErr := func(st int, et, msg string) error { return s.writeGoogleError(c, st, msg) }
	if action != "generateContent" && action != "streamGenerateContent" {
		return nil, s.writeGoogleError(c, http.StatusBadRequest, "Unsupported action for third-party model: "+action)
	}

	targetURL, upstreamModel, err := s.vertexMetaTargetURL(c, account, mappedModel, writeErr)
	if err != nil {
		return nil, err
	}

	chatReq := vertexMetaGeminiBodyToChatRequest(body, upstreamModel, stream)
	responsesReq, err := apicompat.ChatCompletionsToResponses(chatReq)
	if err != nil {
		return nil, s.writeGoogleError(c, http.StatusBadRequest, err.Error())
	}
	responsesReq.Model = upstreamModel
	responsesReq.Stream = stream
	outBody, err := json.Marshal(responsesReq)
	if err != nil {
		return nil, fmt.Errorf("marshal responses request: %w", err)
	}

	resp, requestID, err := s.vertexMetaRoundTrip(ctx, c, account, targetURL, outBody, writeErr)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		logger.L().Warn("vertex meta native upstream error",
			zap.Int64("account_id", account.ID), zap.Int("status_code", resp.StatusCode),
			zap.String("target_url", targetURL), zap.String("upstream_model", upstreamModel),
			zap.String("upstream_resp", string(respBody)))
		return nil, s.writeGoogleError(c, resp.StatusCode, string(respBody))
	}

	var usage ClaudeUsage
	var firstTokenMs *int
	clientDisconnect := false

	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)

		state := apicompat.NewResponsesEventToChatState()
		state.Model = upstreamModel
		scanErr := scanVertexMetaSSE(resp.Body, func(evt *apicompat.ResponsesStreamEvent) {
			if u := responsesTerminalUsage(evt); u != nil {
				usage = claudeUsageFromResponsesUsage(u)
			}
			for _, chunk := range apicompat.ResponsesEventToChatChunks(evt, state) {
				if clientDisconnect || len(chunk.Choices) == 0 {
					continue
				}
				delta := chunk.Choices[0].Delta
				if delta.Content == nil || *delta.Content == "" {
					continue
				}
				if firstTokenMs == nil {
					elapsed := int(time.Since(startTime).Milliseconds())
					firstTokenMs = &elapsed
				}
				geminiChunk := map[string]any{
					"candidates": []map[string]any{{
						"content": map[string]any{
							"parts": []map[string]any{{"text": *delta.Content}},
							"role":  "model",
						},
					}},
				}
				b, _ := json.Marshal(geminiChunk)
				if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", b); err != nil {
					clientDisconnect = true
					return
				}
				c.Writer.Flush()
			}
		})
		if scanErr != nil {
			return nil, fmt.Errorf("vertex meta native stream read error: %w", scanErr)
		}
	} else {
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, s.writeGoogleError(c, http.StatusBadGateway, "Failed to read upstream response")
		}
		var responsesResp apicompat.ResponsesResponse
		if err := json.Unmarshal(respBody, &responsesResp); err != nil {
			return nil, s.writeGoogleError(c, http.StatusBadGateway, "Failed to parse upstream response")
		}
		usage = claudeUsageFromResponsesUsage(responsesResp.Usage)
		ccResp := apicompat.ResponsesToChatCompletions(&responsesResp, upstreamModel)
		replyText := ""
		if len(ccResp.Choices) > 0 {
			var str string
			if err := json.Unmarshal(ccResp.Choices[0].Message.Content, &str); err == nil {
				replyText = str
			} else {
				replyText = string(ccResp.Choices[0].Message.Content)
			}
		}
		geminiResp := map[string]any{
			"candidates": []map[string]any{{
				"content": map[string]any{
					"parts": []map[string]any{{"text": replyText}},
					"role":  "model",
				},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{
				"promptTokenCount":     usage.InputTokens,
				"candidatesTokenCount": usage.OutputTokens,
				"totalTokenCount":      usage.InputTokens + usage.OutputTokens,
			},
		}
		c.JSON(http.StatusOK, geminiResp)
	}

	return &ForwardResult{
		RequestID:        requestID,
		UpstreamHeaders:  resp.Header,
		Usage:            usage,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		Stream:           stream,
		Duration:         time.Since(startTime),
		FirstTokenMs:     firstTokenMs,
		ClientDisconnect: clientDisconnect,
	}, nil
}

// vertexMetaGeminiBodyToChatRequest mirrors the Gemini->Chat message flattening
// used by the OpenAI-compatible third-party native path.
func vertexMetaGeminiBodyToChatRequest(body []byte, upstreamModel string, stream bool) *apicompat.ChatCompletionsRequest {
	messages := make([]apicompat.ChatMessage, 0)
	if sysParts := gjson.GetBytes(body, "systemInstruction.parts").Array(); len(sysParts) > 0 {
		var sysText strings.Builder
		for _, p := range sysParts {
			if t := p.Get("text").String(); t != "" {
				if sysText.Len() > 0 {
					sysText.WriteString("\n")
				}
				sysText.WriteString(t)
			}
		}
		if sysText.Len() > 0 {
			raw, _ := json.Marshal(sysText.String())
			messages = append(messages, apicompat.ChatMessage{Role: "system", Content: raw})
		}
	}
	for _, item := range gjson.GetBytes(body, "contents").Array() {
		role := item.Get("role").String()
		if role == "model" {
			role = "assistant"
		} else if role == "" {
			role = "user"
		}
		var textBuilder strings.Builder
		for _, p := range item.Get("parts").Array() {
			if t := p.Get("text").String(); t != "" {
				if textBuilder.Len() > 0 {
					textBuilder.WriteString("\n")
				}
				textBuilder.WriteString(t)
			}
		}
		raw, _ := json.Marshal(textBuilder.String())
		messages = append(messages, apicompat.ChatMessage{Role: role, Content: raw})
	}
	req := &apicompat.ChatCompletionsRequest{Model: upstreamModel, Messages: messages, Stream: stream}
	if temp := gjson.GetBytes(body, "generationConfig.temperature"); temp.Exists() {
		v := temp.Float()
		req.Temperature = &v
	}
	if topP := gjson.GetBytes(body, "generationConfig.topP"); topP.Exists() {
		v := topP.Float()
		req.TopP = &v
	}
	if maxTokens := gjson.GetBytes(body, "generationConfig.maxOutputTokens"); maxTokens.Exists() {
		v := int(maxTokens.Int())
		req.MaxTokens = &v
	}
	return req
}