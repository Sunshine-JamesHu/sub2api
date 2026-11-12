package geminiweb

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// decodeFrames reads newline-delimited RPC envelopes with XSSI and length lines.
// Scanner enforces a bound while assembling arbitrarily fragmented network reads.
func decodeFrames(reader io.Reader, consume func([]any) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 32<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '[' {
			continue
		}
		var frames []any
		if err := json.Unmarshal(line, &frames); err != nil {
			return fmt.Errorf("decode Gemini Web RPC frame: %w", err)
		}
		for _, item := range frames {
			frame, ok := item.([]any)
			if !ok || len(frame) == 0 {
				continue
			}
			if err := frameError(frame); err != nil {
				return err
			}
			if err := consume(frame); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func frameError(frame []any) error {
	if str(at(frame, 0)) == "er" {
		return &Error{Status: 503, Message: "Gemini Web RPC rejected the request"}
	}
	// The payload in slot 2 is user-controlled model text. Never inspect it as
	// an error envelope (a prompt can ask the model to quote BardErrorInfo).
	for index, field := range frame {
		if index == 2 {
			continue
		}
		encoded, _ := json.Marshal(field)
		if bytes.Contains(encoded, []byte("BardErrorInfo")) {
			return &Error{Status: 503, Message: "Gemini Web refused generation (quota, permissions or verification); try another account or inspect the website"}
		}
	}
	return nil
}

var citation = regexp.MustCompile(`\[cite_start\]|\[cite:\s*[0-9,\s]+\]`)

func cleanText(text string, final bool) string {
	text = citation.ReplaceAllString(text, "")
	if !final {
		if index := strings.LastIndex(text, "["); index >= 0 {
			tail := text[index:]
			if strings.HasPrefix("[cite_start]", tail) || strings.HasPrefix("[cite:", tail) || strings.HasPrefix(tail, "[cite:") && !strings.Contains(tail, "]") {
				text = text[:index]
			}
		}
	}
	return text
}

// DecodeStream writes Gemini-native SSE; existing gateway adapters implement all
// external protocols. Token usage is omitted because the website does not report it.
func DecodeStream(reader io.Reader, writer io.Writer, req *antigravity.GeminiRequest) error {
	lastText, lastThought, sentText, sentThought := "", "", "", ""
	buffered := false
	for _, tool := range req.Tools {
		if len(tool.FunctionDeclarations) > 0 {
			buffered = true
		}
	}
	showThought := req.GenerationConfig != nil && req.GenerationConfig.ThinkingConfig != nil && req.GenerationConfig.ThinkingConfig.IncludeThoughts
	write := func(parts []antigravity.GeminiPart, finish string) error {
		candidate := antigravity.GeminiCandidate{Index: 0, FinishReason: finish}
		if len(parts) > 0 {
			candidate.Content = &antigravity.GeminiContent{Role: "model", Parts: parts}
		}
		raw, err := json.Marshal(antigravity.GeminiResponse{Candidates: []antigravity.GeminiCandidate{candidate}})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(writer, "data: %s\n\n", raw)
		return err
	}
	emit := func(text string, sent *string, thought bool) error {
		if text == "" || text == *sent {
			return nil
		}
		if !strings.HasPrefix(text, *sent) {
			return errors.New("Gemini Web rewrote already streamed content; retry the request")
		}
		delta := strings.TrimPrefix(text, *sent)
		if err := write([]antigravity.GeminiPart{{Text: delta, Thought: thought}}, ""); err != nil {
			return err
		}
		*sent = text
		return nil
	}
	err := decodeFrames(reader, func(frame []any) error {
		if str(at(frame, 0)) != "wrb.fr" {
			return nil
		}
		encoded := str(at(frame, 2))
		if encoded == "" {
			return nil
		}
		var payload []any
		if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
			return fmt.Errorf("decode Gemini Web generation: %w", err)
		}
		candidate := at(payload, 4, 0)
		if candidate == nil {
			return nil
		}
		if images, ok := at(candidate, 12, 7, 0).([]any); ok && len(images) > 0 {
			return &Error{Status: 400, Message: "Gemini Web image output is not supported by this adapter; use AI Studio Playground for image generation"}
		}
		if text := str(at(candidate, 1, 0)); text != "" {
			lastText = text
		}
		if thought := str(at(candidate, 37, 0, 0)); thought != "" {
			lastThought = thought
		}
		if showThought {
			if err := emit(cleanText(lastThought, false), &sentThought, true); err != nil {
				return err
			}
		}
		if !buffered {
			return emit(cleanText(lastText, false), &sentText, false)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if lastText == "" {
		return errors.New("Gemini Web returned no answer; check model access or quota on the website")
	}
	lastText = cleanText(lastText, true)
	if showThought {
		if err := emit(cleanText(lastThought, true), &sentThought, true); err != nil {
			return err
		}
	}
	if buffered {
		parts, err := toolParts(lastText, req)
		if err != nil {
			return err
		}
		if len(parts) > 0 {
			return write(parts, "STOP")
		}
	}
	if err := emit(lastText, &sentText, false); err != nil {
		return err
	}
	return write(nil, "STOP")
}
