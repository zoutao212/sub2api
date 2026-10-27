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

func newGeminiVertexThirdPartyTestAccount(authMode string, projectID string) *Account {
	creds := map[string]any{
		"location": "us-central1",
	}
	if authMode == "apikey" {
		creds["auth_mode"] = "apikey"
		creds["api_key"] = "test-api-key"
	} else {
		creds["service_account_json"] = `{
			"type": "service_account",
			"project_id": "` + projectID + `",
			"private_key_id": "kid",
			"private_key": "-----BEGIN PRIVATE KEY-----\nMIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQD\n-----END PRIVATE KEY-----\n",
			"client_email": "svc@` + projectID + `.iam.gserviceaccount.com"
		}`
	}
	if projectID != "" {
		creds["project_id"] = projectID
	}
	return &Account{
		ID:          999,
		Platform:    PlatformGemini,
		Type:        AccountTypeServiceAccount,
		Credentials: creds,
		Concurrency: 1,
	}
}

func TestForwardAsChatCompletions_VertexThirdPartyMaaS_NonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamResponse := `{"id":"chatcmpl-123","object":"chat.completion","model":"deepseek-ai/deepseek-v4.1-flash-maas","choices":[{"index":0,"message":{"role":"assistant","content":"hello from deepseek"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":6,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":4}}}`

	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"upstream-req-123"}},
			Body:       io.NopCloser(bytes.NewReader([]byte(upstreamResponse))),
		},
	}

	svc := &GeminiMessagesCompatService{
		httpUpstream: httpStub,
		cfg:          &config.Config{},
	}

	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"publishers/deepseek-ai/models/deepseek-v4.1-flash-maas","messages":[{"role":"user","content":"hi"}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, reqBody)
	require.NoError(t, err)
	require.NotNil(t, result)

	capturedReq := httpStub.lastReq
	require.NotNil(t, capturedReq)

	// 验证请求目标 URL 格式
	expectedURL := "https://us-central1-aiplatform.googleapis.com/v1/projects/gen-lang-client-0476190420/locations/us-central1/endpoints/openapi/chat/completions"
	require.Equal(t, expectedURL, capturedReq.URL.String())

	// 验证 API Key 认证头部
	require.Equal(t, "test-api-key", capturedReq.Header.Get("x-goog-api-key"))

	// 验证 body 中的 model 被正确规整化为 OpenAI 端点接收的 deepseek-ai/deepseek-v4.1-flash-maas
	capturedBody, _ := io.ReadAll(capturedReq.Body)
	require.Equal(t, "deepseek-ai/deepseek-v4.1-flash-maas", gjson.GetBytes(capturedBody, "model").String())

	// 验证客户端响应
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, upstreamResponse, rec.Body.String())

	// 验证 Usage 提取
	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 6, result.Usage.OutputTokens)
	require.Equal(t, 4, result.Usage.CacheReadInputTokens)
}

func TestForwardAsChatCompletions_VertexThirdPartyMaaS_Streaming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamSSE := "data: {\"id\":\"chatcmpl-123\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"id\":\"chatcmpl-123\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"

	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
		},
	}

	svc := &GeminiMessagesCompatService{
		httpUpstream: httpStub,
		cfg:          &config.Config{},
	}

	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"deepseek-v4.1-flash-maas","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, reqBody)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "data: [DONE]")
	require.Equal(t, 10, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
}

