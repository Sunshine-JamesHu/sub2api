//go:build cgo

package aistudio

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSpiderMonkeyRuntimeInitializesAndExecutes(t *testing.T) {
	require.NoError(t, CheckRuntime())
}
