package geminiweb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// ParseRequest rejects native features that cannot be represented by the website
// before typed decoding could silently drop their fields.
func ParseRequest(body []byte) (*antigravity.GeminiRequest, error) {
	bad := func(message string) (*antigravity.GeminiRequest, error) {
		return nil, &Error{Status: 400, Message: message}
	}
	var raw struct {
		CachedContent string `json:"cachedContent"`
		Contents      []struct {
			Parts []map[string]json.RawMessage `json:"parts"`
		} `json:"contents"`
		SystemInstruction *struct {
			Parts []map[string]json.RawMessage `json:"parts"`
		} `json:"systemInstruction"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return bad("Invalid Gemini Web request JSON")
	}
	if raw.CachedContent != "" {
		return bad("Gemini Web does not support Gemini API cachedContent")
	}
	parts := []map[string]json.RawMessage{}
	for _, content := range raw.Contents {
		parts = append(parts, content.Parts...)
	}
	if raw.SystemInstruction != nil {
		parts = append(parts, raw.SystemInstruction.Parts...)
	}
	for _, part := range parts {
		for _, field := range []string{"fileData", "videoMetadata", "executableCode", "codeExecutionResult"} {
			if value := part[field]; len(value) > 0 && string(value) != "null" {
				return bad("Gemini Web does not support " + field + "; send attachments as inlineData")
			}
		}
	}
	var request antigravity.GeminiRequest
	if json.Unmarshal(body, &request) != nil || len(request.Contents) == 0 {
		return bad("Gemini Web request must include contents")
	}
	return &request, nil
}

func thinkingLevel(req *antigravity.GeminiRequest) int {
	if req.GenerationConfig == nil || req.GenerationConfig.ThinkingConfig == nil {
		return 0
	}
	cfg := req.GenerationConfig.ThinkingConfig
	switch strings.ToUpper(cfg.ThinkingLevel) {
	case "MINIMAL", "LOW":
		return 1
	case "MEDIUM":
		return 2
	case "HIGH":
		return 4
	}
	if cfg.ThinkingBudget < 0 {
		return 1
	}
	if cfg.ThinkingBudget > 16000 {
		return 4
	}
	if cfg.ThinkingBudget > 4000 {
		return 2
	}
	if cfg.ThinkingBudget > 0 || cfg.IncludeThoughts {
		return 1
	}
	return 0
}

func buildPrompt(req *antigravity.GeminiRequest) (string, []*antigravity.GeminiInlineData, error) {
	var prompt strings.Builder
	var attachments []*antigravity.GeminiInlineData
	write := func(content antigravity.GeminiContent) {
		fmt.Fprintf(&prompt, "\n<%s>\n", content.Role)
		for _, part := range content.Parts {
			if part.Thought {
				continue
			}
			prompt.WriteString(part.Text)
			if part.FunctionCall != nil {
				prompt.WriteString("\nTool call: " + marshal(part.FunctionCall))
			}
			if part.FunctionResponse != nil {
				result := *part.FunctionResponse
				result.Parts = nil
				prompt.WriteString("\nTool result: " + marshal(result))
				for _, media := range part.FunctionResponse.Parts {
					if media.InlineData != nil {
						attachments = append(attachments, media.InlineData)
					}
				}
			}
			if part.InlineData != nil {
				attachments = append(attachments, part.InlineData)
			}
		}
		fmt.Fprintf(&prompt, "\n</%s>\n", content.Role)
	}
	if req.SystemInstruction != nil {
		content := *req.SystemInstruction
		content.Role = "system"
		write(content)
	}
	for _, content := range req.Contents {
		write(content)
	}
	if len(req.Contents) == 0 {
		return "", nil, &Error{Status: 400, Message: "Gemini Web request has no contents"}
	}
	var declarations []antigravity.GeminiFunctionDecl
	for _, tool := range req.Tools {
		declarations = append(declarations, tool.FunctionDeclarations...)
		if tool.CodeExecution != nil {
			return "", nil, &Error{Status: 400, Message: "Gemini Web does not expose the Gemini API codeExecution tool"}
		}
	}
	if len(declarations) > 0 {
		prompt.WriteString("\nAvailable functions (JSON Schema):\n" + marshal(declarations))
		prompt.WriteString("\nWhen calling a function, reply with ONLY a JSON object in this exact format: {\"tool_calls\":[{\"name\":\"function_name\",\"arguments\":{}}]}. Use only the declared functions. Otherwise reply normally. Tool results appear in the conversation as Tool result.\n")
		if req.ToolConfig != nil && req.ToolConfig.FunctionCallingConfig != nil {
			switch req.ToolConfig.FunctionCallingConfig.Mode {
			case "NONE":
				prompt.WriteString("Do not call functions in this turn.\n")
			case "ANY", "VALIDATED":
				prompt.WriteString("Call one of the declared functions for this turn.\n")
			}
		}
	}
	if cfg := req.GenerationConfig; cfg != nil {
		if cfg.ResponseMimeType == "application/json" {
			prompt.WriteString("\nReturn only valid JSON without Markdown fences.\n")
		}
		if cfg.ResponseSchema != nil {
			prompt.WriteString("Response JSON Schema:\n" + marshal(cfg.ResponseSchema))
		}
		for _, modality := range cfg.ResponseModalities {
			if modality != "TEXT" {
				return "", nil, &Error{Status: 400, Message: "Gemini Web API output currently supports TEXT; image output requires AI Studio Playground"}
			}
		}
	}
	if len(attachments) > 20 {
		return "", nil, &Error{Status: 400, Message: "Gemini Web accepts at most 20 attachments per request"}
	}
	return prompt.String(), attachments, nil
}

func (c *Client) upload(ctx context.Context, attachments []*antigravity.GeminiInlineData) ([]any, error) {
	var files []any
	total := 0
	for i, attachment := range attachments {
		if len(attachment.Data) > 28<<20 {
			return nil, &Error{Status: 400, Message: "Gemini Web attachment exceeds 20 MiB"}
		}
		data, err := base64.StdEncoding.DecodeString(attachment.Data)
		if err != nil {
			return nil, &Error{Status: 400, Message: "Gemini Web attachment contains invalid base64"}
		}
		total += len(data)
		if len(data) == 0 || len(data) > 20<<20 || total > 50<<20 {
			return nil, &Error{Status: 400, Message: "Gemini Web attachments must be nonempty, at most 20 MiB each and 50 MiB in total"}
		}
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		name := fmt.Sprintf("attachment-%d.bin", i+1)
		mime := attachment.MimeType
		if mime == "" {
			mime = "application/octet-stream"
		}
		if strings.ContainsAny(mime, "\r\n") {
			return nil, &Error{Status: 400, Message: "Invalid attachment MIME type"}
		}
		part, err := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {fmt.Sprintf(`form-data; name="file"; filename="%s"`, name)}, "Content-Type": {mime}})
		if err != nil {
			return nil, err
		}
		if _, err = part.Write(data); err != nil {
			return nil, err
		}
		if err = writer.Close(); err != nil {
			return nil, err
		}
		h := headers()
		h.Set("Content-Type", writer.FormDataContentType())
		h.Set("X-Tenant-Id", "bard-storage")
		pushID := c.pushID
		if pushID == "" {
			pushID = "feeds/mcudyrk2a4khkz"
		}
		h.Set("Push-ID", pushID)
		resp, err := c.do(ctx, http.MethodPost, "https://content-push.googleapis.com/upload", body.Bytes(), h)
		if err != nil {
			return nil, err
		}
		raw, err := readResponse(resp, 1<<20)
		if err != nil {
			return nil, fmt.Errorf("upload attachment %d: %w", i+1, err)
		}
		id := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(id, "/contrib_service/") {
			return nil, fmt.Errorf("Gemini Web returned an invalid attachment identifier")
		}
		files = append(files, []any{[]string{id}, name})
	}
	return files, nil
}

func toolParts(text string, req *antigravity.GeminiRequest) ([]antigravity.GeminiPart, error) {
	allowed := map[string]bool{}
	for _, tool := range req.Tools {
		for _, declaration := range tool.FunctionDeclarations {
			allowed[declaration.Name] = true
		}
	}
	if len(allowed) == 0 {
		return nil, nil
	}
	var output struct {
		Calls []struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"tool_calls"`
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```json") && strings.HasSuffix(trimmed, "```") {
		trimmed = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "```json"), "```"))
	}
	if json.Unmarshal([]byte(trimmed), &output) != nil || len(output.Calls) == 0 {
		return nil, nil
	}
	parts := make([]antigravity.GeminiPart, 0, len(output.Calls))
	for _, call := range output.Calls {
		if !allowed[call.Name] {
			return nil, fmt.Errorf("Gemini Web returned undeclared function %q", call.Name)
		}
		if call.Arguments == nil {
			call.Arguments = map[string]any{}
		}
		parts = append(parts, antigravity.GeminiPart{FunctionCall: &antigravity.GeminiFunctionCall{Name: call.Name, Args: call.Arguments}})
	}
	return parts, nil
}
