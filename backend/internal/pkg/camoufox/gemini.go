package camoufox

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
)

const GeminiOrigin = "https://gemini.google.com"

func (m *Manager) StartGemini(ctx context.Context, profile, proxy string) (*Browser, error) {
	return m.start(ctx, profile, proxy, 1, GeminiOrigin+"/app")
}

func (m *Manager) StartGeminiBatch(ctx context.Context, profile, proxy string) (*Browser, error) {
	return m.start(ctx, profile, proxy, 2, GeminiOrigin+"/app")
}

func (m *Manager) StartGeminiManualResume(ctx context.Context, profile, proxy, resume string) (*Browser, error) {
	if !validGoogleResumeURL(resume) {
		resume = GeminiOrigin + "/app"
	}
	b, err := m.start(ctx, profile, proxy, 1, resume)
	if err == nil {
		b.origin = GeminiOrigin
	}
	return b, err
}

func (m *Manager) RecoverGemini(parent context.Context, profile, proxy, email string) (aistudio.StorageState, error) {
	if err := m.requireRecoveryProfile(profile); err != nil {
		return aistudio.StorageState{}, err
	}
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	b, err := m.StartGemini(ctx, profile, proxy)
	if err != nil {
		return aistudio.StorageState{}, err
	}
	defer b.Close()
	deadline, _ := ctx.Deadline()
	return pollRecoveredState(parent, time.Until(deadline), time.Second, func(ctx context.Context) (aistudio.StorageState, string, error) { return b.Capture(ctx, email) })
}