func TestForwardAsChatCompletions_VertexThirdPartyMaaS_MissingProjectID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	svc := &GeminiMessagesCompatService{
		cfg: &config.Config{},
	}

	account := newGeminiVertexThirdPartyTestAccount("apikey", "")
	reqBody := []byte(`{"model":"deepseek-v4.1-flash-maas","messages":[{"role":"user","content":"hi"}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, reqBody)
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "project_id")
}

func TestBuildGeminiServiceAccountRequest_VertexThirdPartyMaaS(t *testing.T) {
	svc := &AccountTestService{
		cfg: &config.Config{},
	}

	account := newGeminiVertexThirdPartyTestAccount("apikey", "my-test-proj")
	req, err := svc.buildGeminiServiceAccountRequest(context.Background(), account, "publishers/deepseek-ai/models/deepseek-v4.1-flash-maas", []byte(`{}`))
	require.NoError(t, err)
	require.NotNil(t, req)

	require.Equal(t, "https://us-central1-aiplatform.googleapis.com/v1/projects/my-test-proj/locations/us-central1/endpoints/openapi/chat/completions", req.URL.String())
	require.Equal(t, "test-api-key", req.Header.Get("x-goog-api-key"))

	bodyBytes, _ := io.ReadAll(req.Body)
	require.Equal(t, "deepseek-ai/deepseek-v4.1-flash-maas", gjson.GetBytes(bodyBytes, "model").String())
}

func TestForwardClaudeMessages_VertexThirdPartyMaaS_NonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamCCResponse := `{"id":"chatcmpl-456","object":"chat.completion","model":"deepseek-ai/deepseek-v4.1-flash-maas","choices":[{"index":0,"message":{"role":"assistant","content":"hello from claude bridge"},"finish_reason":"stop"}],"usage":{"prompt_tokens":15,"completion_tokens":8,"total_tokens":23}}`

	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader([]byte(upstreamCCResponse))),
		},
	}

	svc := &GeminiMessagesCompatService{
		httpUpstream: httpStub,
		cfg:          &config.Config{},
	}

	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"publishers/deepseek-ai/models/deepseek-v4.1-flash-maas","messages":[{"role":"user","content":"hi"}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))

	result, err := svc.Forward(context.Background(), c, account, reqBody)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, http.StatusOK, rec.Code)
	var anthropicResp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anthropicResp))
	require.Equal(t, "message", anthropicResp["type"])

	require.Equal(t, 15, result.Usage.InputTokens)
	require.Equal(t, 8, result.Usage.OutputTokens)
}

func TestForwardNative_VertexThirdPartyMaaS_NonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamCCResponse := `{"id":"chatcmpl-789","object":"chat.completion","model":"deepseek-ai/deepseek-v4.1-flash-maas","choices":[{"index":0,"message":{"role":"assistant","content":"hello from native bridge"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader([]byte(upstreamCCResponse))),
		},
	}

	svc := &GeminiMessagesCompatService{
		httpUpstream: httpStub,
		cfg:          &config.Config{},
	}

	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/publishers/deepseek-ai/models/deepseek-v4.1-flash-maas:generateContent", bytes.NewReader(reqBody))

	result, err := svc.ForwardNative(context.Background(), c, account, "publishers/deepseek-ai/models/deepseek-v4.1-flash-maas", "generateContent", false, reqBody)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, http.StatusOK, rec.Code)
	var geminiResp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &geminiResp))
	candidates := geminiResp["candidates"].([]any)
	require.Len(t, candidates, 1)

	require.Equal(t, 10, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
}

func TestForwardAsResponses_VertexThirdPartyMaaS_RoutesToOpenAICompatibleEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamResponse := `{"id":"chatcmpl-123","object":"chat.completion","model":"deepseek-ai/deepseek-v4.1-flash-maas","choices":[{"index":0,"message":{"role":"assistant","content":"hello from responses bridge"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":6,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":4}}}`

	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"upstream-req-123"}},
			Body:       io.NopCloser(bytes.NewReader([]byte(upstreamResponse))),
		},
	}

	svc := &GeminiMessagesCompatService{
		httpUpstream: httpStub,
		cfg:          &config.Config{},
	}

	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"publishers/deepseek-ai/models/deepseek-v4.1-flash-maas","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"lookup","description":"look up","parameters":{"type":"object","properties":{"map":{"type":"object","additionalProperties":{"type":"string"},"propertyNames":{"pattern":"^[a-z]+$"}}},"required":["map"]}}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))

	result, err := svc.ForwardAsResponses(context.Background(), c, account, reqBody)
	require.NoError(t, err)
	require.NotNil(t, result)

	capturedReq := httpStub.lastReq
	require.NotNil(t, capturedReq)

	expectedURL := "https://us-central1-aiplatform.googleapis.com/v1/projects/gen-lang-client-0476190420/locations/us-central1/endpoints/openapi/chat/completions"
	require.Equal(t, expectedURL, capturedReq.URL.String())
	require.Equal(t, "test-api-key", capturedReq.Header.Get("x-goog-api-key"))

	capturedBody, _ := io.ReadAll(capturedReq.Body)
	require.Equal(t, "deepseek-ai/deepseek-v4.1-flash-maas", gjson.GetBytes(capturedBody, "model").String())
	require.Equal(t, "user", gjson.GetBytes(capturedBody, "messages.0.role").String())
	require.False(t, gjson.GetBytes(capturedBody, "contents").Exists())
	require.False(t, gjson.GetBytes(capturedBody, "tools.0.functionDeclarations").Exists())
	require.Equal(t, "function", gjson.GetBytes(capturedBody, "tools.0.type").String())
	require.Equal(t, "lookup", gjson.GetBytes(capturedBody, "tools.0.function.name").String())
	// OpenAI 兼容端点原样接收 propertyNames，不再被 Gemini proto 拒绝。
	require.Equal(t, "^[a-z]+$", gjson.GetBytes(capturedBody, "tools.0.function.parameters.properties.map.propertyNames.pattern").String())

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "response", gjson.GetBytes(rec.Body.Bytes(), "object").String())
	require.Equal(t, "hello from responses bridge", gjson.GetBytes(rec.Body.Bytes(), "output.0.content.0.text").String())

	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 6, result.Usage.OutputTokens)
	require.Equal(t, 4, result.Usage.CacheReadInputTokens)
}

