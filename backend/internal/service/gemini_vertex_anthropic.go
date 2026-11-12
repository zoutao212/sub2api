package service

import (
	"bufio"
	"context"
	"encoding/json"
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

// Anthropic MaaS models on Vertex (e.g. claude-haiku-5-5) are served via the
// publisher rawPredict route (.../publishers/anthropic/models/{model}:rawPredict
// or :streamRawPredict) using the Anthropic Messages wire protocol
// (anthropic_version=vertex-2023-10-16, model in the URL not the body). The
// OpenAI-compatible endpoints/openapi/chat/completions route rejects them with
// "not servable in region global", so they get their own branch here. URL and
// body helpers are reused from the upstream anthropic-vertex path
// (buildVertexAnthropicURL / buildVertexAnthropicRequestBody).

const vertexAnthropicEndpointLabel = "/publishers/anthropic:rawPredict"

func isVertexAnthropicMaaSModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "publishers/anthropic/")
}

// vertexAnthropicBareModel extracts the bare model id used in the rawPredict URL:
//
//	publishers/anthropic/models/claude-haiku-5-5 -> claude-haiku-5-5
func vertexAnthropicBareModel(model string) string {
	trimmed := strings.TrimSpace(model)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "publishers/anthropic/models/") {
		return trimmed[len("publishers/anthropic/models/"):]
	}
	if strings.HasPrefix(lower, "anthropic/") {
		return trimmed[len("anthropic/"):]
	}
	return trimmed
}

func claudeUsageFromAnthropicUsage(u apicompat.AnthropicUsage) ClaudeUsage {
	return ClaudeUsage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
	}
}

func updateClaudeUsageFromAnthropicEvent(evt *apicompat.AnthropicStreamEvent, usage *ClaudeUsage) {
	if evt.Type == "message_start" && evt.Message != nil {
		usage.InputTokens = evt.Message.Usage.InputTokens
		usage.CacheReadInputTokens = evt.Message.Usage.CacheReadInputTokens
		usage.CacheCreationInputTokens = evt.Message.Usage.CacheCreationInputTokens
	}
	if evt.Type == "message_delta" && evt.Usage != nil {
		usage.OutputTokens = evt.Usage.OutputTokens
	}
}

func (s *GeminiMessagesCompatService) vertexAnthropicTargetURL(
	c *gin.Context, account *Account, mappedModel string, stream bool,
	writeErr func(int, string, string) error,
) (string, string, error) {
	projectID := account.VertexProjectID()
	if strings.TrimSpace(projectID) == "" {
		return "", "", writeErr(http.StatusBadRequest, "invalid_request_error",
			"Vertex third-party models require project_id to be configured on the account")
	}
	location := account.VertexLocation(mappedModel)
	bareModel := normalizeVertexAnthropicModelID(vertexAnthropicBareModel(mappedModel))
	targetURL, err := buildVertexAnthropicURL(projectID, location, bareModel, stream)
	if err != nil {
		return "", "", writeErr(http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	SetActualOpenAIUpstreamEndpoint(c, vertexAnthropicEndpointLabel)
	return targetURL, bareModel, nil
}

// scanVertexAnthropicSSE reads upstream Anthropic SSE events.
func scanVertexAnthropicSSE(body io.Reader, onEvent func(evt *apicompat.AnthropicStreamEvent)) error {
	scanner := bufio.NewScanner(body)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)
	for scanner.Scan() {
		payload, ok := extractAnthropicSSEDataLine(scanner.Text())
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(payload)
		if trimmed == "" || trimmed == "[DONE]" {
			continue
		}
		var evt apicompat.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(trimmed), &evt); err != nil {
			continue
		}
		onEvent(&evt)
	}
	return scanner.Err()
}

func (s *GeminiMessagesCompatService) readAnthropicResponse(resp *http.Response, writeErr func(int, string, string) error) (*apicompat.AnthropicResponse, error) {
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, writeErr(http.StatusBadGateway, "upstream_error", "Failed to read upstream response")
	}
	var ar apicompat.AnthropicResponse
	if err := json.Unmarshal(respBody, &ar); err != nil {
		return nil, writeErr(http.StatusBadGateway, "upstream_error", "Failed to parse upstream response")
	}
	return &ar, nil
}

func (s *GeminiMessagesCompatService) logVertexAnthropicError(account *Account, status int, targetURL, upstreamModel string, respBody []byte) {
	logger.L().Warn("vertex anthropic upstream error",
		zap.Int64("account_id", account.ID), zap.Int("status_code", status),
		zap.String("target_url", targetURL), zap.String("upstream_model", upstreamModel),
		zap.String("upstream_resp", string(respBody)))
}

