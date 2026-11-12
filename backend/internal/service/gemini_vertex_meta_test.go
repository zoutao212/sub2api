package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const metaUpstreamResponse = `{"id":"resp_123","object":"response","model":"muse-ember-1.0","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello from meta"}]}],"usage":{"input_tokens":12,"output_tokens":6,"total_tokens":18,"input_tokens_details":{"cached_tokens":4}}}`

const metaUpstreamSSE = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_123\",\"object\":\"response\",\"model\":\"muse-ember-1.0\",\"status\":\"in_progress\"}}\n\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hello\"}\n\n" +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_123\",\"object\":\"response\",\"model\":\"muse-ember-1.0\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n" +
	"data: [DONE]\n\n"

func metaTestService(resp *http.Response) (*GeminiMessagesCompatService, *geminiCompatHTTPUpstreamStub) {
	stub := &geminiCompatHTTPUpstreamStub{response: resp}
	return &GeminiMessagesCompatService{httpUpstream: stub, cfg: &config.Config{}}, stub
}

func metaJSONResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"meta-req-1"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}
}

func metaSSEResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestVertexMetaHelpers(t *testing.T) {
	require.True(t, isVertexMetaMaaSModel("publishers/meta/models/muse-ember-1.0"))
	require.True(t, isVertexMetaMaaSModel("meta/muse-ember-1.0"))
	require.False(t, isVertexMetaMaaSModel("publishers/deepseek-ai/models/deepseek-v4.1-flash-maas"))
	require.False(t, isVertexMetaMaaSModel("gemini-4-argon"))

	require.Equal(t, "muse-ember-1.0", normalizeVertexMetaModelID("publishers/meta/models/muse-ember-1.0"))
	require.Equal(t, "muse-ember-1.0", normalizeVertexMetaModelID("meta/muse-ember-1.0"))
	require.Equal(t, "muse-ember-1.0", normalizeVertexMetaModelID("muse-ember-1.0"))

	url, err := buildVertexMetaResponsesURL("proj-1", "global")
	require.NoError(t, err)
	require.Equal(t, "https://aiplatform.googleapis.com/v1/projects/proj-1/locations/global/publishers/meta/v1/responses", url)

	url, err = buildVertexMetaResponsesURL("proj-1", "us-central1")
	require.NoError(t, err)
	require.Equal(t, "https://us-central1-aiplatform.googleapis.com/v1/projects/proj-1/locations/us-central1/publishers/meta/v1/responses", url)

	_, err = buildVertexMetaResponsesURL("", "global")
	require.Error(t, err)
}

func TestVertexMeta_ChatCompletions_NonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, stub := metaTestService(metaJSONResponse(metaUpstreamResponse))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"publishers/meta/models/muse-ember-1.0","messages":[{"role":"user","content":"hi"}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, reqBody)
	require.NoError(t, err)
	require.NotNil(t, result)

	captured := stub.lastReq
	require.NotNil(t, captured)
	require.Equal(t, "https://us-central1-aiplatform.googleapis.com/v1/projects/gen-lang-client-0476190420/locations/us-central1/publishers/meta/v1/responses", captured.URL.String())
	require.Equal(t, "test-api-key", captured.Header.Get("x-goog-api-key"))

	sentBody, _ := io.ReadAll(captured.Body)
	require.Equal(t, "muse-ember-1.0", gjson.GetBytes(sentBody, "model").String())
	require.True(t, gjson.GetBytes(sentBody, "input").Exists(), "wire body must be Responses format")
	require.False(t, gjson.GetBytes(sentBody, "messages").Exists(), "wire body must not be Chat format")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "chat.completion", gjson.GetBytes(rec.Body.Bytes(), "object").String())
	require.Equal(t, "hello from meta", gjson.GetBytes(rec.Body.Bytes(), "choices.0.message.content").String())
	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 6, result.Usage.OutputTokens)
	require.Equal(t, 4, result.Usage.CacheReadInputTokens)
}

func TestVertexMeta_ChatCompletions_Streaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := metaTestService(metaSSEResponse(metaUpstreamSSE))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"publishers/meta/models/muse-ember-1.0","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, reqBody)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "chat.completion.chunk")
	require.Contains(t, rec.Body.String(), "hello")
	require.Contains(t, rec.Body.String(), "data: [DONE]")
	require.Equal(t, 10, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
}

