package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const anthropicUpstreamResponse = `{"id":"msg_123","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"text","text":"hello from claude"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":6,"cache_read_input_tokens":4}}`

const anthropicUpstreamSSE = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-haiku-5-5\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n" +
	"event: content_block_start\n" +
	"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
	"event: content_block_stop\n" +
	"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

const anthropicModelPath = "publishers/anthropic/models/claude-haiku-5-5"

func TestVertexAnthropicHelpers(t *testing.T) {
	require.True(t, isVertexAnthropicMaaSModel("publishers/anthropic/models/claude-haiku-5-5"))
	require.False(t, isVertexAnthropicMaaSModel("publishers/meta/models/muse-ember-1.0"))
	require.False(t, isVertexAnthropicMaaSModel("publishers/deepseek-ai/models/deepseek-v4.1-flash-maas"))

	require.Equal(t, "claude-haiku-5-5", vertexAnthropicBareModel("publishers/anthropic/models/claude-haiku-5-5"))
	require.Equal(t, "claude-haiku-5-5", vertexAnthropicBareModel("anthropic/claude-haiku-5-5"))
}

func TestVertexAnthropic_ClaudeMessages_NonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, stub := metaTestService(metaJSONResponse(anthropicUpstreamResponse))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"` + anthropicModelPath + `","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))

	result, err := svc.Forward(context.Background(), c, account, reqBody)
	require.NoError(t, err)

	captured := stub.lastReq
	require.Equal(t, "https://us-central1-aiplatform.googleapis.com/v1/projects/gen-lang-client-0476190420/locations/us-central1/publishers/anthropic/models/claude-haiku-5-5:rawPredict", captured.URL.String())
	sentBody, _ := io.ReadAll(captured.Body)
	require.Equal(t, "vertex-2023-10-16", gjson.GetBytes(sentBody, "anthropic_version").String())
	require.False(t, gjson.GetBytes(sentBody, "model").Exists(), "model must be in URL, not body")
	require.True(t, gjson.GetBytes(sentBody, "messages").Exists())

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "message", gjson.GetBytes(rec.Body.Bytes(), "type").String())
	require.Equal(t, "hello from claude", gjson.GetBytes(rec.Body.Bytes(), "content.0.text").String())
	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 6, result.Usage.OutputTokens)
	require.Equal(t, 4, result.Usage.CacheReadInputTokens)
}

func TestVertexAnthropic_ClaudeMessages_Streaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, stub := metaTestService(metaSSEResponse(anthropicUpstreamSSE))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"` + anthropicModelPath + `","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))

	result, err := svc.Forward(context.Background(), c, account, reqBody)
	require.NoError(t, err)
	require.Contains(t, stub.lastReq.URL.String(), ":streamRawPredict")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "event: message_start")
	require.Contains(t, rec.Body.String(), "hello")
	require.Equal(t, 10, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
}

func TestVertexAnthropic_Responses_NonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, stub := metaTestService(metaJSONResponse(anthropicUpstreamResponse))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"` + anthropicModelPath + `","input":"hi"}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))

	result, err := svc.ForwardAsResponses(context.Background(), c, account, reqBody)
	require.NoError(t, err)

	captured := stub.lastReq
	require.Contains(t, captured.URL.Path, "/publishers/anthropic/models/claude-haiku-5-5:rawPredict")
	sentBody, _ := io.ReadAll(captured.Body)
	require.Equal(t, "vertex-2023-10-16", gjson.GetBytes(sentBody, "anthropic_version").String())
	require.False(t, gjson.GetBytes(sentBody, "model").Exists())

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "response", gjson.GetBytes(rec.Body.Bytes(), "object").String())
	require.Equal(t, "hello from claude", gjson.GetBytes(rec.Body.Bytes(), "output.0.content.0.text").String())
	require.Equal(t, 12, result.Usage.InputTokens)
}

func TestVertexAnthropic_Responses_Streaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := metaTestService(metaSSEResponse(anthropicUpstreamSSE))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"` + anthropicModelPath + `","input":"hi","stream":true}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))

	result, err := svc.ForwardAsResponses(context.Background(), c, account, reqBody)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "event: response.output_text.delta")
	require.Contains(t, rec.Body.String(), "hello")
	require.Contains(t, rec.Body.String(), "data: [DONE]")
	require.Equal(t, 10, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
}

func TestVertexAnthropic_ChatCompletions_NonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, stub := metaTestService(metaJSONResponse(anthropicUpstreamResponse))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"` + anthropicModelPath + `","messages":[{"role":"user","content":"hi"}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, reqBody)
	require.NoError(t, err)

	captured := stub.lastReq
	require.Contains(t, captured.URL.Path, ":rawPredict")
	sentBody, _ := io.ReadAll(captured.Body)
	require.Equal(t, "vertex-2023-10-16", gjson.GetBytes(sentBody, "anthropic_version").String())
	require.True(t, gjson.GetBytes(sentBody, "messages").Exists())

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "chat.completion", gjson.GetBytes(rec.Body.Bytes(), "object").String())
	require.Equal(t, "hello from claude", gjson.GetBytes(rec.Body.Bytes(), "choices.0.message.content").String())
	require.Equal(t, 12, result.Usage.InputTokens)
}

func TestVertexAnthropic_Native_NonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, stub := metaTestService(metaJSONResponse(anthropicUpstreamResponse))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/"+anthropicModelPath+":generateContent", bytes.NewReader(reqBody))

	result, err := svc.ForwardNative(context.Background(), c, account, anthropicModelPath, "generateContent", false, reqBody)
	require.NoError(t, err)
	require.Contains(t, stub.lastReq.URL.Path, ":rawPredict")
	require.Equal(t, http.StatusOK, rec.Code)
	var geminiResp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &geminiResp))
	require.Len(t, geminiResp["candidates"].([]any), 1)
	require.Equal(t, "hello from claude", gjson.GetBytes(rec.Body.Bytes(), "candidates.0.content.parts.0.text").String())
	require.Equal(t, 12, result.Usage.InputTokens)
}

func TestVertexAnthropic_MissingProjectID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := metaTestService(metaJSONResponse(anthropicUpstreamResponse))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "")
	reqBody := []byte(`{"model":"` + anthropicModelPath + `","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))

	result, err := svc.Forward(context.Background(), c, account, reqBody)
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "project_id")
}

func TestVertexAnthropic_MetaAndDeepSeekUnaffected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Meta still routes to the Meta Responses endpoint.
	svc, stub := metaTestService(metaJSONResponse(metaUpstreamResponse))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"publishers/meta/models/muse-ember-1.0","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, reqBody)
	require.NoError(t, err)
	require.Contains(t, stub.lastReq.URL.Path, "/publishers/meta/v1/responses")
}