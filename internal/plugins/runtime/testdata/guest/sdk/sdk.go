// Package sdk is a minimal guest side of the pm_v1 ABI for runtime tests.
package sdk

import (
	"encoding/json"
	"errors"

	"github.com/extism/go-pdk"
)

//go:wasmimport pm_v1 http_request
func hostHTTP(req uint64) uint64

//go:wasmimport pm_v1 secret_get
func hostSecret(name uint64) uint64

//go:wasmimport pm_v1 config_get
func hostConfig(name uint64) uint64

//go:wasmimport pm_v1 log
func hostLog(level int32, msg uint64)

type Request struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

type Response struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
	Error   string              `json:"error,omitempty"`
}

func Do(r Request) (Response, error) {
	b, _ := json.Marshal(r)
	m := pdk.AllocateBytes(b)
	defer m.Free()
	off := hostHTTP(m.Offset())
	var resp Response
	if off == 0 {
		return resp, errors.New("host returned nothing")
	}
	out := pdk.FindMemory(off)
	defer out.Free()
	if err := json.Unmarshal(out.ReadBytes(), &resp); err != nil {
		return resp, err
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

func str(fn func(uint64) uint64, name string) (string, bool) {
	m := pdk.AllocateString(name)
	defer m.Free()
	off := fn(m.Offset())
	if off == 0 {
		return "", false
	}
	out := pdk.FindMemory(off)
	defer out.Free()
	return string(out.ReadBytes()), true
}

// Wrapped in closures: TinyGo cannot take an imported function as a value.
func Secret(name string) (string, bool) {
	return str(func(n uint64) uint64 { return hostSecret(n) }, name)
}
func Config(name string) (string, bool) {
	return str(func(n uint64) uint64 { return hostConfig(n) }, name)
}

func Log(level int32, msg string) {
	m := pdk.AllocateString(msg)
	defer m.Free()
	hostLog(level, m.Offset())
}
