// demo: the runtime test plugin. Benign exports exercise the pm_v1 ABI;
// hostile ones try to escape and must fail.
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/extism/go-pdk"

	"pmguest/sdk"
)

//go:wasmexport echo
func echo() int32 { pdk.Output(pdk.Input()); return 0 }

// whoami returns the "token" secret this call sees.
//
//go:wasmexport whoami
func whoami() int32 {
	v, _ := sdk.Secret("token")
	pdk.OutputString(v)
	return 0
}

// config returns config field <input>, or "!" when absent.
//
//go:wasmexport config
func config() int32 {
	v, ok := sdk.Config(pdk.InputString())
	if !ok {
		v = "!"
	}
	pdk.OutputString(v)
	return 0
}

// secret returns secret field <input>, or "!" when refused.
//
//go:wasmexport secret
func secret() int32 {
	v, ok := sdk.Secret(pdk.InputString())
	if !ok {
		v = "!"
	}
	pdk.OutputString(v)
	return 0
}

var calls int

// counter shows whether guest memory survives between calls.
//
//go:wasmexport counter
func counter() int32 {
	calls++
	pdk.OutputString(fmt.Sprint(calls))
	return 0
}

//go:wasmexport test
func test() int32 { pdk.OutputString(`{"ok":true}`); return 0 }

// drop and danger stand in for a write op and a sensitive op.
//
//go:wasmexport drop
func drop() int32 { pdk.OutputString(`{"dropped":true}`); return 0 }

//go:wasmexport danger
func danger() int32 { pdk.OutputString(`{"danger":true}`); return 0 }

//go:wasmexport fail
func fail() int32 {
	pdk.SetError(errors.New("boom"))
	return 1
}

// fetch GETs <input> through pm_v1.http_request.
//
//go:wasmexport fetch
func fetch() int32 {
	r, err := sdk.Do(sdk.Request{URL: pdk.InputString()})
	if err != nil {
		pdk.OutputString("ERR " + err.Error())
		return 0
	}
	pdk.OutputString(fmt.Sprintf("OK %d %s", r.Status, r.Body))
	return 0
}

// nap sleeps 1 s (time.Sleep must block, not spin).
//
//go:wasmexport nap
func nap() int32 { time.Sleep(time.Second); return 0 }

//go:wasmexport spin
func spin() int32 {
	for {
	}
}

var hoard [][]byte

//go:wasmexport membomb
func membomb() int32 {
	for {
		hoard = append(hoard, make([]byte, 1<<20))
	}
}

//go:wasmexport extism_http
func extismHTTP() int32 {
	r := pdk.NewHTTPRequest(pdk.MethodGet, "https://example.com/").Send()
	pdk.OutputString(fmt.Sprint(r.Status()))
	return 0
}

//go:wasmexport wasi
func wasi() int32 {
	var res []string
	_, err := os.ReadFile("/etc/passwd")
	res = append(res, fmt.Sprintf("file:%v", err != nil))
	ents, err := os.ReadDir("/")
	res = append(res, fmt.Sprintf("dir:%v/%d", err != nil, len(ents)))
	res = append(res, fmt.Sprintf("env:%d", len(os.Environ())))
	_, err = net.Dial("tcp", "127.0.0.1:22")
	res = append(res, fmt.Sprintf("dial:%v", err != nil))
	pdk.OutputString(strings.Join(res, ","))
	return 0
}

//go:wasmexport stdout_flood
func stdoutFlood() int32 {
	line := strings.Repeat("x", 1023) + "\n"
	for i := 0; i < 256; i++ {
		os.Stdout.WriteString(line)
		os.Stderr.WriteString(line)
	}
	return 0
}

//go:wasmexport log_flood
func logFlood() int32 {
	for i := 0; i < 10000; i++ {
		sdk.Log(1, "spam")
	}
	return 0
}

//go:wasmimport pm_v1 http_request
func rawHTTP(uint64) uint64

//go:wasmexport badptr
func badptr() int32 {
	off := rawHTTP(0xFFFFFFFF0)
	m := pdk.AllocateString("{not json")
	mm := pdk.FindMemory(rawHTTP(m.Offset()))
	pdk.OutputString(fmt.Sprintf("%d %s", off, mm.ReadBytes()))
	return 0
}

func main() {}
