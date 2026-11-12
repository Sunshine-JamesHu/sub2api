//go:build cgo

package aistudio

import (
	"errors"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio/waa/mozjs"
	"github.com/stretchr/testify/require"
	"os"
	"runtime"
	"testing"
)

func TestSpiderMonkeyRuntimeInitializesAndExecutes(t *testing.T) {
	require.NoError(t, CheckRuntime())
}

// Mozilla's dynamic env interposer must not replace libc in a static build.
// This also verifies that runtime initialization still works after env writes.
func TestSpiderMonkeyStaticEnvironmentMutation(t *testing.T) {
	t.Setenv("SUB2API_NATIVE_ENV_SMOKE", "synthetic")
	require.Equal(t, "synthetic", os.Getenv("SUB2API_NATIVE_ENV_SMOKE"))
	require.NoError(t, CheckRuntime())
}

// Exercise native recursion on a worker thread, where static musl defaults to
// a much smaller stack than the main thread. An exception must not kill the host.
func TestSpiderMonkeyWorkerStackExhaustionReturnsError(t *testing.T) {
	done := make(chan error, 1)
	err := mozjs.StartThread(func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		cx, err := mozjs.NewContext(mozjs.Options{HeapBytes: 64 << 20})
		if err != nil {
			done <- err
			return
		}
		defer cx.Close()
		global, err := cx.NewGlobal(0, "UTC")
		if err != nil {
			done <- err
			return
		}
		value, err := cx.Evaluate(global, "(function recurse(){ return 1 + recurse(); })()", "recursion-test", 1)
		cx.ReleaseValue(value)
		if err == nil {
			done <- fmt.Errorf("recursive script did not report stack exhaustion")
			return
		}
		var jsError *mozjs.Error
		if errors.As(err, &jsError) {
			cx.Release(jsError.Value)
		}
		value, err = cx.Evaluate(global, "1 + 1", "after-recursion", 1)
		if err == nil && value.Number != 2 {
			err = fmt.Errorf("context failed after recursion")
		}
		cx.ReleaseValue(value)
		done <- err
	})
	require.NoError(t, err)
	require.NoError(t, <-done)
}
