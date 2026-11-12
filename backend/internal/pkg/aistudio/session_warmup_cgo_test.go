//go:build cgo

package aistudio

import (
	"context"
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio/waa"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Synthetic interpreter exercises the real native VM and request binding. The
// transport has no network access and rejects any generation during warmup.
func TestSessionWarmupReusesNativeVMWithFreshRequestProofs(t *testing.T) {
	interpreter := `globalThis.warmupFixture={a:function(program,ready){ready(function(done,options){done('!fixture-'+options[0].content)},function(){})}};`
	challenge, _ := json.Marshal([]any{[]any{"fixture", []string{interpreter}, []any{}, waa.InterpreterHash([]byte(interpreter)), "fixture-program", "warmupFixture", nil, ""}})
	state := StorageState{Cookies: []StateCookie{
		{Name: "__Secure-1PSID", Value: "sid", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
		{Name: "SAPISID", Value: "sap", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
		{Name: "__Secure-1PAPISID", Value: "one", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
		{Name: "__Secure-3PAPISID", Value: "three", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
	}}
	var calls, generated, pages atomic.Int32
	client := &http.Client{Transport: protocolRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		body := "[]"
		switch {
		case r.URL.Path == "/prompts/new_chat":
			pages.Add(1)
			body = `<html><script>var cfg={"WIu0Nc":"fixture-page-key"}</script></html>`
		case r.URL.Host == "waa-pa.clients6.google.com":
			body = string(challenge)
		case strings.HasSuffix(r.URL.Path, "/GetLoggingContext"):
			body = `["fixture",1]`
		case strings.HasSuffix(r.URL.Path, "/GetAiStudioBenefitTier"):
			body = `[1]`
		case strings.HasSuffix(r.URL.Path, "/GenerateContent"):
			generated.Add(1)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	manager := &WAAManager{}
	load := func() (SessionOptions, error) {
		return SessionOptions{State: state, Version: "fixture", Client: client}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, release, err := manager.AcquireFresh(ctx, "account", load)
	require.NoError(t, err)
	require.NoError(t, session.Prepare(ctx, ""))
	require.True(t, session.Ready())
	initialVM := session.vm
	beforeMaintenance := calls.Load()
	session.maintenanceAt = time.Now().Add(-11 * time.Minute)
	require.NoError(t, session.Warmup(ctx))
	require.Equal(t, beforeMaintenance+1, calls.Load())
	require.NoError(t, session.Warmup(ctx))
	require.Equal(t, beforeMaintenance+1, calls.Load())
	initialCalls := calls.Load()
	require.Positive(t, initialCalls)
	require.Zero(t, generated.Load())
	release()
	again, finish, err := manager.AcquireFresh(ctx, "account", load)
	require.NoError(t, err)
	defer finish()
	defer again.Close()
	first, err := again.Proof(ctx, "gemini-fixture", ComputePayloadDigest("first"), "first")
	require.NoError(t, err)
	second, err := again.Proof(ctx, "gemini-fixture", ComputePayloadDigest("second"), "second")
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	require.Same(t, initialVM, again.vm)
	require.Equal(t, initialCalls, calls.Load())
	require.Zero(t, generated.Load())
	finish()
	manager.Trim(0, 0)
	require.False(t, session.resident.Load())
	require.True(t, session.pageLoaded.Load())
	resumed, done, err := manager.AcquireFresh(ctx, "account", load)
	require.NoError(t, err)
	defer done()
	require.NoError(t, resumed.Prepare(ctx, ""))
	require.True(t, resumed.Ready())
	require.NotSame(t, initialVM, resumed.vm)
	require.EqualValues(t, 1, pages.Load())
	require.Equal(t, initialCalls+1, calls.Load())
	require.Zero(t, generated.Load())
}
