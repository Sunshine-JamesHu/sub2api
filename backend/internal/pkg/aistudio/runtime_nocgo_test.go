//go:build !cgo

package aistudio

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSpiderMonkeyMissingRuntimeIsReported(t *testing.T) {
	err := CheckRuntime()
	require.Error(t, err)
	require.Contains(t, err.Error(), "CGO_ENABLED=1")
}
