package camoufox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestProfilePathRejectsTraversal(t *testing.T) {
	manager := NewManager("", t.TempDir())
	for _, id := range []string{"../other", "C:/profile", "", "abc"} {
		_, err := manager.ProfilePath(id)
		require.Error(t, err)
	}
	id := uuid.NewString()
	path, err := manager.ProfilePath(id)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(manager.Root, id), path)
}

func TestRemoveProfilePreservesNeighborsAndLinkedFiles(t *testing.T) {
	manager := NewManager("", t.TempDir())
	defer manager.Close()
	profile := uuid.NewString()
	directory, err := manager.ProfilePath(profile)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(directory, 0700))
	neighbor := filepath.Join(manager.Root, uuid.NewString())
	require.NoError(t, os.Mkdir(neighbor, 0700))
	keep := filepath.Join(neighbor, "keep.txt")
	require.NoError(t, os.WriteFile(keep, []byte("keep"), 0600))
	if err := os.Symlink(neighbor, filepath.Join(directory, "link")); err != nil {
		t.Logf("symlink unavailable; testing directory isolation only: %v", err)
	}
	require.Error(t, manager.RemoveProfile("../"))
	require.NoError(t, manager.RemoveProfile(profile))
	require.NoDirExists(t, directory)
	require.FileExists(t, keep)
	require.NoError(t, manager.RemoveProfile(profile))
}

// Explicit opt-in: launches only a fresh synthetic profile, never the user's
// browser profile. Verifies process exit and durable cookie persistence.
func TestBrowserPersistentProfileSmoke(t *testing.T) {
	if os.Getenv("CAMOUFOX_SMOKE") != "1" {
		t.Skip("set CAMOUFOX_SMOKE=1 with a matching installed Camoufox")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	executable, err := FindExecutable(ctx, os.Getenv("CAMOUFOX_DOWNLOAD_PROXY"))
	require.NoError(t, err)
	manager := NewManager(executable, t.TempDir())
	id := uuid.NewString()
	browser, err := manager.Start(ctx, id, os.Getenv("CAMOUFOX_DOWNLOAD_PROXY"))
	var failure *commandError
	if errors.As(err, &failure) {
		t.Log(failure.Detail)
	}
	require.NoError(t, err)
	defer browser.Close()
	_, err = browser.Command(ctx, "storage.setCookie", map[string]any{"cookie": map[string]any{"name": "sub2-profile-smoke", "value": map[string]string{"type": "string", "value": "synthetic"}, "domain": "example.test", "path": "/", "secure": true, "expiry": time.Now().Add(time.Hour).Unix()}})
	require.NoError(t, err)
	frame, err := browser.Screenshot(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, frame)
	_, err = browser.Command(ctx, "browsingContext.navigate", map[string]any{"context": browser.tab, "url": "data:text/html,<input id='smoke'>", "wait": "complete"})
	require.NoError(t, err)
	_, err = browser.Evaluate(ctx, `document.getElementById('smoke').focus();'ready'`)
	require.NoError(t, err)
	require.NoError(t, browser.Input(ctx, Input{Type: "text", Text: "synthetic"}))
	value, err := browser.Evaluate(ctx, `document.getElementById('smoke').value`)
	require.NoError(t, err)
	require.Equal(t, "synthetic", value)
	checkCaptureIdentityInBrowser(t, ctx, browser)
	browser.Close()
	select {
	case <-browser.done:
	case <-time.After(time.Second):
		t.Fatal("browser did not exit")
	}
	reopened, err := manager.Start(ctx, id, os.Getenv("CAMOUFOX_DOWNLOAD_PROXY"))
	require.NoError(t, err)
	defer reopened.Close()
	raw, err := reopened.Command(ctx, "storage.getCookies", map[string]any{})
	require.NoError(t, err)
	var cookies struct {
		Cookies []struct {
			Name string `json:"name"`
		} `json:"cookies"`
	}
	require.NoError(t, json.Unmarshal(raw, &cookies))
	found := false
	for _, cookie := range cookies.Cookies {
		if cookie.Name == "sub2-profile-smoke" {
			found = true
		}
	}
	require.True(t, found, "persistent profile lost its cookie after reopening")
}
