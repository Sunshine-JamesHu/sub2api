package main

import (
	"fmt"
	"runtime"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio/waa/mozjs"
)

func main() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := mozjs.Init(); err != nil {
		panic(err)
	}
	fmt.Println("SpiderMonkey initialized successfully!")

	cx, err := mozjs.NewContext(mozjs.Options{HeapBytes: 16 << 20, Locale: "en-US"})
	if err != nil {
		panic(err)
	}
	defer cx.Close()

	global, err := cx.NewGlobal(0, "UTC")
	if err != nil {
		panic(err)
	}

	val, err := cx.Evaluate(global, "1 + 2 * 3", "test.js", 1)
	if err != nil {
		panic(err)
	}
	fmt.Printf("JS evaluation result: 1 + 2 * 3 = %v\n", val.Number)
}
