package aistudio

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWireBooleanNumericCompatibility(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{{"true", true}, {"false", false}, {"1", true}, {"0", false}} {
		t.Run(tc.raw, func(t *testing.T) {
			value, err := decodeWireValue(json.RawMessage(`[null,null,null,` + tc.raw + `]`))
			require.NoError(t, err)
			require.Equal(t, tc.want, value)
		})
	}
	for _, raw := range []string{`2`, `-1`, `1.5`, `"true"`, `{}`, `[]`} {
		t.Run("reject_"+raw, func(t *testing.T) {
			_, err := decodeWireValue(json.RawMessage(`[null,null,null,` + raw + `]`))
			require.Error(t, err)
		})
	}
}

func TestWireBooleanNumericFunctionCallStream(t *testing.T) {
	part := make([]any, 11)
	part[10] = []any{"inspect", []any{[]any{[]any{"enabled", []any{nil, nil, nil, 1}}, []any{"nested", []any{nil, nil, nil, nil, []any{[]any{[]any{"disabled", []any{nil, nil, nil, 0}}}}}}}}, "call1"}
	frame := []any{[]any{[]any{[]any{[]any{part}, "model"}, 1}}}
	raw, err := json.Marshal([]any{[]any{frame}})
	require.NoError(t, err)
	var events []StreamEvent
	require.NoError(t, DecodeStream(strings.NewReader(string(raw)), func(event StreamEvent) error { events = append(events, event); return nil }))
	require.Len(t, events, 2)
	require.Equal(t, "STOP", events[1].FinishReason)
	args, ok := events[0].FunctionCall.Args.(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, args["enabled"])
	require.Equal(t, map[string]any{"disabled": false}, args["nested"])
}
