package aistudio

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// StreamEvent represents a parsed event from the streaming JSPB response.
type StreamEvent struct {
	Text             string
	Thought          bool
	ThoughtSignature string
	InlineData       *antigravity.GeminiInlineData
	FunctionCall     *antigravity.GeminiFunctionCall
	FinishReason     string
	Usage            *UsageMetadata
}

// UsageMetadata contains token counts reported by MakerSuite.
type UsageMetadata struct {
	PromptTokenCount        int
	CandidatesTokenCount    int
	TotalTokenCount         int
	CachedContentTokenCount int
	ThoughtsTokenCount      int
	ToolUsePromptTokenCount int
}

// sparseJSONReader pads missing elements in Google JSON+Protobuf arrays (e.g. `[1,,3]`) with `null`.
type sparseJSONReader struct {
	source        *bufio.Reader
	pending       []byte
	inString      bool
	escaped       bool
	previousToken byte
}

func newSparseJSONReader(source io.Reader) io.Reader {
	return &sparseJSONReader{source: bufio.NewReader(source)}
}

func (r *sparseJSONReader) Read(destination []byte) (int, error) {
	written := 0
	for written < len(destination) {
		if len(r.pending) > 0 {
			count := copy(destination[written:], r.pending)
			r.pending = r.pending[count:]
			written += count
			continue
		}
		if written > 0 && r.source.Buffered() == 0 {
			return written, nil
		}
		value, err := r.source.ReadByte()
		if err != nil {
			if written > 0 {
				return written, nil
			}
			return 0, err
		}
		r.pending = r.normalize(value)
	}
	return written, nil
}

func (r *sparseJSONReader) normalize(value byte) []byte {
	if r.inString {
		if r.escaped {
			r.escaped = false
		} else if value == '\\' {
			r.escaped = true
		} else if value == '"' {
			r.inString = false
		}
		return []byte{value}
	}
	if value == '"' {
		r.inString = true
		r.previousToken = value
		return []byte{value}
	}
	if value == ',' && (r.previousToken == '[' || r.previousToken == ',') {
		r.previousToken = value
		return []byte("null,")
	}
	if value == ']' && r.previousToken == ',' {
		r.previousToken = value
		return []byte("null]")
	}
	if !unicode.IsSpace(rune(value)) {
		r.previousToken = value
	}
	return []byte{value}
}

// DecodeStream streams frames from MakerSuiteService and calls the onEvent callback.
func DecodeStream(source io.Reader, onEvent func(event StreamEvent) error) error {
	decoder := json.NewDecoder(newSparseJSONReader(source))
	decoder.UseNumber()
	expect := func(want json.Delim) error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if token != want {
			return fmt.Errorf("expected %q, got %v", want, token)
		}
		return nil
	}
	if err := expect('['); err != nil {
		return fmt.Errorf("read root: %w", err)
	}
	if err := expect('['); err != nil {
		return fmt.Errorf("read frames: %w", err)
	}
	finished := false
	for decoder.More() {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return fmt.Errorf("decode frame: %w", err)
		}
		events, err := decodeFrame(raw)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.FinishReason != "" {
				finished = true
			}
			if err := onEvent(event); err != nil {
				return err
			}
		}
	}
	if err := expect(']'); err != nil {
		return fmt.Errorf("finish frames: %w", err)
	}
	index := 1
	for decoder.More() {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if index == 1 && string(raw) != "null" {
			if err := parseTrailerError(raw); err != nil {
				return err
			}
		}
		index++
	}
	if err := expect(']'); err != nil {
		return fmt.Errorf("finish root: %w", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("multiple response roots")
	}
	if !finished {
		return errors.New("AI Studio stream ended without a finish frame")
	}
	return nil
}

func decodeFrame(raw json.RawMessage) ([]StreamEvent, error) {
	var frame []json.RawMessage
	if err := json.Unmarshal(raw, &frame); err != nil {
		return nil, err
	}
	if len(frame) == 0 {
		return nil, errors.New("empty generation frame")
	}
	events := []StreamEvent{}
	if string(frame[0]) != "null" {
		var candidates [][]json.RawMessage
		if err := json.Unmarshal(frame[0], &candidates); err != nil {
			return nil, err
		}
		for _, candidate := range candidates {
			if len(candidate) > 0 && string(candidate[0]) != "null" {
				var content []json.RawMessage
				if err := json.Unmarshal(candidate[0], &content); err != nil {
					return nil, err
				}
				if len(content) > 0 {
					var parts []json.RawMessage
					if err := json.Unmarshal(content[0], &parts); err != nil {
						return nil, err
					}
					for _, rawPart := range parts {
						event, err := decodePart(rawPart)
						if err != nil {
							return nil, err
						}
						events = append(events, event)
					}
				}
			}
			if len(candidate) > 1 && string(candidate[1]) != "null" {
				var code int64
				if err := json.Unmarshal(candidate[1], &code); err != nil {
					return nil, err
				}
				if code == 22 {
					return nil, errors.New("AI Studio requested continuation without a final completion")
				}
				events = append(events, StreamEvent{FinishReason: mapFinishReason(code)})
			}
		}
	} else if len(frame) > 1 && string(frame[1]) != "null" {
		return nil, errors.New("AI Studio rejected the prompt without a candidate")
	}
	if len(frame) > 2 && string(frame[2]) != "null" {
		var values []json.RawMessage
		if err := json.Unmarshal(frame[2], &values); err != nil {
			return nil, err
		}
		if len(values) >= 3 && string(values[0]) != "null" && string(values[2]) != "null" {
			var usage UsageMetadata
			for index, target := range []*int{&usage.PromptTokenCount, &usage.CandidatesTokenCount, &usage.TotalTokenCount, &usage.CachedContentTokenCount} {
				if index < len(values) && string(values[index]) != "null" {
					if err := json.Unmarshal(values[index], target); err != nil {
						return nil, err
					}
				}
			}
			for index, target := range map[int]*int{7: &usage.ToolUsePromptTokenCount, 9: &usage.ThoughtsTokenCount} {
				if index < len(values) && string(values[index]) != "null" {
					if err := json.Unmarshal(values[index], target); err != nil {
						return nil, err
					}
				}
			}
			events = append(events, StreamEvent{Usage: &usage})
		}
	}
	return events, nil
}

