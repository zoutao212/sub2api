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
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// vertexThirdPartyOpenAIEndpoint 是第三方 MaaS 模型实际使用的 Vertex OpenAI 兼容端点标签，
// 写入 usage 记录，避免与 Gemini 原生的 /v1beta/models 混淆。
const vertexThirdPartyOpenAIEndpoint = "/endpoints/openapi/chat/completions"

// forwardVertexThirdPartyChatCompletions 直转客户端的 Chat Completions 请求到 Vertex AI 的
// OpenAI 兼容端点 (endpoints/openapi/chat/completions)。
// 适用于 account.platform=gemini && account.type=service_account，且目标模型为第三方 MaaS 模型（如 DeepSeek）。
func (s *GeminiMessagesCompatService) forwardVertexThirdPartyChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	mappedModel string,
	clientStream bool,
	includeUsage bool,
	startTime time.Time,
) (*ForwardResult, error) {
	projectID := account.VertexProjectID()
	if strings.TrimSpace(projectID) == "" {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error",
			"Vertex third-party models require project_id to be configured on the account")
	}

	location := account.VertexLocation(mappedModel)
	targetURL, err := buildVertexOpenAIEndpointURL(projectID, location)
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	SetActualOpenAIUpstreamEndpoint(c, vertexThirdPartyOpenAIEndpoint)

	upstreamModel := NormalizeVertexThirdPartyModelID(mappedModel)
	upstreamBody := body
	if upstreamModel != originalModel {
		upstreamBody = ReplaceModelInBody(body, upstreamModel)
	}
	if normBody, changed := NormalizeVertexOpenAIReasoningEffort(upstreamBody); changed {
		upstreamBody = normBody
	}
	logVertexPromptHistoryDriftForChatBody(c, account, "chat_maas", upstreamModel, upstreamBody)

	if clientStream {
		if uBody, err := ensureOpenAIChatStreamUsage(upstreamBody); err == nil {
			upstreamBody = uBody
		}
	}
	if !gjson.GetBytes(upstreamBody, "prompt_cache_key").Exists() {
		key := ExtractClientSessionID(c)
		if key != "" {
			if next, err := sjson.SetBytes(upstreamBody, "prompt_cache_key", key); err == nil {
				upstreamBody = next
			}
		}
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	buildReq := func(reqCtx context.Context) (*http.Request, string, error) {
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, bytes.NewReader(upstreamBody))
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
				return nil, err
			}
			if failoverErr := s.handleVertexServiceAccountNotFound(ctx, c, account, err); failoverErr != nil {
				return nil, failoverErr
			}
			return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", err.Error())
		}
		requestIDHeader = idHeader

		resp, err = s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
		if err != nil {
			return nil, s.handleUpstreamTransportError(ctx, c, account, err)
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
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()

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
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		logger.L().Warn("vertex third_party chat_completions upstream error",
			zap.Int64("account_id", account.ID),
			zap.Int("status_code", resp.StatusCode),
			zap.String("target_url", targetURL),
			zap.String("upstream_model", upstreamModel),
			zap.String("upstream_resp", string(respBody)),
		)
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

		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 64*1024)
		scanner.Buffer(buf, 4*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			if payload, ok := extractOpenAISSEDataLine(line); ok {
				trimmedPayload := strings.TrimSpace(payload)
				if trimmedPayload != "[DONE]" {
					if u := extractCCStreamUsage(trimmedPayload); u != nil {
						usage.InputTokens = u.InputTokens
						usage.OutputTokens = u.OutputTokens
						usage.CacheReadInputTokens = u.CacheReadInputTokens
					}
					if firstTokenMs == nil && !isOpenAIChatUsageOnlyStreamChunk(trimmedPayload) {
						elapsed := int(time.Since(startTime).Milliseconds())
						firstTokenMs = &elapsed
					}
				}
			}

			if !clientDisconnect {
				if _, err := c.Writer.WriteString(line + "\n"); err != nil {
					clientDisconnect = true
				} else {
					c.Writer.Flush()
				}
			}
		}
	} else {
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", "Failed to read upstream response")
		}
		if parsedUsage, ok := openAIUsageFromGJSON(gjson.GetBytes(respBody, "usage")); ok {
			usage.InputTokens = parsedUsage.InputTokens
			usage.OutputTokens = parsedUsage.OutputTokens
			usage.CacheReadInputTokens = parsedUsage.CacheReadInputTokens
		}

		c.Data(http.StatusOK, "application/json", respBody)
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

