package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type geminiWebTestTransport func(*http.Request) (*http.Response, error)

func (f geminiWebTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func newGeminiWebProtocolFixture(t *testing.T, output string) (*PlaygroundGatewayService, *Account, *atomic.Int32) {
	t.Helper()
	repo, _ := newPlaygroundTestAccount(t)
	repo.account.Credentials["playground_backend"] = "gemini_web"
	repo.account.Credentials["model_mapping"] = map[string]any{"alias": "gemini-flash"}
	account, err := repo.GetByID(context.Background(), repo.account.ID)
	require.NoError(t, err)
	gateway, _ := newPlaygroundTestGateway(t, repo)
	state := playgroundState("synthetic")
	fingerprint, err := aistudio.NewBrowserFingerprint()
	require.NoError(t, err)
	require.NoError(t, state.SetBrowserFingerprint(fingerprint))
	lease, err := gateway.browser.Cache.Acquire(context.Background(), account.GetCredential("playground_profile_id"))
	require.NoError(t, err)
	require.NoError(t, lease.Put(context.Background(), playgroundCacheKey(account.GetCredential("playground_profile_id"), "", "selected@example.test"), state))
	lease.Release(context.Background())
	calls := &atomic.Int32{}
	encode := func(v any) string { b, e := json.Marshal(v); require.NoError(t, e); return string(b) }
	frame := func(rpc string, payload any) string {
		return encode([]any{[]any{"wrb.fr", rpc, encode(payload)}}) + "\n"
	}
	client := &http.Client{Transport: geminiWebTestTransport(func(req *http.Request) (*http.Response, error) {
		text := ""
		switch {
		case req.Method == "GET":
			text = `{"SNlM0e":"synthetic-token"}`
		case strings.Contains(req.URL.Path, "batchexecute"):
			entry := make([]any, 18)
			entry[0], entry[1], entry[11], entry[17] = "fbb127bbb056c959", "Flash", "3.8 Flash", 1
			status := make([]any, 18)
			status[15] = []any{entry}
			text = frame("otAQ7b", status)
		case strings.Contains(req.URL.Path, "StreamGenerate"):
			calls.Add(1)
			if output == "rejected" {
				text = encode([]any{[]any{"wrb.fr", nil, nil, nil, nil, []any{13, "BardErrorInfo"}}}) + "\n"
			} else {
				candidate := []any{"rc_test", []string{output}}
				text = frame("", []any{nil, []string{"c_test", "r_test"}, nil, nil, []any{candidate}})
			}
		default:
			t.Errorf("unexpected upstream %s", req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(text)), Request: req}, nil
	})}
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	seed, release, err := gateway.waaManager.AcquireFresh(context.Background(), strconv.FormatInt(account.ID, 10), func() (aistudio.SessionOptions, error) {
		return aistudio.SessionOptions{State: state, Version: aistudio.ComputePayloadDigest(string(raw)), Client: client}, nil
	})
	require.NoError(t, err)
	release()
	t.Cleanup(seed.Close)
	return gateway, account, calls
}

func TestGeminiWebAllProtocolsUseWebTransport(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "messages", "native"} {
		for _, stream := range []bool{false, true} {
			t.Run(protocol+"/"+strconv.FormatBool(stream), func(t *testing.T) {
				gateway, account, calls := newGeminiWebProtocolFixture(t, "OK")
				svc := &GeminiMessagesCompatService{cfg: &config.Config{}, playgroundGatewayService: gateway}
				streamValue := strconv.FormatBool(stream)
				body := []byte(`{"model":"alias","messages":[{"role":"user","content":"Hi"}],"max_tokens":1024,"stream":` + streamValue + `}`)
				if protocol == "responses" {
					body = []byte(`{"model":"alias","input":"Hi","stream":` + streamValue + `}`)
				}
				if protocol == "native" {
					body = []byte(`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`)
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/v1/"+protocol, bytes.NewReader(body))
				var result *ForwardResult
				var err error
				switch protocol {
				case "responses":
					result, err = svc.ForwardAsResponses(context.Background(), c, account, body)
				case "chat":
					result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body)
				case "messages":
					result, err = svc.Forward(context.Background(), c, account, body)
				case "native":
					result, err = svc.ForwardNative(context.Background(), c, account, "alias", "generateContent", stream, body)
				}
				require.NoError(t, err)
				require.Equal(t, int32(1), calls.Load())
				require.Equal(t, "gemini-flash", result.UpstreamModel)
				require.Contains(t, rec.Body.String(), "OK")
				require.Equal(t, 200, rec.Code)
				require.NotContains(t, rec.Body.String(), "response.failed")
			})
		}
	}
}

func TestGeminiWebRejectionIsNotAnEmptySuccessfulStream(t *testing.T) {
	gateway, account, _ := newGeminiWebProtocolFixture(t, "rejected")
	resp, err := gateway.DoGemini(context.Background(), account, "gemini-flash", []byte(`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestGeminiWebNativeModelsAndCountTokens(t *testing.T) {
	gateway, account, calls := newGeminiWebProtocolFixture(t, "OK")
	svc := &GeminiMessagesCompatService{cfg: &config.Config{}, playgroundGatewayService: gateway}
	list, err := svc.ForwardListModels(context.Background(), account)
	require.NoError(t, err)
	require.Contains(t, string(list.Body), "gemini-flash")
	model, err := svc.ForwardGetModel(context.Background(), account, "gemini-flash")
	require.NoError(t, err)
	require.Equal(t, 200, model.StatusCode)
	missing, err := svc.ForwardGetModel(context.Background(), account, "gemini-argon")
	require.NoError(t, err)
	require.Equal(t, 404, missing.StatusCode)
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1beta/models/alias:countTokens", bytes.NewReader(body))
	_, err = svc.ForwardNative(context.Background(), c, account, "alias", "countTokens", false, body)
	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), "totalTokens")
	require.Zero(t, calls.Load())
}