func decodePart(raw json.RawMessage) (StreamEvent, error) {
	var part []json.RawMessage
	if err := json.Unmarshal(raw, &part); err != nil {
		return StreamEvent{}, err
	}
	event := StreamEvent{}
	if len(part) > 12 && string(part[12]) != "null" {
		switch string(part[12]) {
		case "1", "true":
			event.Thought = true
		case "0", "false":
		default:
			return event, errors.New("invalid thought enum")
		}
	}
	if len(part) > 14 && string(part[14]) != "null" {
		if err := json.Unmarshal(part[14], &event.ThoughtSignature); err != nil {
			return event, err
		}
	}
	if len(part) > 1 && string(part[1]) != "null" {
		if err := json.Unmarshal(part[1], &event.Text); err != nil {
			return event, err
		}
	}
	if len(part) > 2 && string(part[2]) != "null" {
		var data []string
		if err := json.Unmarshal(part[2], &data); err != nil {
			return event, err
		}
		if len(data) != 2 {
			return event, errors.New("invalid inline data")
		}
		event.InlineData = &antigravity.GeminiInlineData{MimeType: data[0], Data: data[1]}
	}
	if len(part) > 10 && string(part[10]) != "null" {
		var call []json.RawMessage
		if err := json.Unmarshal(part[10], &call); err != nil {
			return event, err
		}
		if len(call) < 1 {
			return event, errors.New("function call missing name")
		}
		function := &antigravity.GeminiFunctionCall{Args: map[string]any{}}
		if err := json.Unmarshal(call[0], &function.Name); err != nil {
			return event, err
		}
		if function.Name == "" {
			return event, errors.New("function call is missing its name")
		}
		if len(call) > 1 && string(call[1]) != "null" {
			args, err := decodeWireStruct(call[1])
			if err != nil {
				return event, err
			}
			function.Args = args
		}
		if len(call) > 2 && string(call[2]) != "null" {
			if err := json.Unmarshal(call[2], &function.ID); err != nil {
				return event, err
			}
		}
		event.FunctionCall = function
	}
	return event, nil
}

func mapFinishReason(code int64) string {
	switch code {
	case 1:
		return "STOP"
	case 2:
		return "MAX_TOKENS"
	case 3:
		return "SAFETY"
	case 4:
		return "RECITATION"
	case 5:
		return "OTHER"
	case 0:
		return ""
	case 6:
		return "LANGUAGE"
	case 7:
		return "BLOCKLIST"
	case 8:
		return "PROHIBITED_CONTENT"
	case 9:
		return "SPII"
	case 10:
		return "MALFORMED_FUNCTION_CALL"
	case 11:
		return "IMAGE_SAFETY"
	case 12:
		return "UNEXPECTED_TOOL_CALL"
	case 13:
		return "TOO_MANY_TOOL_CALLS"
	case 14:
		return "IMAGE_PROHIBITED_CONTENT"
	case 15:
		return "IMAGE_OTHER"
	case 16:
		return "NO_IMAGE"
	case 17:
		return "IMAGE_RECITATION"
	case 18:
		return "MISSING_THOUGHT_SIGNATURE"
	case 19:
		return "MALFORMED_RESPONSE"
	default:
		return fmt.Sprintf("FINISH_REASON_%d", code)
	}
}

func parseTrailerError(raw json.RawMessage) error {
	var trailer []json.RawMessage
	if err := json.Unmarshal(raw, &trailer); err != nil {
		return err
	}
	if len(trailer) == 0 {
		return errors.New("empty RPC trailer")
	}
	var code int
	if err := json.Unmarshal(trailer[0], &code); err != nil {
		return err
	}
	if code == 0 {
		return nil
	}
	var message string
	if len(trailer) > 1 {
		if err := json.Unmarshal(trailer[1], &message); err != nil {
			return err
		}
	}
	return &RPCError{Code: code, Message: message}
}
