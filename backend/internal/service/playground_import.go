package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// normalizePlaygroundCredentials accepts only a completed server-owned browser
// login grant. Raw cookies, caller-selected profiles and Chrome RT imports cannot
// establish or replace a Playground identity.
func (s *adminServiceImpl) normalizePlaygroundCredentials(ctx context.Context, platform, accountType string, proxyID *int64, incoming, existing map[string]any) (map[string]any, error) {
	if platform != PlatformGemini || accountType != AccountTypePlayground {
		return incoming, nil
	}
	next, err := cloneAccountJSONMap(incoming)
	if err != nil {
		return nil, err
	}
	if next == nil {
		next = map[string]any{}
	}
	backendValue, supplied := next["playground_backend"]
	if !supplied {
		backendValue = existing["playground_backend"]
	}
	backendString, _ := backendValue.(string)
	if backendValue != nil && backendString == "" {
		return nil, infraerrors.BadRequest("INVALID_PLAYGROUND_BACKEND", "Invalid Playground source")
	}
	backend, err := normalizePlaygroundBackend(backendString)
	if err != nil {
		return nil, infraerrors.BadRequest("INVALID_PLAYGROUND_BACKEND", err.Error())
	}
	next["playground_backend"] = backend
	for _, key := range []string{"cookies", "cookie", "playground_storage_state", "refresh_token", "wrapped_binding_key", "gaia_id"} {
		if value, ok := next[key]; ok && value != nil {
			return nil, infraerrors.BadRequest("PLAYGROUND_BROWSER_LOGIN_REQUIRED", "Cookie import has been removed; use Camoufox login")
		}
		delete(next, key)
	}
	token, _ := next["playground_login_token"].(string)
	delete(next, "playground_login_token")
	existingBackend, _ := existing["playground_backend"].(string)
	existingBackend, _ = normalizePlaygroundBackend(existingBackend)
	if len(existing) > 0 && backend != existingBackend && token == "" {
		return nil, infraerrors.BadRequest("PLAYGROUND_BROWSER_LOGIN_REQUIRED", "Log in to the selected Playground source before saving")
	}
	for _, key := range []string{"playground_profile_id", "playground_email", "playground_auth_source", "playground_auth_user"} {
		delete(next, key)
		if value, ok := existing[key]; ok {
			next[key] = value
		}
	}
	if token != "" {
		if s.playgroundBrowser == nil {
			return nil, infraerrors.ServiceUnavailable("PLAYGROUND_BROWSER_UNAVAILABLE", "Camoufox login service is unavailable")
		}
		proxy, err := s.playgroundBrowser.proxyURL(ctx, proxyID)
		if err != nil {
			return nil, err
		}
		grant, err := s.playgroundBrowser.Cache.ConsumeGrant(ctx, token)
		if err != nil {
			return nil, infraerrors.BadRequest("PLAYGROUND_LOGIN_EXPIRED", err.Error())
		}
		if grant.Proxy != aistudio.ComputePayloadDigest(proxy) {
			return nil, infraerrors.BadRequest("PLAYGROUND_PROXY_CHANGED", "Proxy changed after login; log in again using the selected proxy")
		}
		grantBackend, err := normalizePlaygroundBackend(grant.Backend)
		if err != nil || grantBackend != backend {
			return nil, infraerrors.BadRequest("PLAYGROUND_SOURCE_CHANGED", "Playground source changed after login; log in again using the selected source")
		}
		next["playground_profile_id"] = grant.Profile
		next["playground_email"] = grant.Email
		next["playground_auth_source"] = map[string]any{"browser": "camoufox", "email": grant.Email, "auto_refresh": true, "backend": backend}
	}
	if next["playground_profile_id"] == nil || next["playground_profile_id"] == "" {
		return nil, infraerrors.BadRequest("PLAYGROUND_BROWSER_LOGIN_REQUIRED", "Authorize this account with Camoufox before saving")
	}
	return next, nil
}
