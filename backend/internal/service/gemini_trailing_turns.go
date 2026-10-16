package service

// geminiContinuationTurnText is the user turn appended when a converted history
// would end with a model turn.
const geminiContinuationTurnText = "Continue."

// normalizeGeminiTrailingTurns rewrites the tail of a converted `contents` array
// into a shape Gemini 3.6+ and 4.x accept. Both shapes below are answered with
// `400 Requests ending with a model turn are not supported.` (verified against
// Vertex with single-purpose probe payloads):
//
//  1. A history that ends with a model turn. These models reject a prefilled
//     model turn, and every translated request whose last message is an
//     assistant message produces one - a client handing over a history built
//     against another vendor's model, a generation replayed after being cut
//     short, a re-run turn. The model turn is kept (dropping it would lose the
//     client's context) and a minimal user turn is appended.
//
//  2. A trailing user turn that mixes a functionResponse with other parts. This
//     is what the chain produces when a client puts a tool result and a user
//     message in the same turn: the Responses/Anthropic bridges merge adjacent
//     user messages, so the merged turn holds functionResponse plus text.
//     Moving the non-functionResponse parts into their own trailing user turn is
//     accepted; a trailing turn holding only functionResponse is accepted as-is.
func normalizeGeminiTrailingTurns(contents []any) []any {
	if len(contents) == 0 {
		return contents
	}
	last, ok := contents[len(contents)-1].(map[string]any)
	if !ok {
		return contents
	}

	switch role, _ := last["role"].(string); role {
	case "model":
		return append(contents, geminiContinuationTurn())
	case "user":
		parts, ok := last["parts"].([]any)
		if !ok {
			return contents
		}
		responses := make([]any, 0, len(parts))
		rest := make([]any, 0, len(parts))
		for _, part := range parts {
			if pm, ok := part.(map[string]any); ok && pm["functionResponse"] != nil {
				responses = append(responses, part)
				continue
			}
			rest = append(rest, part)
		}
		if len(responses) == 0 || len(rest) == 0 {
			return contents
		}
		last["parts"] = responses
		return append(contents, map[string]any{"role": "user", "parts": rest})
	}
	return contents
}

func geminiContinuationTurn() map[string]any {
	return map[string]any{
		"role":  "user",
		"parts": []any{map[string]any{"text": geminiContinuationTurnText}},
	}
}
