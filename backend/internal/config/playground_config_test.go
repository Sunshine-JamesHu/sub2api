package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlaygroundSessionCacheTTLConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		want      int
		invalid   bool
	}{
		{"default", "", 1800, false},
		{"override", "600", 600, false},
		{"minimum", "60", 60, false},
		{"maximum", "86400", 86400, false},
		{"too short", "59", 0, true},
		{"too long", "86401", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("GATEWAY_PLAYGROUND_SESSION_CACHE_TTL_SECONDS", tc.env)
			cfg, err := Load()
			if tc.invalid {
				require.ErrorContains(t, err, "playground_session_cache_ttl_seconds")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.Gateway.PlaygroundSessionCacheTTLSeconds)
		})
	}
}