// ---------------------------------------------------------------------------
// /v1/messages (Claude) -> Anthropic rawPredict (passthrough)
// ---------------------------------------------------------------------------

func (s *GeminiMessagesCompatService) forwardVertexAnthropicClaudeMessages(
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
	targetURL, upstreamModel, err := s.vertexAnthropicTargetURL(c, account, mappedModel, clientStream, writeErr)
	if err != nil {
		return nil, err
	}
	wireBody, err := buildVertexAnthropicRequestBody(body)
	if err != nil {
		return nil, writeErr(http.StatusBadRequest, "invalid_request_error", err.Error())
	}

	resp, requestID, err := s.vertexMaaSRoundTrip(ctx, c, account, targetURL, wireBody, writeErr)
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
		s.logVertexAnthropicError(account, resp.StatusCode, targetURL, upstreamModel, respBody)
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
		for scanner.Scan() {
			line := scanner.Text()
			if data, ok := extractAnthropicSSEDataLine(line); ok {
				var evt apicompat.AnthropicStreamEvent
				if json.Unmarshal([]byte(strings.TrimSpace(data)), &evt) == nil {
					updateClaudeUsageFromAnthropicEvent(&evt, &usage)
					if firstTokenMs == nil && evt.Type == "content_block_delta" && evt.Delta != nil && evt.Delta.Text != "" {
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
		ar, err := s.readAnthropicResponse(resp, writeErr)
		if err != nil {
			return nil, err
		}
		usage = claudeUsageFromAnthropicUsage(ar.Usage)
		ar.Model = originalModel
		c.JSON(http.StatusOK, ar)
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
// /v1/responses -> Anthropic rawPredict -> Responses
// ---------------------------------------------------------------------------

func (s *GeminiMessagesCompatService) forwardVertexAnthropicResponses(
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
	targetURL, upstreamModel, err := s.vertexAnthropicTargetURL(c, account, mappedModel, clientStream, writeErr)
	if err != nil {
		return nil, err
	}
	anthropicReq, err := apicompat.ResponsesToAnthropicRequest(&responsesReq)
	if err != nil {
		return nil, writeErr(http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	anthropicBody, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}
	wireBody, err := buildVertexAnthropicRequestBody(anthropicBody)
	if err != nil {
		return nil, writeErr(http.StatusBadRequest, "invalid_request_error", err.Error())
	}

	resp, requestID, err := s.vertexMaaSRoundTrip(ctx, c, account, targetURL, wireBody, writeErr)
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
		s.logVertexAnthropicError(account, resp.StatusCode, targetURL, upstreamModel, respBody)
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

		state := apicompat.NewAnthropicEventToResponsesState()
		restorer := apicompat.NewResponsesClientToolStreamRestorer(clientToolMapping)
		writeEvents := func(events []apicompat.ResponsesStreamEvent) {
			if clientDisconnect {
				return
			}
			for _, evt := range events {
				if evt.Response != nil && evt.Response.Model != "" {
					evt.Response.Model = originalModel
				}
				payload, err := json.Marshal(evt)
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
		scanErr := scanVertexAnthropicSSE(resp.Body, func(evt *apicompat.AnthropicStreamEvent) {
			updateClaudeUsageFromAnthropicEvent(evt, &usage)
			if firstTokenMs == nil && evt.Type == "content_block_delta" && evt.Delta != nil && evt.Delta.Text != "" {
				elapsed := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &elapsed
			}
			writeEvents(apicompat.AnthropicEventToResponsesEvents(evt, state))
		})
		if scanErr != nil {
			return nil, fmt.Errorf("vertex anthropic responses stream read error: %w", scanErr)
		}
		writeEvents(apicompat.FinalizeAnthropicResponsesStream(state))
		if !clientDisconnect {
			if _, err := fmt.Fprint(c.Writer, "data: [DONE]\n\n"); err != nil {
				clientDisconnect = true
			} else {
				c.Writer.Flush()
			}
		}
	} else {
		ar, err := s.readAnthropicResponse(resp, writeErr)
		if err != nil {
			return nil, err
		}
		usage = claudeUsageFromAnthropicUsage(ar.Usage)
		responsesResp := apicompat.AnthropicToResponsesResponse(ar)
		responsesResp.Model = originalModel
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

// ---------------------------------------------------------------------------
// /v1/chat/completions -> (Responses) -> Anthropic rawPredict -> chat.completion
// ---------------------------------------------------------------------------

func (s *GeminiMessagesCompatService) forwardVertexAnthropicChatCompletions(
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
	targetURL, upstreamModel, err := s.vertexAnthropicTargetURL(c, account, mappedModel, clientStream, writeErr)
	if err != nil {
		return nil, err
	}
	wireBody, err := vertexAnthropicWireBodyFromChat(&chatReq)
	if err != nil {
		return nil, writeErr(http.StatusBadRequest, "invalid_request_error", err.Error())
	}

	resp, requestID, err := s.vertexMaaSRoundTrip(ctx, c, account, targetURL, wireBody, writeErr)
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
		s.logVertexAnthropicError(account, resp.StatusCode, targetURL, upstreamModel, respBody)
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

		anthState := apicompat.NewAnthropicEventToResponsesState()
		chatState := apicompat.NewResponsesEventToChatState()
		chatState.Model = originalModel
		chatState.IncludeUsage = true
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
		scanErr := scanVertexAnthropicSSE(resp.Body, func(evt *apicompat.AnthropicStreamEvent) {
			updateClaudeUsageFromAnthropicEvent(evt, &usage)
			if firstTokenMs == nil && evt.Type == "content_block_delta" && evt.Delta != nil && evt.Delta.Text != "" {
				elapsed := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &elapsed
			}
			for _, revt := range apicompat.AnthropicEventToResponsesEvents(evt, anthState) {
				for _, chunk := range apicompat.ResponsesEventToChatChunks(&revt, chatState) {
					writeChunk(chunk)
				}
			}
		})
		if scanErr != nil {
			return nil, fmt.Errorf("vertex anthropic chat_completions stream read error: %w", scanErr)
		}
		for _, revt := range apicompat.FinalizeAnthropicResponsesStream(anthState) {
			for _, chunk := range apicompat.ResponsesEventToChatChunks(&revt, chatState) {
				writeChunk(chunk)
			}
		}
		for _, chunk := range apicompat.FinalizeResponsesChatStream(chatState) {
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
		ar, err := s.readAnthropicResponse(resp, writeErr)
		if err != nil {
			return nil, err
		}
		usage = claudeUsageFromAnthropicUsage(ar.Usage)
		responsesResp := apicompat.AnthropicToResponsesResponse(ar)
		ccResp := apicompat.ResponsesToChatCompletions(responsesResp, originalModel)
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
// Gemini native (:generateContent) -> Anthropic rawPredict -> Gemini response
// ---------------------------------------------------------------------------

func (s *GeminiMessagesCompatService) forwardVertexAnthropicNative(
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
	targetURL, upstreamModel, err := s.vertexAnthropicTargetURL(c, account, mappedModel, stream, writeErr)
	if err != nil {
		return nil, err
	}
	chatReq := vertexMetaGeminiBodyToChatRequest(body, upstreamModel, stream)
	wireBody, err := vertexAnthropicWireBodyFromChat(chatReq)
	if err != nil {
		return nil, s.writeGoogleError(c, http.StatusBadRequest, err.Error())
	}

	resp, requestID, err := s.vertexMaaSRoundTrip(ctx, c, account, targetURL, wireBody, writeErr)
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
		s.logVertexAnthropicError(account, resp.StatusCode, targetURL, upstreamModel, respBody)
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

		anthState := apicompat.NewAnthropicEventToResponsesState()
		chatState := apicompat.NewResponsesEventToChatState()
		chatState.Model = upstreamModel
		scanErr := scanVertexAnthropicSSE(resp.Body, func(evt *apicompat.AnthropicStreamEvent) {
			updateClaudeUsageFromAnthropicEvent(evt, &usage)
			for _, revt := range apicompat.AnthropicEventToResponsesEvents(evt, anthState) {
				for _, chunk := range apicompat.ResponsesEventToChatChunks(&revt, chatState) {
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
			}
		})
		if scanErr != nil {
			return nil, fmt.Errorf("vertex anthropic native stream read error: %w", scanErr)
		}
	} else {
		ar, err := s.readAnthropicResponse(resp, writeErr)
		if err != nil {
			return nil, err
		}
		usage = claudeUsageFromAnthropicUsage(ar.Usage)
		responsesResp := apicompat.AnthropicToResponsesResponse(ar)
		ccResp := apicompat.ResponsesToChatCompletions(responsesResp, upstreamModel)
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

// vertexAnthropicWireBodyFromChat converts a Chat Completions request to the
// Anthropic Messages wire body via the Responses intermediate representation
// (no direct CC->Anthropic converter exists), then applies the Vertex body
// fixups (drop model, set anthropic_version).
func vertexAnthropicWireBodyFromChat(chatReq *apicompat.ChatCompletionsRequest) ([]byte, error) {
	responsesReq, err := apicompat.ChatCompletionsToResponses(chatReq)
	if err != nil {
		return nil, err
	}
	anthropicReq, err := apicompat.ResponsesToAnthropicRequest(responsesReq)
	if err != nil {
		return nil, err
	}
	anthropicBody, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}
	return buildVertexAnthropicRequestBody(anthropicBody)
}