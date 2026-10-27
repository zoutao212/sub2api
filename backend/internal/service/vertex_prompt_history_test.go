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

func TestOpenAICacheReadTokensFromUsage_DeepSeekPromptCacheHit(t *testing.T) {
	require.Equal(t, 800, openAICacheReadTokensFromUsage(gjson.Parse(
		`{"prompt_tokens":1200,"prompt_cache_hit_tokens":800,"prompt_cache_miss_tokens":400}`,
	)))
	// A zero-valued compatibility field must not hide a positive DeepSeek hit count.
	require.Equal(t, 800, openAICacheReadTokensFromUsage(gjson.Parse(
		`{"prompt_tokens":1200,"prompt_tokens_details":{"cached_tokens":0},"prompt_cache_hit_tokens":800}`,
	)))
	// A positive official nested field remains authoritative.
	require.Equal(t, 500, openAICacheReadTokensFromUsage(gjson.Parse(
		`{"prompt_tokens":1200,"prompt_tokens_details":{"cached_tokens":500},"prompt_cache_hit_tokens":800}`,
	)))
	require.Equal(t, 0, openAICacheReadTokensFromUsage(gjson.Parse(`{"prompt_tokens":1200}`)))
}

func TestObserveVertexPromptHistory_DetectsRewriteOnlyWhenHistoryGrows(t *testing.T) {
	key := "test-responses|42|session-a"
	digests := []string{"a", "b", "c"}

	prev, common, _, _, seen := observeVertexPromptHistory(key, "tools", "system", digests)
	require.False(t, seen)
	require.Zero(t, prev)
	require.Zero(t, common)

	// Append-only: the previous messages remain a literal prefix.
	prev, common, toolsChanged, systemChanged, seen := observeVertexPromptHistory(
		key, "tools", "system", []string{"a", "b", "c", "d", "e"},
	)
	require.True(t, seen)
	require.Equal(t, 3, prev)
	require.Equal(t, 3, common)
	require.False(t, toolsChanged)
	require.False(t, systemChanged)

	// Rewrite: history grew but earlier messages changed.
	prev, common, _, _, _ = observeVertexPromptHistory(
		key, "tools", "system", []string{"a", "x", "c", "d", "e", "f", "g"},
	)
	require.Equal(t, 5, prev)
	require.Equal(t, 1, common)

	// Tool-list change is reported independently.
	_, _, toolsChanged, _, _ = observeVertexPromptHistory(
		key, "tools-v2", "system", []string{"a", "x", "c", "d", "e", "f", "g", "h"},
	)
	require.True(t, toolsChanged)
}

func TestForwardAsResponses_VertexThirdPartyMaaS_DeepSeekCacheHitUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name     string
		stream   bool
		upstream string
	}{
		{
			name:     "non-streaming",
			upstream: `{"id":"chatcmpl-cache","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1200,"completion_tokens":5,"prompt_cache_hit_tokens":900}}`,
		},
		{
			name:     "streaming",
			stream:   true,
			upstream: "data: {\"id\":\"chatcmpl-cache\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"id\":\"chatcmpl-cache\",\"choices\":[],\"usage\":{\"prompt_tokens\":1200,\"completion_tokens\":5,\"prompt_cache_hit_tokens\":900}}\n\ndata: [DONE]\n\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			httpStub := &geminiCompatHTTPUpstreamStub{
				response: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(test.upstream)),
				},
			}
			svc := &GeminiMessagesCompatService{httpUpstream: httpStub, cfg: &config.Config{}}
			account := newGeminiVertexThirdPartyTestAccount("apikey", "gen-lang-client-0476190420")
			reqBody, err := json.Marshal(map[string]any{
				"model": "deepseek-v4.1-flash-maas",
				"input": []any{map[string]any{
					"type":    "message",
					"role":    "user",
					"content": []any{map[string]any{"type": "input_text", "text": "hi"}},
				}},
				"stream": test.stream,
			})
			require.NoError(t, err)

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))

			result, err := svc.ForwardAsResponses(context.Background(), c, account, reqBody)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, 1200, result.Usage.InputTokens)
			require.Equal(t, 5, result.Usage.OutputTokens)
			require.Equal(t, 900, result.Usage.CacheReadInputTokens)
		})
	}
}
