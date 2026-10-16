package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func geminiContentsFromClaudeRequest(t *testing.T, messages []any) []any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"messages": messages})
	require.NoError(t, err)

	converted, err := convertClaudeMessagesToGeminiGenerateContent(body)
	require.NoError(t, err)

	var geminiReq map[string]any
	require.NoError(t, json.Unmarshal(converted, &geminiReq))
	contents, ok := geminiReq["contents"].([]any)
	require.True(t, ok)
	return contents
}

func contentRole(t *testing.T, content any) string {
	t.Helper()
	cm, ok := content.(map[string]any)
	require.True(t, ok)
	role, _ := cm["role"].(string)
	return role
}

func contentParts(t *testing.T, content any) []any {
	t.Helper()
	cm, ok := content.(map[string]any)
	require.True(t, ok)
	parts, ok := cm["parts"].([]any)
	require.True(t, ok)
	return parts
}

func firstPartText(t *testing.T, content any) string {
	t.Helper()
	parts := contentParts(t, content)
	require.NotEmpty(t, parts)
	pm, ok := parts[0].(map[string]any)
	require.True(t, ok)
	text, _ := pm["text"].(string)
	return text
}

func hasFunctionResponse(t *testing.T, content any) bool {
	t.Helper()
	for _, part := range contentParts(t, content) {
		if pm, ok := part.(map[string]any); ok && pm["functionResponse"] != nil {
			return true
		}
	}
	return false
}

func TestNormalizeGeminiTrailingTurns_AppendsUserTurnAfterModelTurn(t *testing.T) {
	contents := geminiContentsFromClaudeRequest(t, []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "partial answer"},
	})

	require.Len(t, contents, 3)
	require.Equal(t, "model", contentRole(t, contents[1]))
	require.Equal(t, "partial answer", firstPartText(t, contents[1]))
	require.Equal(t, "user", contentRole(t, contents[2]))
	require.Equal(t, geminiContinuationTurnText, firstPartText(t, contents[2]))
}

func TestNormalizeGeminiTrailingTurns_SplitsToolResultMergedWithText(t *testing.T) {
	contents := geminiContentsFromClaudeRequest(t, []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "search it"},
		}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    "toolu_1",
				"name":  "Grep",
				"input": map[string]any{"pattern": "foo"},
			},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "file body"},
			map[string]any{"type": "text", "text": "now what"},
		}},
	})

	require.Len(t, contents, 4)
	require.Equal(t, "user", contentRole(t, contents[2]))
	require.True(t, hasFunctionResponse(t, contents[2]))
	require.Equal(t, "user", contentRole(t, contents[3]))
	require.False(t, hasFunctionResponse(t, contents[3]))
	require.Equal(t, "now what", firstPartText(t, contents[3]))
}

func TestNormalizeGeminiTrailingTurns_KeepsToolResultOnlyTurn(t *testing.T) {
	contents := geminiContentsFromClaudeRequest(t, []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "search it"},
		}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    "toolu_1",
				"name":  "Grep",
				"input": map[string]any{"pattern": "foo"},
			},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "file body"},
		}},
	})

	require.Len(t, contents, 3)
	require.Equal(t, "user", contentRole(t, contents[2]))
	require.True(t, hasFunctionResponse(t, contents[2]))
	require.Equal(t, 1, len(contentParts(t, contents[2])))
}

func TestNormalizeGeminiTrailingTurns_SplitsTextBeforeToolResult(t *testing.T) {
	contents := normalizeGeminiTrailingTurns([]any{
		map[string]any{"role": "user", "parts": []any{
			map[string]any{"text": "trailing note"},
			map[string]any{"functionResponse": map[string]any{"name": "Grep"}},
		}},
	})

	require.Len(t, contents, 2)
	require.True(t, hasFunctionResponse(t, contents[0]))
	require.Len(t, contentParts(t, contents[0]), 1)
	require.False(t, hasFunctionResponse(t, contents[1]))
	require.Equal(t, "trailing note", firstPartText(t, contents[1]))
}

func TestNormalizeGeminiTrailingTurns_KeepsMultipleFunctionResponsesTogether(t *testing.T) {
	contents := normalizeGeminiTrailingTurns([]any{
		map[string]any{"role": "user", "parts": []any{
			map[string]any{"functionResponse": map[string]any{"name": "Grep"}},
			map[string]any{"functionResponse": map[string]any{"name": "Read"}},
			map[string]any{"text": "both done"},
		}},
	})

	require.Len(t, contents, 2)
	require.Len(t, contentParts(t, contents[0]), 2)
	require.Equal(t, "both done", firstPartText(t, contents[1]))
}

func TestNormalizeGeminiTrailingTurns_LeavesTrailingUserTurnUntouched(t *testing.T) {
	contents := geminiContentsFromClaudeRequest(t, []any{
		map[string]any{"role": "user", "content": "first"},
		map[string]any{"role": "assistant", "content": "reply"},
		map[string]any{"role": "user", "content": "second"},
	})

	require.Len(t, contents, 3)
	require.Equal(t, "user", contentRole(t, contents[2]))
	require.Equal(t, "second", firstPartText(t, contents[2]))
}

func TestNormalizeGeminiTrailingTurns_EmptyContents(t *testing.T) {
	require.Empty(t, normalizeGeminiTrailingTurns(nil))
	require.Empty(t, normalizeGeminiTrailingTurns([]any{}))
}