// forwardVertexThirdPartyNative 处理通过 Gemini 原生端点（/v1beta/models/...:generateContent）发起的第三方 MaaS 模型请求。
func (s *GeminiMessagesCompatService) forwardVertexThirdPartyNative(
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
	if action != "generateContent" && action != "streamGenerateContent" {
		return nil, s.writeGoogleError(c, http.StatusBadRequest, "Unsupported action for third-party model: "+action)
	}

	projectID := account.VertexProjectID()
	if strings.TrimSpace(projectID) == "" {
		return nil, s.writeGoogleError(c, http.StatusBadRequest, "Vertex third-party models require project_id to be configured on the account")
	}

	location := account.VertexLocation(mappedModel)
	targetURL, err := buildVertexOpenAIEndpointURL(projectID, location)
	if err != nil {
		return nil, s.writeGoogleError(c, http.StatusBadRequest, err.Error())
	}
	SetActualOpenAIUpstreamEndpoint(c, vertexThirdPartyOpenAIEndpoint)

	upstreamModel := NormalizeVertexThirdPartyModelID(mappedModel)

	messages := make([]map[string]any, 0)

	sysParts := gjson.GetBytes(body, "systemInstruction.parts").Array()
	if len(sysParts) > 0 {
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
			messages = append(messages, map[string]any{
				"role":    "system",
				"content": sysText.String(),
			})
		}
	}

	contents := gjson.GetBytes(body, "contents").Array()
	for _, item := range contents {
		role := item.Get("role").String()
		if role == "model" {
			role = "assistant"
		} else if role == "" {
			role = "user"
		}

		parts := item.Get("parts").Array()
		var textBuilder strings.Builder
		for _, p := range parts {
			if t := p.Get("text").String(); t != "" {
				if textBuilder.Len() > 0 {
					textBuilder.WriteString("\n")
				}
				textBuilder.WriteString(t)
			}
		}

		messages = append(messages, map[string]any{
			"role":    role,
			"content": textBuilder.String(),
		})
	}

	chatPayload := map[string]any{
		"model":    upstreamModel,
		"messages": messages,
		"stream":   stream,
	}

	if temp := gjson.GetBytes(body, "generationConfig.temperature"); temp.Exists() {
		chatPayload["temperature"] = temp.Float()
	}
	if topP := gjson.GetBytes(body, "generationConfig.topP"); topP.Exists() {
		chatPayload["top_p"] = topP.Float()
	}
	if maxTokens := gjson.GetBytes(body, "generationConfig.maxOutputTokens"); maxTokens.Exists() {
		chatPayload["max_tokens"] = maxTokens.Int()
	}

	chatBody, err := json.Marshal(chatPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal chat completions request: %w", err)
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	buildReq := func(reqCtx context.Context) (*http.Request, string, error) {
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, bytes.NewReader(chatBody))
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
				return nil, err
			}
			if failoverErr := s.handleVertexServiceAccountNotFound(ctx, c, account, err); failoverErr != nil {
				return nil, failoverErr
			}
			return nil, s.writeGoogleError(c, http.StatusBadGateway, err.Error())
		}
		requestIDHeader = idHeader

		resp, err = s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
		if err != nil {
			return nil, s.handleUpstreamTransportError(ctx, c, account, err)
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
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()

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
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
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

		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 64*1024)
		scanner.Buffer(buf, 4*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			payload, ok := extractOpenAISSEDataLine(line)
			if !ok {
				continue
			}
			trimmed := strings.TrimSpace(payload)
			if trimmed == "[DONE]" {
				c.Writer.Flush()
				break
			}

			var chunk apicompat.ChatCompletionsChunk
			if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
				continue
			}

			if chunkUsage := extractCCStreamUsage(trimmed); chunkUsage != nil {
				usage.InputTokens = chunkUsage.InputTokens
				usage.OutputTokens = chunkUsage.OutputTokens
				usage.CacheReadInputTokens = chunkUsage.CacheReadInputTokens
				if rawUsageNode := gjson.GetBytes([]byte(trimmed), "usage"); rawUsageNode.Exists() {
					logger.L().Info("vertex third-party raw upstream usage",
						zap.String("raw_usage", rawUsageNode.Raw),
						zap.Int("parsed_cache_read_tokens", chunkUsage.CacheReadInputTokens),
					)
				}
			}

			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != nil && *chunk.Choices[0].Delta.Content != "" {
				text := *chunk.Choices[0].Delta.Content
				if firstTokenMs == nil {
					elapsed := int(time.Since(startTime).Milliseconds())
					firstTokenMs = &elapsed
				}
				geminiChunk := map[string]any{
					"candidates": []map[string]any{
						{
							"content": map[string]any{
								"parts": []map[string]any{
									{"text": text},
								},
								"role": "model",
							},
						},
					},
				}
				b, _ := json.Marshal(geminiChunk)
				if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", b); err != nil {
					clientDisconnect = true
					break
				}
				c.Writer.Flush()
			}
		}
	} else {
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, s.writeGoogleError(c, http.StatusBadGateway, "Failed to read upstream response")
		}

		var ccResp apicompat.ChatCompletionsResponse
		if err := json.Unmarshal(respBody, &ccResp); err != nil {
			return nil, s.writeGoogleError(c, http.StatusBadGateway, "Failed to parse upstream response")
		}

		replyText := ""
		if len(ccResp.Choices) > 0 {
			var str string
			if err := json.Unmarshal(ccResp.Choices[0].Message.Content, &str); err == nil {
				replyText = str
			} else {
				replyText = string(ccResp.Choices[0].Message.Content)
			}
		}

		var promptTokens, completionTokens, totalTokens int
		if ccResp.Usage != nil {
			promptTokens = ccResp.Usage.PromptTokens
			completionTokens = ccResp.Usage.CompletionTokens
			totalTokens = ccResp.Usage.TotalTokens
			usage.InputTokens = ccResp.Usage.PromptTokens
			usage.OutputTokens = ccResp.Usage.CompletionTokens
			if parsedUsage, ok := openAIUsageFromGJSON(gjson.GetBytes(respBody, "usage")); ok {
				usage.CacheReadInputTokens = parsedUsage.CacheReadInputTokens
			}
		}

		geminiResp := map[string]any{
			"candidates": []map[string]any{
				{
					"content": map[string]any{
						"parts": []map[string]any{
							{"text": replyText},
						},
						"role": "model",
					},
					"finishReason": "STOP",
				},
			},
			"usageMetadata": map[string]any{
				"promptTokenCount":     promptTokens,
				"candidatesTokenCount": completionTokens,
				"totalTokenCount":      totalTokens,
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

// forwardVertexThirdPartyResponses 将 OpenAI Responses 请求转为 Chat Completions 请求，
// 发送至 Vertex AI 的 OpenAI 兼容端点，并将响应转回 Responses 格式。
func (s *GeminiMessagesCompatService) forwardVertexThirdPartyResponses(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	mappedModel string,
	clientToolMapping apicompat.ResponsesClientToolMapping,
	startTime time.Time,
) (*ForwardResult, error) {
	var responsesReq apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &responsesReq); err != nil {
		return nil, writeResponsesCompatError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	}

	clientStream := responsesReq.Stream

	reasoningItems, plaintextReasoningItems := vertexResponsesReasoningInputStats(responsesReq.Input)
	reasoningCacheLookups := 0
	reasoningCacheHits := 0
	reasoningCacheMisses := 0
	reasoningContentByID := func(itemID string) string {
		reasoningCacheLookups++
		content := s.reasoningContentByID(itemID)
		if content == "" {
			reasoningCacheMisses++
		} else {
			reasoningCacheHits++
		}
		return content
	}

	s.recacheReasoningItemsFromInput(responsesReq.Input)

	chatReq, err := apicompat.ResponsesToChatCompletionsRequestWithOptions(&responsesReq, &apicompat.ResponsesToChatOptions{
		ReasoningContentByID: reasoningContentByID,
	})
	if err != nil {
		return nil, writeResponsesCompatError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	upstreamModel := NormalizeVertexThirdPartyModelID(mappedModel)
	chatReq.Model = upstreamModel
	chatReq.Stream = clientStream
	if clientStream {
		chatReq.StreamOptions = &apicompat.ChatStreamOptions{IncludeUsage: true}
	}
	if strings.TrimSpace(chatReq.PromptCacheKey) == "" {
		if sessionID := ExtractClientSessionID(c); sessionID != "" {
			chatReq.PromptCacheKey = sessionID
		} else {
			chatReq.PromptCacheKey = deriveCompatPromptCacheKey(chatReq, upstreamModel)
		}
	}

	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal chat completions request: %w", err)
	}
	if normBody, changed := NormalizeVertexOpenAIReasoningEffort(chatBody); changed {
		chatBody = normBody
	}
	outgoingReasoningMessages := 0
	outgoingReasoningBytes := 0
	for _, message := range chatReq.Messages {
		if message.ReasoningContent != "" {
			outgoingReasoningMessages++
			outgoingReasoningBytes += len(message.ReasoningContent)
		}
	}
	logger.L().Info("vertex third-party responses reasoning reconstruction",
		zap.Int64("account_id", account.ID),
		zap.String("model", upstreamModel),
		zap.String("prompt_cache_key", chatReq.PromptCacheKey),
		zap.Int("input_reasoning_items", reasoningItems),
		zap.Int("input_plaintext_reasoning_items", plaintextReasoningItems),
		zap.Int("reasoning_cache_lookups", reasoningCacheLookups),
		zap.Int("reasoning_cache_hits", reasoningCacheHits),
		zap.Int("reasoning_cache_misses", reasoningCacheMisses),
		zap.Int("outgoing_reasoning_messages", outgoingReasoningMessages),
		zap.Int("outgoing_reasoning_bytes", outgoingReasoningBytes),
	)
	logVertexPromptHistoryDriftForChatBody(c, account, "responses_maas", upstreamModel, chatBody)

	projectID := account.VertexProjectID()
	if strings.TrimSpace(projectID) == "" {
		return nil, writeResponsesCompatError(c, http.StatusBadRequest, "invalid_request_error",
			"Vertex third-party models require project_id to be configured on the account")
	}

	location := account.VertexLocation(mappedModel)
	targetURL, err := buildVertexOpenAIEndpointURL(projectID, location)
	if err != nil {
		return nil, writeResponsesCompatError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	SetActualOpenAIUpstreamEndpoint(c, vertexThirdPartyOpenAIEndpoint)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	buildReq := func(reqCtx context.Context) (*http.Request, string, error) {
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, bytes.NewReader(chatBody))
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
				return nil, err
			}
			if failoverErr := s.handleVertexServiceAccountNotFound(ctx, c, account, err); failoverErr != nil {
				return nil, failoverErr
			}
			return nil, writeResponsesCompatError(c, http.StatusBadGateway, "upstream_error", err.Error())
		}
		requestIDHeader = idHeader

		resp, err = s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
		if err != nil {
			return nil, s.handleUpstreamTransportError(ctx, c, account, err)
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
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()

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
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
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

		state := apicompat.NewChatCompletionsToResponsesStreamState(originalModel)
		restorer := apicompat.NewResponsesClientToolStreamRestorer(clientToolMapping)
		writeEvents := func(events []apicompat.ResponsesStreamEvent) {
			if clientDisconnect {
				return
			}
			for _, event := range events {
				payload, err := json.Marshal(event)
				if err != nil {
					continue
				}
				payload = reverseToolNamesIfPresent(c, payload)
				payloads, _, err := restorer.RestoreEvent(payload)
				if err != nil {
					continue
				}
				for _, restored := range payloads {
					eventType := gjson.GetBytes(restored, "type").String()
					if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", eventType, restored); err != nil {
						clientDisconnect = true
						return
					}
				}
			}
			c.Writer.Flush()
		}

		scanner := bufio.NewScanner(resp.Body)
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

			var chunk apicompat.ChatCompletionsChunk
			if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
				continue
			}

			if chunkUsage := extractCCStreamUsage(trimmed); chunkUsage != nil {
				usage.InputTokens = chunkUsage.InputTokens
				usage.OutputTokens = chunkUsage.OutputTokens
				usage.CacheReadInputTokens = chunkUsage.CacheReadInputTokens
				logger.L().Info("vertex third-party responses stream usage",
					zap.String("chunk", trimmed),
					zap.Int("input_tokens", usage.InputTokens),
					zap.Int("output_tokens", usage.OutputTokens),
					zap.Int("cache_read_tokens", usage.CacheReadInputTokens),
				)
			}
			if firstTokenMs == nil && chatChunkStartsResponsesOutput(&chunk) {
				elapsed := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &elapsed
			}

			events := apicompat.ChatCompletionsChunkToResponsesEvents(&chunk, state)
			s.cacheReasoningItemsFromEvents(events)
			writeEvents(events)
		}
		if scanErr := scanner.Err(); scanErr != nil {
			return nil, fmt.Errorf("vertex third_party responses stream read error: %w", scanErr)
		}
		if err := state.ValidateToolCallArguments(); err != nil {
			return nil, fmt.Errorf("invalid tool call arguments from upstream: %w", err)
		}

		finalEvents := apicompat.FinalizeChatCompletionsResponsesStream(state)
		s.cacheReasoningItemsFromEvents(finalEvents)
		writeEvents(finalEvents)
		if !clientDisconnect {
			if _, err := fmt.Fprint(c.Writer, "data: [DONE]\n\n"); err != nil {
				clientDisconnect = true
			}
			if !clientDisconnect {
				c.Writer.Flush()
			}
		}
	} else {
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, writeResponsesCompatError(c, http.StatusBadGateway, "upstream_error", "Failed to read upstream response")
		}

		var ccResp apicompat.ChatCompletionsResponse
		if err := json.Unmarshal(respBody, &ccResp); err != nil {
			return nil, writeResponsesCompatError(c, http.StatusBadGateway, "upstream_error", "Failed to parse upstream response")
		}

		if parsedUsage, ok := openAIUsageFromGJSON(gjson.GetBytes(respBody, "usage")); ok {
			usage.InputTokens = parsedUsage.InputTokens
			usage.OutputTokens = parsedUsage.OutputTokens
			usage.CacheReadInputTokens = parsedUsage.CacheReadInputTokens
		}

		responsesResp := apicompat.ChatCompletionsResponseToResponses(&ccResp, originalModel, nil, nil, false, nil)
		if responsesResp == nil {
			return nil, writeResponsesCompatError(c, http.StatusBadGateway, "upstream_error", "Failed to convert response to Responses format")
		}
		s.cacheReasoningItemsFromOutput(responsesResp.Output)
		writeResponsesJSON(c, s, resp, responsesResp, clientToolMapping)
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

