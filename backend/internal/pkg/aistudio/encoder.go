package aistudio

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// WireModelName formats the model name with models/ prefix as expected by MakerSuite RPC.
func WireModelName(model string) string {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(model, "models/") {
		return model
	}
	return "models/" + model
}

// ExtractPromptForBinding extracts all user text and relevant payload elements
// to compute the binding SHA-256 digest for BotGuard WAA snapshot.
func ExtractPromptForBinding(req *antigravity.GeminiRequest) string {
	if req == nil {
		return ""
	}
	values := make([]string, 0)
	for _, content := range req.Contents {
		for _, part := range content.Parts {
			switch {
			case part.Text != "":
				values = append(values, part.Text)
			case part.InlineData != nil:
				values = append(values, part.InlineData.Data)
			default:
				values = append(values, "")
			}
		}
	}
	return strings.Join(values, " ")
}

// ComputePayloadDigest returns the sha256 hex digest of the prompt binding.
func ComputePayloadDigest(prompt string) string {
	digest := sha256.Sum256([]byte(prompt))
	return fmt.Sprintf("%x", digest)
}

// EncodeGenerateContentRequest encodes GeminiRequest into Google application/json+protobuf array format.
func EncodeGenerateContentRequest(model string, req *antigravity.GeminiRequest, proof string) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}

	contentsWire := make([]any, 0, len(req.Contents))
	for _, c := range req.Contents {
		partsWire := make([]any, 0, len(c.Parts))
		for _, p := range c.Parts {
			var partWire []any

			if p.Text != "" {
				partWire = []any{nil, p.Text}
				if p.Thought {
					partWire = setWireField(partWire, 12, int64(1))
				}
			} else if p.InlineData != nil {
				partWire = []any{nil, nil, []any{p.InlineData.MimeType, p.InlineData.Data}}
			} else if p.FunctionCall != nil {
				args, err := encodeWireObject(p.FunctionCall.Args)
				if err != nil {
					return nil, err
				}
				callObj := []any{wireFunctionName(p.FunctionCall.Name), args}
				if p.FunctionCall.ID != "" {
					callObj = append(callObj, p.FunctionCall.ID)
				}
				partWire = setWireField(nil, 10, callObj)
			} else if p.FunctionResponse != nil {
				response, err := encodeWireObject(p.FunctionResponse.Response)
				if err != nil {
					return nil, err
				}
				respObj := []any{wireFunctionName(p.FunctionResponse.Name), response}
				if p.FunctionResponse.ID != "" {
					respObj = append(respObj, p.FunctionResponse.ID)
				}
				partWire = setWireField(nil, 11, respObj)
			} else {
				partWire = []any{nil, ""}
			}

			if p.ThoughtSignature != "" {
				partWire = setWireField(partWire, 14, p.ThoughtSignature)
			} else if p.FunctionCall != nil {
				// Avoid upstream signature validation failure on tool calls
				partWire = setWireField(partWire, 14, "skip_thought_signature_validator")
			}

			partsWire = append(partsWire, partWire)
		}

		role := c.Role
		if role == "" || role == "user" || role == "tool" {
			role = "user"
		} else {
			role = "model"
		}

		contentWire := []any{partsWire, role}
		contentsWire = append(contentsWire, contentWire)
	}

	// 3. Generation Config
	configWire := make([]any, 14)
	configWire[13] = int64(1)
	if req.GenerationConfig != nil {
		configWire = make([]any, 17)
		configWire[13] = int64(1)
		cfg := req.GenerationConfig
		if len(cfg.StopSequences) > 0 {
			configWire[1] = cfg.StopSequences
		}
		if cfg.MaxOutputTokens > 0 {
			configWire[3] = cfg.MaxOutputTokens
		}
		if cfg.Temperature != nil {
			configWire[4] = *cfg.Temperature
		}
		if cfg.TopP != nil {
			configWire[5] = *cfg.TopP
		}
		if cfg.TopK != nil {
			configWire[6] = *cfg.TopK
		}
		if cfg.ResponseMimeType != "" {
			configWire[7] = cfg.ResponseMimeType
		}
		if cfg.ResponseSchema != nil {
			raw, err := json.Marshal(cfg.ResponseSchema)
			if err != nil {
				return nil, err
			}
			schema, err := encodeJSONSchema(raw)
			if err != nil {
				return nil, err
			}
			configWire[8] = schema
		}
		if cfg.ResponseModalities != nil {
			modalities := make([]int64, 0, len(cfg.ResponseModalities))
			for _, name := range cfg.ResponseModalities {
				switch strings.ToUpper(name) {
				case "TEXT":
					modalities = append(modalities, 1)
				case "IMAGE":
					modalities = append(modalities, 2)
				case "AUDIO":
					modalities = append(modalities, 3)
				default:
					return nil, fmt.Errorf("unsupported response modality %q", name)
				}
			}
			configWire[14] = modalities
		}
		if cfg.Seed != nil {
			configWire = setWireField(configWire, 18, *cfg.Seed)
		}
		if cfg.ImageConfig != nil {
			var aspect any
			if cfg.ImageConfig.AspectRatio != "" {
				aspect = cfg.ImageConfig.AspectRatio
			}
			configWire = setWireField(configWire, 26, []any{aspect, cfg.ImageConfig.ImageSize})
		}
		if cfg.ThinkingConfig != nil {
			thinking := []any{int64(1)}
			if level := strings.ToUpper(cfg.ThinkingConfig.ThinkingLevel); level != "" {
				values := map[string]int64{"MINIMAL": 4, "LOW": 1, "MEDIUM": 2, "HIGH": 3}
				code, ok := values[level]
				if !ok {
					return nil, fmt.Errorf("unsupported thinking level %q", level)
				}
				thinking = setWireField(thinking, 3, code)
			} else if cfg.ThinkingConfig.ThinkingBudget >= 0 {
				// A level excludes the budget. Its omitted integer value otherwise
				// becomes zero and sends two mutually exclusive protobuf fields.
				thinking = append(thinking, cfg.ThinkingConfig.ThinkingBudget)
			}
			configWire[16] = thinking
		}
	}

	// 4. System Instruction
	var systemWire []any
	if req.SystemInstruction != nil && len(req.SystemInstruction.Parts) > 0 {
		sysParts := make([]any, 0, len(req.SystemInstruction.Parts))
		for _, p := range req.SystemInstruction.Parts {
			if p.Text != "" {
				sysParts = append(sysParts, []any{nil, p.Text})
			}
		}
		if len(sysParts) > 0 {
			systemWire = []any{sysParts, "user"}
		}
	}

	// 5. Tools
	var toolsWire []any
	hasBuiltInTools, hasFunctions := false, false
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			if t.CodeExecution != nil {
				hasBuiltInTools = true
				toolsWire = append(toolsWire, []any{[]any{}})
			}
			if t.GoogleSearch != nil {
				hasBuiltInTools = true
				searchTypes := []any{[]any{}}
				if t.GoogleSearch.EnhancedContent != nil && t.GoogleSearch.EnhancedContent.ImageSearch != nil {
					searchTypes = append(searchTypes, []any{})
				}
				toolsWire = append(toolsWire, []any{nil, nil, nil, []any{nil, searchTypes}})
			}
			if len(t.FunctionDeclarations) > 0 {
				hasFunctions = true
				decls := make([]any, 0, len(t.FunctionDeclarations))
				for _, fd := range t.FunctionDeclarations {
					if fd.Name == "" {
						return nil, fmt.Errorf("function declaration is missing its name")
					}
					decl := []any{wireFunctionName(fd.Name), fd.Description}
					if fd.Parameters != nil {
						raw, err := json.Marshal(fd.Parameters)
						if err != nil {
							return nil, err
						}
						schema, err := encodeJSONSchema(inlineSchemaRefs(raw))
						if err != nil {
							return nil, err
						}
						projectToolSchema(schema)
						decl = append(decl, schema)
					}
					decls = append(decls, decl)
				}
				toolsWire = append(toolsWire, []any{nil, decls})
			}
		}
	}
	if req.ToolConfig != nil && req.ToolConfig.FunctionCallingConfig != nil {
		switch strings.ToUpper(req.ToolConfig.FunctionCallingConfig.Mode) {
		case "NONE":
			toolsWire = []any{}
			hasBuiltInTools, hasFunctions = false, false
		case "", "AUTO", "ANY", "VALIDATED":
		default:
			return nil, fmt.Errorf("unsupported function calling mode %q", req.ToolConfig.FunctionCallingConfig.Mode)
		}
	}

	// Build the main wire array
	// Index 0: Model
	// Index 1: Contents
	// Index 2: SafetySettings (nil)
	// Index 3: Config
	// Index 4: WAA Proof (Field 5 in Protobuf)
	// Index 5: SystemInstruction
	// Index 6: Tools
	// Index 10: 1
	wire := make([]any, 11)
	wire[0] = WireModelName(model)
	wire[1] = contentsWire
	if len(req.SafetySettings) > 0 {
		categories := map[string]int64{"HARM_CATEGORY_HARASSMENT": 7, "HARM_CATEGORY_HATE_SPEECH": 8, "HARM_CATEGORY_SEXUALLY_EXPLICIT": 9, "HARM_CATEGORY_DANGEROUS_CONTENT": 10, "HARM_CATEGORY_CIVIC_INTEGRITY": 11}
		thresholds := map[string]int64{"BLOCK_LOW_AND_ABOVE": 1, "BLOCK_MEDIUM_AND_ABOVE": 2, "BLOCK_ONLY_HIGH": 3, "BLOCK_NONE": 4, "OFF": 5}
		safety := make([]any, 0, len(req.SafetySettings))
		for _, setting := range req.SafetySettings {
			category, ok := categories[setting.Category]
			if !ok {
				return nil, fmt.Errorf("unsupported safety category %q", setting.Category)
			}
			threshold, ok := thresholds[setting.Threshold]
			if !ok {
				return nil, fmt.Errorf("unsupported safety threshold %q", setting.Threshold)
			}
			safety = append(safety, []any{nil, nil, category, threshold})
		}
		wire[2] = safety
	}
	if configWire != nil {
		wire[3] = configWire
	}
	if proof != "" {
		wire[4] = proof
	}
	if systemWire != nil {
		wire[5] = systemWire
	}
	if toolsWire != nil {
		wire[6] = toolsWire
	}
	if req.ToolConfig != nil && req.ToolConfig.IncludeServerSideToolInvocations != nil {
		wire = setWireField(wire, 13, []any{nil, nil, *req.ToolConfig.IncludeServerSideToolInvocations})
	} else if hasBuiltInTools && hasFunctions {
		// AI Studio requires this when built-in tools and function calling coexist.
		wire = setWireField(wire, 13, []any{nil, nil, true})
	}
	wire[10] = int64(1)

	return json.Marshal(wire)
}

// PagePrompt is the last turn's visible text; the proof binding includes all turns.
func PagePrompt(req *antigravity.GeminiRequest) string {
	if req == nil || len(req.Contents) == 0 {
		return ""
	}
	parts := []string{}
	for _, part := range req.Contents[len(req.Contents)-1].Parts {
		if part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, " ")
}
