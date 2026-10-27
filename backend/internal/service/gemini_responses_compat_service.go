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
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// ForwardAsResponses serves OpenAI Responses clients (Codex and friends)
// through Gemini accounts. It keeps the client-facing response in Responses
// format while routing the upstream call through Gemini native endpoints
// (generateContent / streamGenerateContent, or the Code Assist v1internal
// wrapper for OAuth accounts).
//
// The conversion chain mirrors the Chat Completions sibling
// (gemini_chat_completions_compat_service.go):
//
//	Responses → Anthropic (ResponsesToAnthropicRequest)
//	→ Gemini generateContent (convertClaudeMessagesToGeminiGenerateContent)
//	→ response: Gemini → Anthropic (convertGeminiToClaudeMessage)
//	            → Responses (AnthropicToResponsesResponse / event bridge)
//
// The streaming event bridge reuses the same Anthropic pseudo-event sequence
// that the Chat Completions sibling feeds into
// apicompat.AnthropicEventToResponsesEvents, so both chains emit identical
// Responses-stream semantics.
func (s *GeminiMessagesCompatService) ForwardAsResponses(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
) (*ForwardResult, error) {
	startTime := time.Now()

	// Normalize legacy Responses ingress shapes (same as the Anthropic
	// Responses path) before any conversion.
	if normalizedBody, normalized, err := normalizeOpenAIResponsesLegacyIngress(body); err != nil {
		return nil, writeResponsesCompatError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	} else if normalized {
		body = normalizedBody
	}

	// Lower Codex client-side tools to function tools understood by the
	// Anthropic intermediary (restored on the response side).
	adaptedBody, clientToolMapping, err := adaptResponsesClientToolsForAnthropic(body)
	if err != nil {
		return nil, writeResponsesCompatError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}

	var responsesReq apicompat.ResponsesRequest
	if err := json.Unmarshal(adaptedBody, &responsesReq); err != nil {
		return nil, writeResponsesCompatError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	}
	if strings.TrimSpace(responsesReq.Model) == "" {
		return nil, writeResponsesCompatError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
	}

	originalModel := responsesReq.Model
	clientStream := responsesReq.Stream

	mappedModel := originalModel
	if account.Type == AccountTypeAPIKey || account.Type == AccountTypeServiceAccount {
		mappedModel = account.GetMappedModel(originalModel)
	}
	// Vertex AI 上的第三方 MaaS 模型（如 DeepSeek）只提供 OpenAI 兼容端点，
	// 走原生的 ResponsesToChatCompletionsRequest 转换，不要使用 Anthropic 工具适配改写过的 body，
	// 避免 map 序列化随机顺序击穿前缀缓存。
	if account.IsVertexServiceAccount() && IsVertexThirdPartyMaaSModel(mappedModel) {
		return s.forwardVertexThirdPartyResponses(ctx, c, account, body, originalModel, mappedModel, clientToolMapping, startTime)
	}

	anthropicReq, err := apicompat.ResponsesToAnthropicRequest(&responsesReq)
	if err != nil {
		return nil, writeResponsesCompatError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	anthropicReq.Stream = clientStream

	claudeBody, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal responses compat request: %w", err)
	}

	return s.forwardClaudeBodyAsResponses(ctx, c, account, claudeBody, originalModel, clientStream, clientToolMapping, startTime)
}

