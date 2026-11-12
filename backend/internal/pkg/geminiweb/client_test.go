package geminiweb

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func rpcFrame(rpc string, payload any) string {
	return marshal([]any{[]any{"wrb.fr", rpc, marshal(payload), nil, nil, nil}}) + "\n"
}
func statusFixture(entries ...any) string {
	status := make([]any, 18)
	status[15] = entries
	return ")]}'\n\n123\n" + rpcFrame("otAQ7b", status)
}
func modelEntry(id, family, display string, number int) []any {
	entry := make([]any, 20)
	entry[0], entry[1], entry[11], entry[17] = id, family, display, number
	return entry
}
func generationFrame(text, thought string) string {
	candidate := make([]any, 38)
	candidate[0], candidate[1] = "rc_test", []string{text}
	if thought != "" {
		candidate[37] = [][]string{{thought}}
	}
	return rpcFrame("", []any{nil, []string{"c_test", "r_test"}, nil, nil, []any{candidate}})
}
func testRequest() *antigravity.GeminiRequest {
	return &antigravity.GeminiRequest{Contents: []antigravity.GeminiContent{{Role: "user", Parts: []antigravity.GeminiPart{{Text: "你好"}}}}}
}
func response(text string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(text))}
}

func TestGeminiWebModelsAreAccountScoped(t *testing.T) {
	flash := modelEntry("fbb127bbb056c959", "Flash", "3.8 Flash", 1)
	argon := modelEntry("95a4c0abcba24364", "Argon", "4 Argon", 8)
	models, err := ParseModels([]byte(statusFixture(flash, argon)))
	require.NoError(t, err)
	require.Len(t, models, 2)
	model, err := ResolveModel(models, "gemini-argon")
	require.NoError(t, err)
	require.Equal(t, "95a4c0abcba24364", model.ID)
	other, err := ParseModels([]byte(statusFixture(flash)))
	require.NoError(t, err)
	_, err = ResolveModel(other, "gemini-argon")
	require.ErrorContains(t, err, "unavailable")
	_, err = ResolveModel(other, "gemini-3-pro-image")
	require.ErrorContains(t, err, "unavailable")
	model, err = ResolveModel(other, "gemini-2.5-flash")
	require.NoError(t, err)
	require.Equal(t, flash[0], model.ID)
	_, err = ParseModels([]byte(statusFixture(modelEntry("invalid", "Flash", "Flash", 1))))
	require.Error(t, err)
}

func TestGeminiWebGenerateBindsModelTokenAndFullHistory(t *testing.T) {
	calls := 0
	client := New(func(ctx context.Context, method, target string, body []byte, header http.Header) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			require.Equal(t, "GET", method)
			require.Contains(t, target, "/app")
			return response(`{"SNlM0e":"token\u003d","cfb2h":"build","FdrFJe":"session"}`), nil
		case 2:
			require.Contains(t, target, "rpcids=otAQ7b")
			return response(statusFixture(modelEntry("fbb127bbb056c959", "Flash", "3.8 Flash", 1))), nil
		case 3:
			require.Contains(t, target, "StreamGenerate")
			require.Contains(t, target, "bl=build")
			form, err := url.ParseQuery(string(body))
			require.NoError(t, err)
			require.Equal(t, "token=", form.Get("at"))
			var outer []any
			require.NoError(t, json.Unmarshal([]byte(form.Get("f.req")), &outer))
			var payload []any
			require.NoError(t, json.Unmarshal([]byte(outer[1].(string)), &payload))
			require.Len(t, payload, 81)
			require.Equal(t, float64(4), payload[80])
			require.Equal(t, float64(1), payload[79])
			prompt := str(at(payload, 0, 0))
			require.Contains(t, prompt, "system policy")
			require.Contains(t, prompt, "previous answer")
			require.Contains(t, prompt, "你好")
			var modelHeader []any
			require.NoError(t, json.Unmarshal([]byte(header.Get("x-goog-ext-525001261-jspb")), &modelHeader))
			require.Equal(t, "fbb127bbb056c959", modelHeader[4])
			require.Equal(t, float64(4), modelHeader[15])
			return response(generationFrame("回答", "")), nil
		default:
			t.Fatal("unexpected network request")
			return nil, nil
		}
	})
	req := testRequest()
	req.SystemInstruction = &antigravity.GeminiContent{Parts: []antigravity.GeminiPart{{Text: "system policy"}}}
	req.Contents = append([]antigravity.GeminiContent{{Role: "model", Parts: []antigravity.GeminiPart{{Text: "previous answer"}}}}, req.Contents...)
	req.GenerationConfig = &antigravity.GeminiGenerationConfig{ThinkingConfig: &antigravity.GeminiThinkingConfig{ThinkingLevel: "HIGH"}}
	resp, err := client.Generate(context.Background(), "gemini-flash", req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 3, calls)
}

type fragmentedReader struct{ io.Reader }

func (r fragmentedReader) Read(p []byte) (int, error) {
	if len(p) > 3 {
		p = p[:3]
	}
	return r.Reader.Read(p)
}

