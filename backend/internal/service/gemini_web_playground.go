package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
)

func normalizePlaygroundBackend(value string) (string, error) {
	switch value {
	case "", "aistudio":
		return "aistudio", nil
	case "gemini_web":
		return value, nil
	default:
		return "", errors.New("Playground source must be aistudio or gemini_web")
	}
}
func isGeminiWebPlayground(account *Account) bool {
	return account != nil && account.Platform == PlatformGemini && account.Type == AccountTypePlayground && account.GetCredential("playground_backend") == "gemini_web"
}

func geminiWebError(err error) error {
	if errors.Is(err, geminiweb.ErrAuthenticationRequired) {
		return fmt.Errorf("%w: Gemini Web requires browser reauthorization", aistudio.ErrAuthenticationRequired)
	}
	return err
}

func webErrorResponse(err error) *http.Response {
	var upstream *geminiweb.Error
	if !errors.As(err, &upstream) {
		return nil
	}
	status := upstream.Status
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	name := "INTERNAL"
	switch status {
	case 400:
		name = "INVALID_ARGUMENT"
	case 404:
		name = "NOT_FOUND"
	case 429:
		name = "RESOURCE_EXHAUSTED"
	case 503:
		name = "UNAVAILABLE"
	}
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{"code": status, "message": upstream.Message, "status": name}})
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}
}

func (s *PlaygroundGatewayService) doGeminiWeb(ctx context.Context, account *Account, model string, body []byte) (*http.Response, error) {
	request, err := geminiweb.ParseRequest(body)
	if err != nil {
		if response := webErrorResponse(err); response != nil {
			return response, nil
		}
		return nil, err
	}
	// A total budget prevents website heartbeat frames from retaining leases indefinitely.
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	session, release, err := s.acquireSession(runCtx, account)
	if err != nil {
		cancel()
		return nil, geminiWebError(err)
	}
	runCtx = s.sessionContext(session)
	fresh := s.sessionAccount(session)
	if !isGeminiWebPlayground(fresh) {
		release()
		cancel()
		return nil, errPlaygroundSessionChanged
	}
	done := func() { cancel(); release() }
	var upstream *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		client := geminiweb.New(session.Do)
		upstream, err = client.Generate(runCtx, model, request)
		if errors.Is(err, geminiweb.ErrAuthenticationRequired) && attempt == 0 {
			if err = s.refreshSession(session); err == nil {
				continue
			}
		}
		break
	}
	if err != nil {
		err = geminiWebError(err)
		s.markManualActionFailure(runCtx, fresh, err)
		done()
		if response := webErrorResponse(err); response != nil {
			return response, nil
		}
		return nil, err
	}
	reader, writer := io.Pipe()
	go func() {
		defer done()
		defer upstream.Body.Close()
		err := geminiweb.DecodeStream(upstream.Body, writer, request)
		_ = writer.CloseWithError(err)
	}()
	// Reject quota and protocol errors before committing HTTP 200 downstream.
	buffered := bufio.NewReader(reader)
	if _, err := buffered.Peek(1); err != nil {
		_ = reader.Close()
		cancel()
		if response := webErrorResponse(err); response != nil {
			return response, nil
		}
		return nil, err
	}
	stream := &geminiWebStreamBody{Reader: buffered, Closer: reader}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &playgroundBody{ReadCloser: stream, done: cancel}}, nil
}

type geminiWebStreamBody struct {
	io.Reader
	io.Closer
}

func (s *PlaygroundGatewayService) geminiWebModels(ctx context.Context, account *Account) ([]string, []byte, error) {
	session, release, err := s.acquireSession(ctx, account)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	ctx = s.sessionContext(session)
	if !isGeminiWebPlayground(s.sessionAccount(session)) {
		return nil, nil, errPlaygroundSessionChanged
	}
	for attempt := 0; attempt < 2; attempt++ {
		models, _, err := geminiweb.New(session.Do).Models(ctx)
		if errors.Is(err, geminiweb.ErrAuthenticationRequired) && attempt == 0 {
			if err = s.refreshSession(session); err != nil {
				return nil, nil, err
			}
			continue
		}
		if err != nil {
			return nil, nil, geminiWebError(err)
		}
		names := make([]string, 0, len(models))
		for _, model := range models {
			names = append(names, model.Name)
		}
		raw, err := json.Marshal(map[string]any{"models": models})
		return names, raw, err
	}
	return nil, nil, geminiWebError(geminiweb.ErrAuthenticationRequired)
}