func (s *GeminiMessagesCompatService) forwardClaudeBodyAsResponses(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	claudeBody []byte,
	originalModel string,
	clientStream bool,
	clientToolMapping apicompat.ResponsesClientToolMapping,
	startTime time.Time,
) (*ForwardResult, error) {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(claudeBody, &req); err != nil {
		return nil, writeResponsesCompatError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, writeResponsesCompatError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
	}

	mappedModel := req.Model
	if account.Type == AccountTypeAPIKey || account.Type == AccountTypeServiceAccount {
		mappedModel = account.GetMappedModel(req.Model)
	}

	geminiReq, err := convertClaudeMessagesToGeminiGenerateContent(claudeBody)
	if err != nil {
		return nil, writeResponsesCompatError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	geminiReq = ensureGeminiFunctionCallThoughtSignatures(geminiReq)
	logVertexPromptHistoryDriftForGeminiBody(c, account, "responses_gemini", mappedModel, geminiReq)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	useUpstreamStream := clientStream
	if account.Type == AccountTypeOAuth && !clientStream && strings.TrimSpace(account.GetCredential("project_id")) != "" {
		useUpstreamStream = true
	}

	buildReq, requestIDHeader := s.buildGeminiCompatUpstreamRequestFunc(
		account,
		mappedModel,
		geminiReq,
		clientStream,
		useUpstreamStream,
	)

	var resp *http.Response
	// checkedPolicy 是循环内对当前 resp 的错误策略检查结果，未检查时为 nil。
	var checkedPolicy *ErrorPolicyResult
	for attempt := 1; attempt <= geminiMaxRetries; attempt++ {
		checkedPolicy = nil
		upstreamReq, idHeader, err := buildReq(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, writeResponsesCompatError(c, http.StatusBadGateway, "upstream_error", err.Error())
		}
		requestIDHeader = idHeader

		resp, err = s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
		if err != nil {
			return nil, s.handleUpstreamTransportError(ctx, c, account, err)
		}

		checked, rebuilt := s.checkErrorPolicyInLoop(ctx, account, resp, mappedModel)
		resp = rebuilt
		checkedPolicy = checked
		if checked != nil && *checked != ErrorPolicyNone {
			break
		}

		if resp.StatusCode >= 400 && s.shouldRetryGeminiUpstreamError(account, resp.StatusCode) {
			respBody := s.readUpstreamErrorBody(resp)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusForbidden && isGeminiInsufficientScope(resp.Header, respBody) {
				resp = &http.Response{
					StatusCode: resp.StatusCode,
					Header:     resp.Header.Clone(),
					Body:       io.NopCloser(bytes.NewReader(respBody)),
				}
				break
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				s.handleGeminiUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
			}
			if attempt < geminiMaxRetries {
				upstreamReqID := resp.Header.Get(requestIDHeader)
				if upstreamReqID == "" {
					upstreamReqID = resp.Header.Get("x-goog-request-id")
				}
				upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
				upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					ProxyID:            opsUpstreamProxyID(account),
					ProxyName:          opsUpstreamProxyName(account),
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamRequestID:  upstreamReqID,
					Kind:               "retry",
					Message:            upstreamMsg,
				})
				logger.LegacyPrintf("service.gemini_responses", "Gemini account %d: upstream status %d, retry %d/%d", account.ID, resp.StatusCode, attempt, geminiMaxRetries)
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
	defer func() { _ = resp.Body.Close() }()

	requestID := resp.Header.Get(requestIDHeader)
	if requestID == "" {
		requestID = resp.Header.Get("x-goog-request-id")
	}
	if requestID != "" {
		c.Header("x-request-id", requestID)
	}

	reasoningEffort := extractGeminiReasoningEffortFromBody(geminiReq)

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		policy := ErrorPolicyNone
		if s.rateLimitService != nil {
			policy = s.finalErrorPolicy(ctx, account, resp.StatusCode, respBody, mappedModel, checkedPolicy)
		}
		// Same account-state handling contract as the messages and chat
		// completions compat layers: only None / Matched reach rate-limit
		// bookkeeping; Skipped / TempUnscheduled are decisions already made
		// by the policy layer.
		if policy == ErrorPolicyNone || policy == ErrorPolicyMatched {
			s.handleGeminiUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
		}
		evBody := unwrapIfNeeded(account.Type == AccountTypeOAuth, respBody)

		if s.shouldFailoverGeminiUpstreamError(resp.StatusCode) {
			upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(evBody)))
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:            opsUpstreamProxyID(account),
				ProxyName:          opsUpstreamProxyName(account),
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  requestID,
				Kind:               "failover",
				Message:            upstreamMsg,
			})
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           evBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}

		if policy == ErrorPolicySkipped && account.IsCustomErrorCodesEnabled() {
			return nil, s.writeGeminiCustomCodeSkippedError(c, account, resp.StatusCode, requestID, evBody, func() {
				_ = writeResponsesCompatError(c, http.StatusInternalServerError, "api_error", geminiCustomCodeSkippedClientMessage)
			})
		}
		return nil, s.writeGeminiResponsesMappedError(c, account, resp.StatusCode, requestID, evBody)
	}

	var usage *ClaudeUsage
	var firstTokenMs *int
	if clientStream {
		streamRes, err := s.handleResponsesStreamingResponseFromGemini(c, resp, startTime, originalModel, clientToolMapping, account.Type == AccountTypeOAuth)
		if err != nil {
			return nil, err
		}
		usage = streamRes.usage
		firstTokenMs = streamRes.firstTokenMs
	} else if useUpstreamStream {
		collected, usageObj, err := collectGeminiSSE(resp.Body, account.Type == AccountTypeOAuth)
		if err != nil {
			return nil, writeResponsesCompatError(c, http.StatusBadGateway, "upstream_error", "Failed to read upstream stream")
		}
		collectedBytes, _ := json.Marshal(collected)
		responsesResp, usageObj2, err := geminiResponseToResponses(collected, originalModel, collectedBytes, usageObj)
		if err != nil {
			return nil, writeResponsesCompatError(c, http.StatusBadGateway, "upstream_error", "Failed to parse upstream response")
		}
		writeResponsesJSON(c, s, resp, responsesResp, clientToolMapping)
		usage = usageObj2
	} else {
		usageResp, err := s.handleResponsesNonStreamingResponseFromGemini(c, resp, originalModel, clientToolMapping, account.Type == AccountTypeOAuth)
		if err != nil {
			return nil, err
		}
		usage = usageResp
	}

	if usage == nil {
		usage = &ClaudeUsage{}
	}

	imageCount := 0
	imageInputSize := s.extractImageInputSize(claudeBody)
	imageSize := normalizeOpenAIImageSizeTier(imageInputSize)
	if isImageGenerationModel(originalModel) {
		imageCount = 1
	}

	return &ForwardResult{
		RequestID:        requestID,
		UpstreamHeaders:  resp.Header,
		Usage:            *usage,
		Model:            originalModel,
		UpstreamModel:    mappedModel,
		Stream:           clientStream,
		Duration:         time.Since(startTime),
		FirstTokenMs:     firstTokenMs,
		ReasoningEffort:  reasoningEffort,
		ImageCount:       imageCount,
		ImageSize:        imageSize,
		ImageInputSize:   imageInputSize,
		ClientDisconnect: false,
	}, nil
}