func TestForwardAsResponses_VertexThirdPartyMaaS_GCPAliasUsesAccountMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamResponse := `{"id":"chatcmpl-alias","object":"chat.completion","model":"deepseek-ai/deepseek-v4.1-flash-maas","choices":[{"index":0,"message":{"role":"assistant","content":"alias ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader([]byte(upstreamResponse))),
		},
	}

	svc := &GeminiMessagesCompatService{
		httpUpstream: httpStub,
		cfg:          &config.Config{},
	}

	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	account.Credentials["model_mapping"] = map[string]any{
		"deepseek-v4.1-flash-gcp": "publishers/deepseek-ai/models/deepseek-v4.1-flash-maas",
	}
	reqBody := []byte(`{"model":"deepseek-v4.1-flash-gcp","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))

	result, err := svc.ForwardAsResponses(context.Background(), c, account, reqBody)
	require.NoError(t, err)
	require.NotNil(t, result)

	capturedReq := httpStub.lastReq
	require.NotNil(t, capturedReq)
	require.Contains(t, capturedReq.URL.Path, "/endpoints/openapi/chat/completions")

	capturedBody, _ := io.ReadAll(capturedReq.Body)
	require.Equal(t, "deepseek-ai/deepseek-v4.1-flash-maas", gjson.GetBytes(capturedBody, "model").String())
	require.Equal(t, "deepseek-v4.1-flash-gcp", result.Model)
	require.Equal(t, "deepseek-ai/deepseek-v4.1-flash-maas", result.UpstreamModel)
	require.Equal(t, "alias ok", gjson.GetBytes(rec.Body.Bytes(), "output.0.content.0.text").String())
}

func TestForwardAsResponses_VertexThirdPartyMaaS_Streaming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamSSE := "data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
		"data: [DONE]\n\n"

	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
		},
	}

	svc := &GeminiMessagesCompatService{
		httpUpstream: httpStub,
		cfg:          &config.Config{},
	}

	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"deepseek-v4.1-flash-maas","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))

	result, err := svc.ForwardAsResponses(context.Background(), c, account, reqBody)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "event: response.output_text.delta")
	require.Contains(t, rec.Body.String(), "hello")
	require.Contains(t, rec.Body.String(), "data: [DONE]")
	require.Equal(t, 10, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
}

func TestForwardAsChatCompletions_VertexThirdPartyMaaS_ReasoningEffortXHighMappedToMax(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamResponse := `{"id":"chatcmpl-xhigh","object":"chat.completion","model":"deepseek-ai/deepseek-v4.1-flash-maas","choices":[{"index":0,"message":{"role":"assistant","content":"done reasoning"},"finish_reason":"stop"}]}`

	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader([]byte(upstreamResponse))),
		},
	}

	svc := &GeminiMessagesCompatService{
		httpUpstream: httpStub,
		cfg:          &config.Config{},
	}

	account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
	reqBody := []byte(`{"model":"publishers/deepseek-ai/models/deepseek-v4.1-flash-maas","messages":[{"role":"user","content":"hard problem"}],"reasoning_effort":"xhigh"}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, reqBody)
	require.NoError(t, err)
	require.NotNil(t, result)

	capturedReq := httpStub.lastReq
	require.NotNil(t, capturedReq)

	capturedBody, _ := io.ReadAll(capturedReq.Body)
	// 验证 xhigh 已被正确规范化为 Google 接受的 max
	require.Equal(t, "max", gjson.GetBytes(capturedBody, "reasoning_effort").String())
}