func TestGeminiWebStreamUnicodeCitationsThoughtsAndStop(t *testing.T) {
	req := testRequest()
	req.GenerationConfig = &antigravity.GeminiGenerationConfig{ThinkingConfig: &antigravity.GeminiThinkingConfig{IncludeThoughts: true}}
	source := ")]}'\n123\n" + generationFrame("你好[cit", "想") + generationFrame("你好[cite: 1]世界", "想好了")
	var out strings.Builder
	require.NoError(t, DecodeStream(fragmentedReader{strings.NewReader(source)}, &out, req))
	require.NotContains(t, out.String(), "cite")
	require.Contains(t, out.String(), "你好")
	require.Contains(t, out.String(), "世界")
	require.Contains(t, out.String(), `"thought":true`)
	require.Contains(t, out.String(), `"finishReason":"STOP"`)
	require.NotContains(t, out.String(), "usageMetadata")
}

func TestGeminiWebStreamDoesNotExposeUnrequestedThoughts(t *testing.T) {
	var out strings.Builder
	require.NoError(t, DecodeStream(strings.NewReader(generationFrame("answer", "private thought")), &out, testRequest()))
	require.NotContains(t, out.String(), "private thought")
}

func TestGeminiWebStreamToolsAndErrors(t *testing.T) {
	req := testRequest()
	req.Tools = []antigravity.GeminiToolDeclaration{{FunctionDeclarations: []antigravity.GeminiFunctionDecl{{Name: "lookup"}}}}
	var out strings.Builder
	require.NoError(t, DecodeStream(strings.NewReader(generationFrame(`{"tool_calls":[{"name":"lookup","arguments":{"id":1}}]}`, "")), &out, req))
	require.Contains(t, out.String(), `"functionCall"`)
	require.Contains(t, out.String(), `"name":"lookup"`)
	require.NotContains(t, out.String(), `"text"`)
	out.Reset()
	require.ErrorContains(t, DecodeStream(strings.NewReader(generationFrame(`{"tool_calls":[{"name":"undeclared","arguments":{}}]}`, "")), &out, req), "undeclared")
	for _, source := range []string{"[[broken\n", marshal([]any{[]any{"wrb.fr", nil, nil, nil, nil, []any{13, "BardErrorInfo"}}}) + "\n", ""} {
		out.Reset()
		require.Error(t, DecodeStream(strings.NewReader(source), &out, testRequest()))
		require.NotContains(t, out.String(), "STOP")
	}
	out.Reset()
	require.NoError(t, DecodeStream(strings.NewReader(generationFrame("BardErrorInfo is a quoted string", "")), &out, testRequest()))
}

func TestGeminiWebRefusesDroppedAttachmentsAndInvalidSessions(t *testing.T) {
	req := testRequest()
	req.Contents[0].Parts = append(req.Contents[0].Parts, antigravity.GeminiPart{InlineData: &antigravity.GeminiInlineData{MimeType: "image/png", Data: "invalid"}})
	client := New(func(context.Context, string, string, []byte, http.Header) (*http.Response, error) {
		t.Fatal("invalid attachment must not be uploaded")
		return nil, nil
	})
	_, attachments, err := buildPrompt(req)
	require.NoError(t, err)
	_, err = client.upload(context.Background(), attachments)
	require.ErrorContains(t, err, "base64")
	client = New(func(context.Context, string, string, []byte, http.Header) (*http.Response, error) {
		return response("signed-out page"), nil
	})
	require.ErrorIs(t, client.Initialize(context.Background()), ErrAuthenticationRequired)
}

func TestGeminiWebRejectsUnsupportedNativeParts(t *testing.T) {
	for _, body := range []string{
		`{"contents":[{"parts":[{"fileData":{"fileUri":"gs://file"}}]}]}`,
		`{"contents":[{"parts":[{"text":"hi"}]}],"cachedContent":"cachedContents/private"}`,
		`{"systemInstruction":{"parts":[{"fileData":{"fileUri":"gs://file"}}]},"contents":[{"parts":[{"text":"hi"}]}]}`,
	} {
		_, err := ParseRequest([]byte(body))
		require.Error(t, err)
	}
	request, err := ParseRequest([]byte(`{"contents":[{"parts":[{"text":"fileData is just a word"}]}]}`))
	require.NoError(t, err)
	require.Equal(t, "fileData is just a word", request.Contents[0].Parts[0].Text)
}

func TestGeminiWebUploadsAttachmentThroughAccountTransport(t *testing.T) {
	client := New(func(ctx context.Context, method, target string, body []byte, h http.Header) (*http.Response, error) {
		require.Equal(t, "POST", method)
		require.Equal(t, "https://content-push.googleapis.com/upload", target)
		require.Equal(t, "bard-storage", h.Get("X-Tenant-Id"))
		require.Equal(t, "feeds/synthetic", h.Get("Push-ID"))
		boundary := strings.Split(h.Get("Content-Type"), "boundary=")[1]
		part, err := multipart.NewReader(bytes.NewReader(body), boundary).NextPart()
		require.NoError(t, err)
		raw, err := io.ReadAll(part)
		require.NoError(t, err)
		require.Equal(t, []byte{1, 2, 3}, raw)
		require.Equal(t, "image/png", part.Header.Get("Content-Type"))
		return response("/contrib_service/ttl_1d/synthetic"), nil
	})
	client.pushID = "feeds/synthetic"
	files, err := client.upload(context.Background(), []*antigravity.GeminiInlineData{{MimeType: "image/png", Data: "AQID"}})
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "/contrib_service/ttl_1d/synthetic", files[0].([]any)[0].([]string)[0])
}