// writeResponsesCompatError writes a Responses-formatted error response and
// returns an error, keeping the "return nil, err" shape shared by the Gemini
// compat layers (see writeChatCompletionsError).
func writeResponsesCompatError(c *gin.Context, status int, code, message string) error {
	writeResponsesError(c, status, code, message)
	return fmt.Errorf("%s", message)
}

// writeResponsesJSON writes a buffered (non-streaming) Responses response with
// the same header filtering and Codex client-tool restoration the Anthropic
// Responses path applies.
func writeResponsesJSON(
	c *gin.Context,
	s *GeminiMessagesCompatService,
	resp *http.Response,
	responsesResp *apicompat.ResponsesResponse,
	clientToolMapping apicompat.ResponsesClientToolMapping,
) {
	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	// Upstream streamed responses carry text/event-stream; the client asked
	// for a single JSON document, so force the JSON content type like the
	// Anthropic Responses path does.
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	respBytes, err := json.Marshal(responsesResp)
	if err != nil {
		c.JSON(http.StatusOK, responsesResp)
		return
	}
	respBytes = reverseToolNamesIfPresent(c, respBytes)
	if restored, _, err := apicompat.RestoreResponsesClientToolPayload(respBytes, clientToolMapping); err == nil {
		respBytes = restored
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", respBytes)
}

func (s *GeminiMessagesCompatService) handleResponsesNonStreamingResponseFromGemini(
	c *gin.Context,
	resp *http.Response,
	originalModel string,
	clientToolMapping apicompat.ResponsesClientToolMapping,
	isOAuth bool,
) (*ClaudeUsage, error) {
	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	if isOAuth {
		if unwrappedBody, uwErr := unwrapGeminiResponse(respBody); uwErr == nil {
			respBody = unwrappedBody
		}
	}

	var geminiResp map[string]any
	if err := json.Unmarshal(respBody, &geminiResp); err != nil {
		return nil, writeResponsesCompatError(c, http.StatusBadGateway, "upstream_error", "Failed to parse upstream response")
	}

	responsesResp, usage, err := geminiResponseToResponses(geminiResp, originalModel, respBody, nil)
	if err != nil {
		return nil, writeResponsesCompatError(c, http.StatusBadGateway, "upstream_error", "Failed to parse upstream response")
	}

	writeResponsesJSON(c, s, resp, responsesResp, clientToolMapping)
	return usage, nil
}

// geminiResponseToResponses converts a Gemini generateContent response into a
// Responses API response through the shared Anthropic intermediary, matching
// the geminiResponseToChatCompletions sibling.
func geminiResponseToResponses(
	geminiResp map[string]any,
	originalModel string,
	rawData []byte,
	usageOverride *ClaudeUsage,
) (*apicompat.ResponsesResponse, *ClaudeUsage, error) {
	claudeRespMap, usage := convertGeminiToClaudeMessage(geminiResp, originalModel, rawData, true)
	if usageOverride != nil && (usageOverride.InputTokens > 0 || usageOverride.OutputTokens > 0 || usageOverride.CacheReadInputTokens > 0) {
		usage = usageOverride
		if usageMap, ok := claudeRespMap["usage"].(map[string]any); ok {
			usageMap["input_tokens"] = usage.InputTokens
			usageMap["output_tokens"] = usage.OutputTokens
			usageMap["cache_read_input_tokens"] = usage.CacheReadInputTokens
		}
	}

	claudeBytes, err := json.Marshal(claudeRespMap)
	if err != nil {
		return nil, nil, err
	}
	var anthropicResp apicompat.AnthropicResponse
	if err := json.Unmarshal(claudeBytes, &anthropicResp); err != nil {
		return nil, nil, err
	}
	responsesResp := apicompat.AnthropicToResponsesResponse(&anthropicResp)
	return responsesResp, usage, nil
}

// handleResponsesStreamingResponseFromGemini bridges the upstream Gemini SSE
// stream into Responses SSE events. The Gemini chunk state machine is copied
// verbatim from the chat completions sibling (same pseudo Anthropic event
// sequence: text blocks, tool_use blocks, stop reason, usage); only the final
// hop differs — Anthropic events are rendered directly as Responses SSE
// instead of being converted to chat chunks.
func (s *GeminiMessagesCompatService) handleResponsesStreamingResponseFromGemini(
	c *gin.Context,
	resp *http.Response,
	startTime time.Time,
	originalModel string,
	clientToolMapping apicompat.ResponsesClientToolMapping,
	isOAuth bool,
) (*geminiStreamResult, error) {
	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}

	anthState := apicompat.NewAnthropicEventToResponsesState()
	anthState.Model = originalModel
	clientToolRestorer := apicompat.NewResponsesClientToolStreamRestorer(clientToolMapping)

	var usage ClaudeUsage
	var firstTokenMs *int
	firstChunk := true

	writeResponsesEvent := func(resEvt apicompat.ResponsesStreamEvent) bool {
		payload, err := json.Marshal(resEvt)
		if err != nil {
			return false
		}
		payload = reverseToolNamesIfPresent(c, payload)
		payloads, _, err := clientToolRestorer.RestoreEvent(payload)
		if err != nil {
			return false
		}
		for _, restored := range payloads {
			eventType := gjson.GetBytes(restored, "type").String()
			if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", eventType, restored); err != nil {
				return true
			}
		}
		return false
	}

	emitAnthropicEvent := func(evt *apicompat.AnthropicStreamEvent) bool {
		for _, resEvt := range apicompat.AnthropicEventToResponsesEvents(evt, anthState) {
			if writeResponsesEvent(resEvt) {
				return true
			}
		}
		flusher.Flush()
		return false
	}

	messageID := generateAnthropicMsgID()
	if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{
		Type: "message_start",
		Message: &apicompat.AnthropicResponse{
			ID:         messageID,
			Type:       "message",
			Role:       "assistant",
			Model:      originalModel,
			Content:    []apicompat.AnthropicContentBlock{},
			StopReason: nil, // JSON null
			Usage:      apicompat.AnthropicUsage{},
		},
	}) {
		return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
	}

	finishReason := ""
	sawToolUse := false
	nextBlockIndex := 0
	openBlockIndex := -1
	openBlockType := ""
	seenText := ""
	openToolIndex := -1
	openToolName := ""
	seenToolJSON := ""

	closeOpenBlock := func() bool {
		if openBlockIndex < 0 {
			return false
		}
		disconnected := emitAnthropicEvent(&apicompat.AnthropicStreamEvent{Type: "content_block_stop"})
		openBlockIndex = -1
		openBlockType = ""
		return disconnected
	}
	closeOpenTool := func() bool {
		if openToolIndex < 0 {
			return false
		}
		disconnected := emitAnthropicEvent(&apicompat.AnthropicStreamEvent{Type: "content_block_stop"})
		openToolIndex = -1
		openToolName = ""
		seenToolJSON = ""
		return disconnected
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload != "" && payload != "[DONE]" {
					rawBytes := []byte(payload)
					if isOAuth {
						if innerBytes, uwErr := unwrapGeminiResponse(rawBytes); uwErr == nil {
							rawBytes = innerBytes
						}
					}

					var geminiResp map[string]any
					if err := json.Unmarshal(rawBytes, &geminiResp); err == nil {
						if firstChunk {
							firstChunk = false
							ms := int(time.Since(startTime).Milliseconds())
							firstTokenMs = &ms
						}
						if fr := extractGeminiFinishReason(geminiResp); fr != "" {
							finishReason = fr
						}
						if u := extractGeminiUsage(rawBytes); u != nil {
							usage = *u
						}

						for _, part := range extractGeminiParts(geminiResp) {
							if text, ok := part["text"].(string); ok && text != "" {
								if openToolIndex >= 0 {
									if closeOpenTool() {
										return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
									}
								}
								delta, newSeen := computeGeminiTextDelta(seenText, text)
								seenText = newSeen
								if delta == "" {
									continue
								}
								if openBlockType != "text" {
									if closeOpenBlock() {
										return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
									}
									idx := nextBlockIndex
									nextBlockIndex++
									openBlockIndex = idx
									openBlockType = "text"
									if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{
										Type:  "content_block_start",
										Index: &idx,
										ContentBlock: &apicompat.AnthropicContentBlock{
											Type: "text",
											Text: "",
										},
									}) {
										return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
									}
								}
								if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{
									Type: "content_block_delta",
									Delta: &apicompat.AnthropicDelta{
										Type: "text_delta",
										Text: delta,
									},
								}) {
									return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
								}
								continue
							}

							if fc, ok := part["functionCall"].(map[string]any); ok && fc != nil {
								name, _ := fc["name"].(string)
								if strings.TrimSpace(name) == "" {
									name = "tool"
								}
								if closeOpenBlock() {
									return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
								}
								if openToolIndex >= 0 && openToolName != name {
									if closeOpenTool() {
										return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
									}
								}
								if openToolIndex < 0 {
									idx := nextBlockIndex
									nextBlockIndex++
									openToolIndex = idx
									openToolName = name
									sawToolUse = true
									if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{
										Type:  "content_block_start",
										Index: &idx,
										ContentBlock: &apicompat.AnthropicContentBlock{
											Type:  "tool_use",
											ID:    "toolu_" + randomHex(8),
											Name:  name,
											Input: json.RawMessage(`{}`),
										},
									}) {
										return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
									}
								}

								argsJSONText := "{}"
								switch v := fc["args"].(type) {
								case nil:
								case string:
									if strings.TrimSpace(v) != "" {
										argsJSONText = v
									}
								default:
									if b, err := json.Marshal(v); err == nil && len(b) > 0 {
										argsJSONText = string(b)
									}
								}
								delta, newSeen := computeGeminiTextDelta(seenToolJSON, argsJSONText)
								seenToolJSON = newSeen
								if delta != "" {
									if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{
										Type: "content_block_delta",
										Delta: &apicompat.AnthropicDelta{
											Type:        "input_json_delta",
											PartialJSON: delta,
										},
									}) {
										return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
									}
								}
							}
						}
					}
				}
			}
		}

		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("stream read error: %w", err)
		}
	}

	if closeOpenBlock() {
		return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
	}
	if closeOpenTool() {
		return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
	}

	stopReason := mapGeminiFinishReasonToClaudeStopReason(finishReason)
	if sawToolUse {
		stopReason = "tool_use"
	}
	anthState.InputTokens = usage.InputTokens
	anthState.CacheReadInputTokens = usage.CacheReadInputTokens
	if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{
		Type: "message_delta",
		Delta: &apicompat.AnthropicDelta{
			Type:       "message_delta",
			StopReason: stopReason,
		},
		Usage: &apicompat.AnthropicUsage{
			InputTokens:          usage.InputTokens,
			OutputTokens:         usage.OutputTokens,
			CacheReadInputTokens: usage.CacheReadInputTokens,
		},
	}) {
		return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
	}
	if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{Type: "message_stop"}) {
		return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
	}

	for _, resEvt := range apicompat.FinalizeAnthropicResponsesStream(anthState) {
		if writeResponsesEvent(resEvt) {
			return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
		}
	}
	flusher.Flush()

	return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs}, nil
}

