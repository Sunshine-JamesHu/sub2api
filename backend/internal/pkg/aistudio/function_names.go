package aistudio

import (
	"crypto/sha256"
	"fmt"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// MakerSuite accepts shorter function names than some MCP clients produce.
// Keep the same deterministic alias in declarations and history across requests.
const maxWireFunctionNameLength = 63

func wireFunctionName(name string) string {
	if len(name) <= maxWireFunctionNameLength {
		return name
	}
	digest := sha256.Sum256([]byte(name))
	suffix := fmt.Sprintf("_%x", digest[:6])
	prefix := name[:maxWireFunctionNameLength-len(suffix)]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + suffix
}

// RestoreFunctionNames restores client-visible names before protocol conversion.
// History is included so a follow-up that omits tools can still restore names.
func RestoreFunctionNames(req *antigravity.GeminiRequest, emit func(StreamEvent) error) func(StreamEvent) error {
	originals := make(map[string]string)
	remember := func(name string) {
		if alias := wireFunctionName(name); alias != name {
			originals[alias] = name
		}
	}
	for _, tool := range req.Tools {
		for _, declaration := range tool.FunctionDeclarations {
			remember(declaration.Name)
		}
	}
	for _, content := range req.Contents {
		for _, part := range content.Parts {
			if part.FunctionCall != nil {
				remember(part.FunctionCall.Name)
			}
			if part.FunctionResponse != nil {
				remember(part.FunctionResponse.Name)
			}
		}
	}
	if len(originals) == 0 {
		return emit
	}
	return func(event StreamEvent) error {
		if event.FunctionCall != nil {
			if name, ok := originals[event.FunctionCall.Name]; ok {
				call := *event.FunctionCall
				call.Name = name
				event.FunctionCall = &call
			}
		}
		return emit(event)
	}
}
