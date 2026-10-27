package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// vertexPromptHistoryEntry 保留同一客户端会话上一轮上发提示的指纹。只存摘要，
// 不存原文；用于区分「网关翻译后前缀漂移」和「上游缓存未命中」。
type vertexPromptHistoryEntry struct {
	toolsDigest    string
	systemDigest   string
	messageDigests []string
	updatedAt      time.Time
}

type vertexPromptHistoryTracker struct {
	mu      sync.Mutex
	max     int
	entries map[string]*vertexPromptHistoryEntry
}

var vertexPromptHistory = &vertexPromptHistoryTracker{max: 256, entries: make(map[string]*vertexPromptHistoryEntry)}

func vertexPromptDigest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}

func commonVertexPromptMessages(previous, current []string) int {
	limit := len(previous)
	if len(current) < limit {
		limit = len(current)
	}
	common := 0
	for common < limit && previous[common] == current[common] {
		common++
	}
	return common
}

// observeVertexPromptHistory 记录本轮消息指纹并返回与上一轮的对比结果。
// ok=false 表示这是该会话的第一次观测。
func observeVertexPromptHistory(
	key, toolsDigest, systemDigest string, messageDigests []string,
) (prevMessages, commonMessages int, toolsChanged, systemChanged, ok bool) {
	if key == "" || len(messageDigests) == 0 {
		return 0, 0, false, false, false
	}
	t := vertexPromptHistory
	t.mu.Lock()
	defer t.mu.Unlock()

	prev := t.entries[key]
	if prev != nil {
		prevMessages = len(prev.messageDigests)
		commonMessages = commonVertexPromptMessages(prev.messageDigests, messageDigests)
		toolsChanged = prev.toolsDigest != "" && prev.toolsDigest != toolsDigest
		systemChanged = prev.systemDigest != "" && prev.systemDigest != systemDigest
	}
	if prev == nil && len(t.entries) >= t.max {
		oldestKey := ""
		var oldest time.Time
		for entryKey, entry := range t.entries {
			if oldestKey == "" || entry.updatedAt.Before(oldest) {
				oldestKey, oldest = entryKey, entry.updatedAt
			}
		}
		delete(t.entries, oldestKey)
	}
	t.entries[key] = &vertexPromptHistoryEntry{
		toolsDigest:    toolsDigest,
		systemDigest:   systemDigest,
		messageDigests: messageDigests,
		updatedAt:      time.Now(),
	}
	return prevMessages, commonMessages, toolsChanged, systemChanged, prev != nil
}

// logVertexPromptHistoryDriftForChatBody 检查 Chat Completions 形状（messages
// 数组）的上发请求。第三方 MaaS 的 OpenAI 兼容通道使用该形状。
func logVertexPromptHistoryDriftForChatBody(c *gin.Context, account *Account, protocol, model string, body []byte) {
	messages := gjson.GetBytes(body, "messages").Array()
	if len(messages) == 0 {
		return
	}
	digests := make([]string, 0, len(messages))
	systemDigest := ""
	for index, message := range messages {
		digests = append(digests, vertexPromptDigest(message.Raw))
		if index == 0 && message.Get("role").String() == "system" {
			systemDigest = vertexPromptDigest(message.Raw)
		}
	}
	toolsDigest := ""
	if tools := gjson.GetBytes(body, "tools"); tools.Exists() {
		toolsDigest = vertexPromptDigest(tools.Raw)
	}
	logVertexPromptHistoryDrift(c, account, protocol, model, toolsDigest, systemDigest, digests)
}

// logVertexPromptHistoryDriftForGeminiBody 检查 Gemini generateContent 形状
// （contents 数组）的上发请求。
func logVertexPromptHistoryDriftForGeminiBody(c *gin.Context, account *Account, protocol, model string, body []byte) {
	contents := gjson.GetBytes(body, "contents").Array()
	if len(contents) == 0 {
		return
	}
	digests := make([]string, 0, len(contents))
	for _, content := range contents {
		digests = append(digests, vertexPromptDigest(content.Raw))
	}
	systemDigest := ""
	if system := gjson.GetBytes(body, "systemInstruction"); system.Exists() {
		systemDigest = vertexPromptDigest(system.Raw)
	}
	toolsDigest := ""
	if tools := gjson.GetBytes(body, "tools"); tools.Exists() {
		toolsDigest = vertexPromptDigest(tools.Raw)
	}
	logVertexPromptHistoryDrift(c, account, protocol, model, toolsDigest, systemDigest, digests)
}

// logVertexPromptHistoryDrift 在同一会话的历史变长、但上一轮消息不再是本轮前缀
// 时告警。正常情况下客户端追加消息，前缀必然完整保留；此时若出现断点，说明
// 客户端或中转重写了历史，上游前缀缓存必然失效。
func logVertexPromptHistoryDrift(
	c *gin.Context,
	account *Account,
	protocol, model string,
	toolsDigest, systemDigest string,
	messageDigests []string,
) {
	sessionID := ExtractClientSessionID(c)
	if sessionID == "" || account == nil || len(messageDigests) == 0 {
		return
	}

	key := fmt.Sprintf("%s|%d|%s", protocol, account.ID, sessionID)
	prevMessages, commonMessages, toolsChanged, systemChanged, seen := observeVertexPromptHistory(
		key, toolsDigest, systemDigest, messageDigests,
	)
	if !seen {
		return
	}
	// 历史被截断/压缩时消息会变少，属于客户端正常行为；只有历史变长却无法
	// 完整前缀匹配，才是会击穿缓存的重写。tools/system 位于提示最前端，任何
	// 变化都会让缓存整段失效，因此无论历史长度如何都要报告。
	historyRewritten := len(messageDigests) >= prevMessages && commonMessages < prevMessages
	if !historyRewritten && !toolsChanged && !systemChanged {
		return
	}

	logger.L().Warn("vertex prompt history rewritten between turns",
		zap.Int64("account_id", account.ID),
		zap.String("protocol", protocol),
		zap.String("model", model),
		zap.Int("previous_messages", prevMessages),
		zap.Int("current_messages", len(messageDigests)),
		zap.Int("common_prefix_messages", commonMessages),
		zap.Bool("tools_changed", toolsChanged),
		zap.Bool("system_changed", systemChanged),
	)
}