func (s *GeminiMessagesCompatService) writeGeminiResponsesMappedError(
	c *gin.Context,
	account *Account,
	upstreamStatus int,
	upstreamRequestID string,
	body []byte,
) error {
	upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	setOpsUpstreamError(c, upstreamStatus, upstreamMsg, "")
	if account != nil {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            opsUpstreamProxyID(account),
			ProxyName:          opsUpstreamProxyName(account),
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: upstreamStatus,
			UpstreamRequestID:  upstreamRequestID,
			Kind:               "http_error",
			Message:            upstreamMsg,
		})
	}

	if status, errType, errMsg, matched := applyErrorPassthroughRule(
		c,
		PlatformGemini,
		upstreamStatus,
		body,
		http.StatusBadGateway,
		"upstream_error",
		"Upstream request failed",
	); matched {
		return writeResponsesCompatError(c, status, errType, errMsg)
	}

	statusCode := http.StatusBadGateway
	errType := "upstream_error"
	errMsg := "Upstream request failed"
	if mapped := mapGeminiErrorBodyToClaudeError(body); mapped != nil {
		if mapped.Type != "" {
			errType = mapped.Type
		}
		if mapped.Message != "" {
			errMsg = mapped.Message
		}
		if mapped.StatusCode > 0 {
			statusCode = mapped.StatusCode
		}
	}

	switch upstreamStatus {
	case http.StatusBadRequest:
		if statusCode == http.StatusBadGateway {
			statusCode = http.StatusBadRequest
		}
		if errType == "upstream_error" {
			errType = "invalid_request_error"
		}
		// 400 is a deterministic request error: return the sanitized upstream
		// message so the client can locate the offending field.
		if errMsg == "Upstream request failed" {
			if upstreamMsg != "" {
				errMsg = upstreamMsg
			} else {
				errMsg = "Invalid request"
			}
		}
	case http.StatusNotFound:
		statusCode = http.StatusNotFound
		if errType == "upstream_error" {
			errType = "not_found_error"
		}
		if errMsg == "Upstream request failed" {
			errMsg = "Resource not found"
		}
	case http.StatusTooManyRequests:
		statusCode = http.StatusTooManyRequests
		if errType == "upstream_error" {
			errType = "rate_limit_error"
		}
		if errMsg == "Upstream request failed" {
			errMsg = "Upstream rate limit exceeded, please retry later"
		}
	case 529:
		statusCode = http.StatusServiceUnavailable
		if errType == "upstream_error" {
			errType = "overloaded_error"
		}
		if errMsg == "Upstream request failed" {
			errMsg = "Upstream service overloaded, please retry later"
		}
	}

	if upstreamMsg != "" && errMsg == "Upstream request failed" {
		errMsg = upstreamMsg
	}
	return writeResponsesCompatError(c, statusCode, errType, errMsg)
}

