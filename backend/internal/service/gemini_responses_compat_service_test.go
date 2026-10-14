package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newResponsesCompatTestContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c, w
}

func TestGeminiResponseToResponsesTextAndUsage(t *testing.T) {
	geminiResp := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": "Hello"}}},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{"promptTokenCount": 10, "candidatesTokenCount": 5},
	}
	raw, err := json.Marshal(geminiResp)
	require.NoError(t, err)

	got, usage, err := geminiResponseToResponses(geminiResp, "gemini-test", raw, nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, usage)
	require.Equal(t, 10, usage.InputTokens)
	require.Equal(t, 5, usage.OutputTokens)

	out, err := json.Marshal(got)
	require.NoError(t, err)
	require.Contains(t, string(out), "Hello")
}

func TestGeminiResponseToResponsesFunctionCall(t *testing.T) {
	geminiResp := map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{
				map[string]any{"functionCall": map[string]any{"name": "get_weather", "args": map[string]any{"city": "Paris"}}},
			}},
			"finishReason": "STOP",
		}},
	}
	raw, err := json.Marshal(geminiResp)
	require.NoError(t, err)

	got, _, err := geminiResponseToResponses(geminiResp, "gemini-test", raw, nil)
	require.NoError(t, err)

	out, err := json.Marshal(got)
	require.NoError(t, err)
	var parsed struct {
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Len(t, parsed.Output, 1)
	require.Equal(t, "function_call", parsed.Output[0].Type)
	require.Equal(t, "get_weather", parsed.Output[0].Name)
	require.JSONEq(t, `{"city":"Paris"}`, parsed.Output[0].Arguments)
}

func TestHandleResponsesStreamingResponseFromGeminiText(t *testing.T) {
	c, w := newResponsesCompatTestContext(t)
	svc := &GeminiMessagesCompatService{}

	sse := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"Hel"}],"role":"model"}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":1}}`,
		"",
		`data: {"candidates":[{"content":{"parts":[{"text":"lo"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2}}`,
		"",
		"",
	}, "\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	result, err := svc.handleResponsesStreamingResponseFromGemini(
		c, resp, time.Now(), "gemini-2.5-pro", apicompat.ResponsesClientToolMapping{}, false,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
	require.Equal(t, 7, result.usage.InputTokens)
	require.Equal(t, 2, result.usage.OutputTokens)

	out := w.Body.String()
	require.Contains(t, out, "event: response.created")
	require.Contains(t, out, `"delta":"Hel"`)
	require.Contains(t, out, `"delta":"lo"`)
	require.Contains(t, out, "event: response.output_text.done")
	require.Contains(t, out, "event: response.completed")
}

func TestHandleResponsesStreamingResponseFromGeminiFunctionCall(t *testing.T) {
	c, w := newResponsesCompatTestContext(t)
	svc := &GeminiMessagesCompatService{}

	sse := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}],"role":"model"},"finishReason":"STOP"}]}`,
		"",
		"",
	}, "\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	result, err := svc.handleResponsesStreamingResponseFromGemini(
		c, resp, time.Now(), "gemini-2.5-pro", apicompat.ResponsesClientToolMapping{}, false,
	)
	require.NoError(t, err)
	require.NotNil(t, result)

	out := w.Body.String()
	require.Contains(t, out, "event: response.created")
	require.Contains(t, out, "event: response.output_item.added")
	require.Contains(t, out, "event: response.function_call_arguments.delta")
	require.Contains(t, out, "event: response.function_call_arguments.done")
	require.Contains(t, out, "event: response.output_item.done")
	require.Contains(t, out, `"name":"get_weather"`)
	require.Contains(t, out, "event: response.completed")
	require.NotContains(t, out, "event: response.output_text.delta")
}

func TestHandleResponsesStreamingResponseFromGeminiStopsCleanlyOnEmptyStream(t *testing.T) {
	c, _ := newResponsesCompatTestContext(t)
	svc := &GeminiMessagesCompatService{}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
	}
	result, err := svc.handleResponsesStreamingResponseFromGemini(
		c, resp, time.Now(), "gemini-2.5-pro", apicompat.ResponsesClientToolMapping{}, false,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
}
