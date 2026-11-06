//go:build !cgo

package mozjs

import (
	"errors"
	"time"
)

var errCgoRequired = errors.New("spidermonkey (mozjs) requires CGO_ENABLED=1")

type Handle uint32
type Kind int32

const (
	KindUndefined Kind = 0
	KindNull      Kind = 1
	KindBool      Kind = 2
	KindNumber    Kind = 3
	KindString    Kind = 4
	KindHandle    Kind = 5
)

type Value struct {
	Kind   Kind
	Bool   bool
	Number float64
	String string
	Handle Handle
}

func Undefined() Value           { return Value{} }
func Null() Value                { return Value{Kind: KindNull} }
func Bool(value bool) Value      { return Value{Kind: KindBool, Bool: value} }
func Number(value float64) Value { return Value{Kind: KindNumber, Number: value} }
func String(value string) Value  { return Value{Kind: KindString, String: value} }
func Object(handle Handle) Value { return Value{Kind: KindHandle, Handle: handle} }

type NativeFunc func(call *Call) error

type ThrowError struct {
	Type    string
	Message string
}

func (err *ThrowError) Error() string { return err.Type + ": " + err.Message }

type Error struct {
	Message  string
	Stack    string
	Filename string
	Line     int
	Column   int
	Value    Handle
}

func (err *Error) Error() string {
	if err.Stack != "" {
		return err.Message + "\n" + err.Stack
	}
	return err.Message
}

type Options struct {
	HeapBytes uint32
	Locale    string
}

type Context struct{}

type Call struct{}

func Init() error                                       { return errCgoRequired }
func NewContext(options Options) (*Context, error)      { return nil, errCgoRequired }
func SetCamouLocale(language, region, locale string)    {}
func Now() float64                                      { return float64(time.Now().UnixMilli()) }
func (cx *Context) Close()                              {}
func (cx *Context) Release(handle Handle)               {}
func (cx *Context) ReleaseValue(value Value)            {}
func (cx *Context) NewGlobal(same Handle, timezone string) (Handle, error) {
	return 0, errCgoRequired
}
func (cx *Context) Evaluate(global Handle, source, filename string, line int) (Value, error) {
	return Undefined(), errCgoRequired
}
func (cx *Context) Call(fn Handle, this Handle, args ...Value) (Value, error) {
	return Undefined(), errCgoRequired
}
func (cx *Context) Get(object Handle, name string) (Value, error) {
	return Undefined(), errCgoRequired
}
func (cx *Context) Set(object Handle, name string, value Value) error {
	return errCgoRequired
}
func (cx *Context) NewObject(global Handle) (Handle, error) {
	return 0, errCgoRequired
}
func (cx *Context) ParseJSON(global Handle, text string) (Value, error) {
	return Undefined(), errCgoRequired
}
func (cx *Context) NewFunction(global Handle, name string, nargs int, fn NativeFunc) (Handle, error) {
	return 0, errCgoRequired
}
func (cx *Context) InstallHostNatives(target Handle) error   { return errCgoRequired }
func (cx *Context) InstallWindowNatives(global Handle) error { return errCgoRequired }
func (cx *Context) RunJobs()                                {}
func (cx *Context) ClearKeptObjects()                       {}
func (cx *Context) SetMicrotaskReporter(fn Handle)          {}
func (cx *Context) FlushRejections(fn Handle) error         { return errCgoRequired }
func (cx *Context) SetTrustedTypes(registry Handle, block bool) {}
func (cx *Context) SetThrowLocation(filename string, line, column int) {}
func (cx *Context) Interrupt()                              {}
func (cx *Context) GC()                                     {}
func (cx *Context) ShrinkGC()                               {}

func (call *Call) Arg(index int) Value             { return Undefined() }
func (call *Call) ArgString(index int) string      { return "" }
func (call *Call) ArgNumber(index int) float64     { return 0 }
func (call *Call) ArgBytes(index int) ([]byte, bool) { return nil, false }
func (call *Call) Return(value Value)              {}
func (call *Call) ReturnBytes(data []byte)         {}
