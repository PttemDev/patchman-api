package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	extism "github.com/extism/go-sdk"
)

// Per-call limits on what the guest can push through the host.
const (
	maxLogsPerCall   = 100
	maxLogLine       = 4096
	maxResponseBytes = 8 << 20
	maxRequestBytes  = 8 << 20
)

type callState struct {
	rt     *Runtime
	e      *entry
	conn   *Connection
	client *http.Client
	logs   []string
}

type stateKey struct{}

func state(ctx context.Context) *callState { s, _ := ctx.Value(stateKey{}).(*callState); return s }

func (st *callState) log(level, msg string) {
	if len(st.logs) >= maxLogsPerCall {
		return
	}
	if len(msg) > maxLogLine {
		msg = msg[:maxLogLine]
	}
	st.logs = append(st.logs, level+": "+msg)
	st.rt.opts.Log(st.e.id, st.conn.ID, level, msg)
}

// HTTPReq / HTTPResp are the pm_v1.http_request wire format (JSON).
type HTTPReq struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

type HTTPResp struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
	Error   string              `json:"error,omitempty"`
}

var i64 = []extism.ValueType{extism.ValueTypeI64}

// pmV1Names is the pm_v1 ABI this build provides (install-time import check).
var pmV1Names = map[string]bool{"http_request": true, "secret_get": true, "config_get": true, "log": true}

// hostFunctions is the whole pm_v1 ABI for v1: HTTP, the connection's
// declared secrets and config, and logging. TCP is v1.1 (ADR-5).
func hostFunctions() []extism.HostFunction {
	fns := []extism.HostFunction{
		extism.NewHostFunctionWithStack("http_request", hostHTTP, i64, i64),
		extism.NewHostFunctionWithStack("secret_get", hostSecret, i64, i64),
		extism.NewHostFunctionWithStack("config_get", hostConfig, i64, i64),
		extism.NewHostFunctionWithStack("log", hostLog, []extism.ValueType{extism.ValueTypeI32, extism.ValueTypeI64}, nil),
	}
	for i := range fns {
		fns[i].SetNamespace("pm_v1")
	}
	return fns
}

func writeJSON(cp *extism.CurrentPlugin, v any) uint64 {
	b, _ := json.Marshal(v)
	off, err := cp.WriteBytes(b)
	if err != nil {
		return 0
	}
	return off
}

func hostHTTP(ctx context.Context, cp *extism.CurrentPlugin, stack []uint64) {
	st := state(ctx)
	fail := func(err error) { stack[0] = writeJSON(cp, HTTPResp{Error: err.Error()}) }
	raw, err := cp.ReadBytes(stack[0])
	if err != nil || st == nil {
		fail(errors.New("bad request pointer"))
		return
	}
	if len(raw) > maxRequestBytes {
		fail(fmt.Errorf("request exceeds %d bytes", maxRequestBytes))
		return
	}
	var r HTTPReq
	if err := json.Unmarshal(raw, &r); err != nil {
		fail(fmt.Errorf("bad request json: %w", err))
		return
	}
	if r.Method == "" {
		r.Method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, bytes.NewReader(r.Body))
	if err != nil {
		fail(err)
		return
	}
	if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
		fail(fmt.Errorf("scheme %q not allowed", req.URL.Scheme))
		return
	}
	if req.URL.User != nil {
		fail(errors.New("userinfo in URL not allowed"))
		return
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	resp, err := st.client.Do(req)
	if err != nil {
		fail(err)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		fail(err)
		return
	}
	if len(body) > maxResponseBytes {
		fail(fmt.Errorf("response exceeds %d bytes", maxResponseBytes))
		return
	}
	stack[0] = writeJSON(cp, HTTPResp{Status: resp.StatusCode, Headers: resp.Header, Body: body})
}

// hostSecret answers only the manifest's secretFields of this call's connection.
func hostSecret(ctx context.Context, cp *extism.CurrentPlugin, stack []uint64) {
	st := state(ctx)
	name, err := cp.ReadString(stack[0])
	stack[0] = 0
	if err != nil || st == nil || !st.e.m.SecretField(name) {
		return
	}
	if v, ok := st.conn.Secrets[name]; ok {
		stack[0], _ = cp.WriteString(v)
	}
}

// hostConfig answers non-secret config only; a secret field name gets nothing
// even if it somehow ended up in Config.
func hostConfig(ctx context.Context, cp *extism.CurrentPlugin, stack []uint64) {
	st := state(ctx)
	name, err := cp.ReadString(stack[0])
	stack[0] = 0
	if err != nil || st == nil || st.e.m.SecretField(name) {
		return
	}
	if v, ok := st.conn.Config[name]; ok {
		stack[0], _ = cp.WriteString(v)
	}
}

func hostLog(ctx context.Context, cp *extism.CurrentPlugin, stack []uint64) {
	st := state(ctx)
	msg, err := cp.ReadString(stack[1])
	if err != nil || st == nil {
		return
	}
	st.log([]string{"debug", "info", "warn", "error"}[min(max(int(int32(stack[0])), 0), 3)], msg)
}