func (s *GeminiMessagesCompatService) reasoningContentByID(itemID string) string {
	if s == nil || s.cache == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	content, err := s.cache.GetReasoningContent(ctx, itemID)
	if err != nil {
		return ""
	}
	return content
}

func (s *GeminiMessagesCompatService) setReasoningContent(itemID, content string) {
	if s == nil || s.cache == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.cache.SetReasoningContent(ctx, itemID, content, responsesReasoningCacheTTL); err != nil {
		logger.L().Warn("gemini responses vertex maas: cache reasoning content failed",
			zap.Error(err),
			zap.String("item_id", itemID),
		)
	}
}

func (s *GeminiMessagesCompatService) recacheReasoningItemsFromInput(inputRaw json.RawMessage) {
	if s == nil || s.cache == nil {
		return
	}
	inputRaw = bytes.TrimSpace(inputRaw)
	if len(inputRaw) == 0 || inputRaw[0] != '[' {
		return
	}
	var items []json.RawMessage
	if err := json.Unmarshal(inputRaw, &items); err != nil {
		return
	}
	for _, raw := range items {
		id, text, ok := apicompat.ExtractResponsesReasoningItem(raw)
		if !ok || id == "" || text == "" {
			continue
		}
		s.setReasoningContent(id, text)
	}
}

func (s *GeminiMessagesCompatService) cacheReasoningItemsFromEvents(events []apicompat.ResponsesStreamEvent) {
	for _, event := range events {
		if event.Type != "response.output_item.done" || event.Item == nil {
			continue
		}
		s.cacheReasoningItem(event.Item)
	}
}

func (s *GeminiMessagesCompatService) cacheReasoningItemsFromOutput(output []apicompat.ResponsesOutput) {
	for i := range output {
		s.cacheReasoningItem(&output[i])
	}
}

func (s *GeminiMessagesCompatService) cacheReasoningItem(item *apicompat.ResponsesOutput) {
	if item == nil || item.Type != "reasoning" || item.ID == "" {
		return
	}
	var parts []string
	for _, sum := range item.Summary {
		if t := strings.TrimSpace(sum.Text); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return
	}
	s.setReasoningContent(item.ID, strings.Join(parts, "\n"))
}
