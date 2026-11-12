// Package geminiweb adapts the Gemini website's RPC protocol to Gemini API SSE.
// Protocol reference: https://github.com/xwteam/gemini2api (no vendored source).
package geminiweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/google/uuid"
)

const Origin = "https://gemini.google.com"
const AppURL = Origin + "/app"

var ErrAuthenticationRequired = errors.New("Gemini Web session requires browser login")

type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Transport uses the caller's browser fingerprint, proxy, cookie persistence and lease.
type Transport func(context.Context, string, string, []byte, http.Header) (*http.Response, error)

type Client struct {
	do                            Transport
	token, build, session, pushID string
	models                        []Model
}

func New(do Transport) *Client { return &Client{do: do} }

var pageField = regexp.MustCompile(`"(SNlM0e|cfb2h|FdrFJe)"\s*:\s*("(?:[^"\\]|\\.)*")`)
var pushField = regexp.MustCompile(`feeds/[a-zA-Z0-9_-]+`)

func headers() http.Header {
	return http.Header{"Origin": {Origin}, "Referer": {AppURL}, "X-Same-Domain": {"1"}}
}

func readResponse(resp *http.Response, limit int64) ([]byte, error) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("Gemini Web response exceeded size limit")
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 || (resp.StatusCode >= 300 && resp.StatusCode < 400) {
		return nil, ErrAuthenticationRequired
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Status: resp.StatusCode, Message: fmt.Sprintf("Gemini Web upstream HTTP %d", resp.StatusCode)}
	}
	return raw, nil
}

func (c *Client) Initialize(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, AppURL+"?hl=en", nil, headers())
	if err != nil {
		return err
	}
	raw, err := readResponse(resp, 16<<20)
	if err != nil {
		return err
	}
	for _, match := range pageField.FindAllSubmatch(raw, -1) {
		var value string
		if err := json.Unmarshal(match[2], &value); err != nil {
			return err
		}
		switch string(match[1]) {
		case "SNlM0e":
			c.token = value
		case "cfb2h":
			c.build = value
		case "FdrFJe":
			c.session = value
		}
	}
	if c.token == "" {
		return ErrAuthenticationRequired
	}
	c.pushID = pushField.FindString(string(raw))
	return nil
}

func (c *Client) target(path string) string {
	query := url.Values{"hl": {"en"}, "rt": {"c"}, "_reqid": {strconv.FormatInt(time.Now().UnixMilli()%900000+100000, 10)}}
	if c.build != "" {
		query.Set("bl", c.build)
	}
	if c.session != "" {
		query.Set("f.sid", c.session)
	}
	return Origin + path + "?" + query.Encode()
}

func marshal(value any) string { raw, _ := json.Marshal(value); return string(raw) }

func (c *Client) Models(ctx context.Context) ([]Model, []byte, error) {
	if c.token == "" {
		if err := c.Initialize(ctx); err != nil {
			return nil, nil, err
		}
	}
	target := c.target("/_/BardChatUi/data/batchexecute") + "&rpcids=otAQ7b&source-path=%2Fapp"
	body := url.Values{"at": {c.token}, "f.req": {marshal([]any{[]any{[]any{"otAQ7b", "[]", nil, "generic"}}})}}
	h := headers()
	h.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	h.Set("x-goog-ext-525001261-jspb", "[1,null,null,null,null,null,null,null,[4]]")
	h.Set("x-goog-ext-73010989-jspb", "[0]")
	resp, err := c.do(ctx, http.MethodPost, target, []byte(body.Encode()), h)
	if err != nil {
		return nil, nil, err
	}
	raw, err := readResponse(resp, 16<<20)
	if err != nil {
		return nil, nil, err
	}
	models, err := ParseModels(raw)
	if err != nil {
		return nil, nil, err
	}
	c.models = models
	return append([]Model(nil), models...), raw, nil
}

// Generate returns the upstream stream. Decoding remains incremental and belongs
// to the caller so a disconnected downstream closes the upstream immediately.
func (c *Client) Generate(ctx context.Context, name string, request *antigravity.GeminiRequest) (*http.Response, error) {
	if len(c.models) == 0 {
		if _, _, err := c.Models(ctx); err != nil {
			return nil, err
		}
	}
	model, err := ResolveModel(c.models, name)
	if err != nil {
		return nil, err
	}
	prompt, attachments, err := buildPrompt(request)
	if err != nil {
		return nil, err
	}
	files, err := c.upload(ctx, attachments)
	if err != nil {
		return nil, err
	}
	level := thinkingLevel(request)
	message := []any{prompt, 0, nil, nil, nil, nil, 0}
	if len(files) > 0 {
		message[3] = files
	}
	payload := []any{message, nil, nil, model.Name}
	h := headers()
	modelHeader := []any{1, nil, nil, nil, model.ID, nil, nil, 0, []int{4}, nil, nil, model.Capacity}
	if level > 0 {
		id := strings.ToUpper(uuid.NewString())
		payload = make([]any, 81)
		payload[0], payload[1], payload[2] = message, []string{"en"}, []any{"", "", "", nil, nil, nil, nil, nil, nil, ""}
		payload[6], payload[7], payload[10], payload[11] = []int{1}, 1, 1, 0
		payload[17], payload[18], payload[27], payload[30] = [][]int{{0}}, 0, 1, []int{4}
		payload[41], payload[53], payload[59], payload[61], payload[68] = []int{1}, 0, id, []any{}, 1
		payload[79], payload[80] = model.Number, level
		modelHeader[8] = []int{4, 5, 6, 8}
		modelHeader = append(modelHeader, nil, nil, model.Number, level, id)
		h.Set("x-goog-ext-525005358-jspb", marshal([]any{id, 1}))
	}
	h.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	h.Set("x-goog-ext-525001261-jspb", marshal(modelHeader))
	h.Set("x-goog-ext-73010989-jspb", "[0]")
	h.Set("x-goog-ext-73010990-jspb", "[0,0,0]")
	body := url.Values{"at": {c.token}, "f.req": {marshal([]any{nil, marshal(payload)})}}
	resp, err := c.do(ctx, http.MethodPost, c.target("/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"), []byte(body.Encode()), h)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_, err = readResponse(resp, 1<<20)
		return nil, err
	}
	return resp, nil
}