// forwardVertexThirdPartyClaudeMessages 将 Claude /v1/messages 请求转为 Chat Completions 请求，
// 发送至 Vertex AI 的 OpenAI 兼容端点，并将响应转回 Claude Messages 格式。
func (s *GeminiMessagesCompatService) forwardVertexThirdPartyClaudeMessages(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	mappedModel string,
	clientStream bool,
	startTime time.Time,
) (*ForwardResult, error) {
	var anthropicReq apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		return nil, s.writeClaudeError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	}

	chatReq, err := apicompat.AnthropicToChatCompletionsRequest(&anthropicReq)
	if err != nil {
		return nil, s.writeClaudeError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	upstreamModel := NormalizeVertexThirdPartyModelID(mappedModel)
	chatReq.Model = upstreamModel
	chatReq.Stream = clientStream
	if clientStream {
		chatReq.StreamOptions = &apicompat.ChatStreamOptions{IncludeUsage: true}
	}

	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal chat completions request: %w", err)
	}
	if normBody, changed := NormalizeVertexOpenAIReasoningEffort(chatBody); changed {
		chatBody = normBody
	}

	projectID := account.VertexProjectID()
	if strings.TrimSpace(projectID) == "" {
		return nil, s.writeClaudeError(c, http.StatusBadRequest, "invalid_request_error",
			"Vertex third-party models require project_id to be configured on the account")
	}

	location := account.VertexLocation(mappedModel)
	targetURL, err := buildVertexOpenAIEndpointURL(projectID, location)
	if err != nil {
		return nil, s.writeClaudeError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	SetActualOpenAIUpstreamEndpoint(c, vertexThirdPartyOpenAIEndpoint)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	buildReq := func(reqCtx context.Context) (*http.Request, string, error) {
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, bytes.NewReader(chatBody))
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
				return nil, err
			}
			if failoverErr := s.handleVertexServiceAccountNotFound(ctx, c, account, err); failoverErr != nil {
				return nil, failoverErr
			}
			return nil, s.writeClaudeError(c, http.StatusBadGateway, "upstream_error", err.Error())
		}
		requestIDHeader = idHeader

		resp, err = s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
		if err != nil {
			return nil, s.handleUpstreamTransportError(ctx, c, account, err)
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
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()

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
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
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

		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 64*1024)
		scanner.Buffer(buf, 4*1024*1024)

		state := apicompat.NewChatCompletionsToAnthropicStreamState(originalModel)

		for scanner.Scan() {
			line := scanner.Text()
			payload, ok := extractOpenAISSEDataLine(line)
			if !ok {
				continue
			}
			trimmed := strings.TrimSpace(payload)
			if trimmed == "[DONE]" {
				c.Writer.Flush()
				break
			}

			var chunk apicompat.ChatCompletionsChunk
			if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
				continue
			}

			if chunkUsage := extractCCStreamUsage(trimmed); chunkUsage != nil {
				usage.InputTokens = chunkUsage.InputTokens
				usage.OutputTokens = chunkUsage.OutputTokens
				usage.CacheReadInputTokens = chunkUsage.CacheReadInputTokens
			}

			if firstTokenMs == nil && len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != nil && *chunk.Choices[0].Delta.Content != "" {
				elapsed := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &elapsed
			}

			events := apicompat.ChatCompletionsChunkToAnthropicEvents(&chunk, state)
			for _, ev := range events {
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
			if clientDisconnect {
				break
			}
		}
	} else {
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, s.writeClaudeError(c, http.StatusBadGateway, "upstream_error", "Failed to read upstream response")
		}

		var ccResp apicompat.ChatCompletionsResponse
		if err := json.Unmarshal(respBody, &ccResp); err != nil {
			return nil, s.writeClaudeError(c, http.StatusBadGateway, "upstream_error", "Failed to parse upstream response")
		}

		anthropicResp := apicompat.ChatCompletionsResponseToAnthropic(&ccResp, originalModel)
		if anthropicResp == nil {
			return nil, s.writeClaudeError(c, http.StatusBadGateway, "upstream_error", "Failed to convert response to Anthropic format")
		}

		if parsedUsage, ok := openAIUsageFromGJSON(gjson.GetBytes(respBody, "usage")); ok {
			usage.InputTokens = parsedUsage.InputTokens
			usage.OutputTokens = parsedUsage.OutputTokens
			usage.CacheReadInputTokens = parsedUsage.CacheReadInputTokens
		}

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

func vertexResponsesReasoningInputStats(inputRaw json.RawMessage) (total int, plaintext int) {
	inputRaw = bytes.TrimSpace(inputRaw)
	if len(inputRaw) == 0 || inputRaw[0] != '[' {
		return 0, 0
	}
	var items []json.RawMessage
	if err := json.Unmarshal(inputRaw, &items); err != nil {
		return 0, 0
	}
	for _, raw := range items {
		_, text, ok := apicompat.ExtractResponsesReasoningItem(raw)
		if !ok {
			continue
		}
		total++
		if text != "" {
			plaintext++
		}
	}
	return total, plaintext
}

