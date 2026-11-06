package aistudio

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCapturedFingerprintCacheFormatRemainsCompatible(t *testing.T) {
	// A session already in Redis must retain its browser identity after upgrading.
	raw := []byte(`{"cookies":[],"origins":[],"playground_fingerprint":{"navigator.language":"zh-CN"}}`)
	var state StorageState
	require.NoError(t, json.Unmarshal(raw, &state))
	reencoded, err := json.Marshal(state)
	require.NoError(t, err)
	require.JSONEq(t, string(raw), string(reencoded))

	// Exporting a fingerprint takes a snapshot, not a reference to a live browser.
	config := map[string]any{"navigator.language": "en-US"}
	require.NoError(t, state.SetBrowserFingerprint(config))
	config["navigator.language"] = "changed"
	require.JSONEq(t, `{"navigator.language":"en-US"}`, string(state.Fingerprint))
}