func TestVertexMeta_Responses_NonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, stub := metaTestService(metaJSONResponse(metaUpstreamResponse))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"meta-muse-ember-gcp","input":"hi"}`)
	account.Credentials["model_mapping"] = map[string]any{
		"meta-muse-ember-gcp": "publishers/meta/models/muse-ember-1.0",
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))

	result, err := svc.ForwardAsResponses(context.Background(), c, account, reqBody)
	require.NoError(t, err)

	captured := stub.lastReq
	require.Contains(t, captured.URL.Path, "/publishers/meta/v1/responses")
	sentBody, _ := io.ReadAll(captured.Body)
	require.Equal(t, "muse-ember-1.0", gjson.GetBytes(sentBody, "model").String())

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "response", gjson.GetBytes(rec.Body.Bytes(), "object").String())
	require.Equal(t, "hello from meta", gjson.GetBytes(rec.Body.Bytes(), "output.0.content.0.text").String())
	require.Equal(t, "meta-muse-ember-gcp", gjson.GetBytes(rec.Body.Bytes(), "model").String())
	require.Equal(t, "meta-muse-ember-gcp", result.Model)
	require.Equal(t, "muse-ember-1.0", result.UpstreamModel)
	require.Equal(t, 12, result.Usage.InputTokens)
}

func TestVertexMeta_Responses_Streaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := metaTestService(metaSSEResponse(metaUpstreamSSE))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"publishers/meta/models/muse-ember-1.0","input":"hi","stream":true}`)

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

func TestVertexMeta_ClaudeMessages_NonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, stub := metaTestService(metaJSONResponse(metaUpstreamResponse))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"publishers/meta/models/muse-ember-1.0","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))

	result, err := svc.Forward(context.Background(), c, account, reqBody)
	require.NoError(t, err)

	captured := stub.lastReq
	require.Contains(t, captured.URL.Path, "/publishers/meta/v1/responses")
	sentBody, _ := io.ReadAll(captured.Body)
	require.Equal(t, "muse-ember-1.0", gjson.GetBytes(sentBody, "model").String())
	require.True(t, gjson.GetBytes(sentBody, "input").Exists())

	require.Equal(t, http.StatusOK, rec.Code)
	var anthropicResp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anthropicResp))
	require.Equal(t, "message", anthropicResp["type"])
	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 6, result.Usage.OutputTokens)
}

func TestVertexMeta_Native_NonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, stub := metaTestService(metaJSONResponse(metaUpstreamResponse))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/publishers/meta/models/muse-ember-1.0:generateContent", bytes.NewReader(reqBody))

	result, err := svc.ForwardNative(context.Background(), c, account, "publishers/meta/models/muse-ember-1.0", "generateContent", false, reqBody)
	require.NoError(t, err)

	captured := stub.lastReq
	require.Contains(t, captured.URL.Path, "/publishers/meta/v1/responses")

	require.Equal(t, http.StatusOK, rec.Code)
	var geminiResp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &geminiResp))
	candidates := geminiResp["candidates"].([]any)
	require.Len(t, candidates, 1)
	require.Equal(t, "hello from meta", gjson.GetBytes(rec.Body.Bytes(), "candidates.0.content.parts.0.text").String())
	require.Equal(t, 12, result.Usage.InputTokens)
}

func TestVertexMeta_MissingProjectID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := metaTestService(metaJSONResponse(metaUpstreamResponse))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "")
	reqBody := []byte(`{"model":"publishers/meta/models/muse-ember-1.0","messages":[{"role":"user","content":"hi"}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, reqBody)
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "project_id")
}

func TestVertexMeta_DeepSeekStillUsesOpenAIEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := `{"id":"chatcmpl-1","object":"chat.completion","model":"deepseek-ai/deepseek-v4.1-flash-maas","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	svc, stub := metaTestService(metaJSONResponse(upstream))
	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"publishers/deepseek-ai/models/deepseek-v4.1-flash-maas","messages":[{"role":"user","content":"hi"}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, reqBody)
	require.NoError(t, err)
	require.Contains(t, stub.lastReq.URL.Path, "/endpoints/openapi/chat/completions")
	require.NotContains(t, stub.lastReq.URL.Path, "/publishers/meta/")
}