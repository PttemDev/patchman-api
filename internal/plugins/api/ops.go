package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/jsonschema-go/jsonschema"

	"patchman-be/internal/middleware"
	"patchman-be/internal/models"
	"patchman-be/internal/plugins/runtime"
)

const maxOpInput = 8 << 20

// rawOrText returns plugin output as JSON when it is JSON, else as text.
func rawOrText(out []byte) any {
	if json.Valid(out) && len(out) > 0 {
		return json.RawMessage(out)
	}
	return gin.H{"text": string(out)}
}

// callOp runs one op of a plugin on one connection. Gates, in order:
// connection belongs to the plugin and the user can reach it; plugin enabled
// and op declared; write ops need a write role, the connection's read-write
// switch and, from an agent, an explicit allowWrite; sensitive ops never run
// for an agent (step-up comes from LocalGate's matcher); input matches the
// op's schema. Every call is audited.
func (s *Service) callOp(c *gin.Context) {
	pluginID, connID, opName := c.Param("pluginId"), c.Param("connId"), c.Param("op")
	userID := c.GetString("userID")
	cn, role, err := s.loadAccessible(pluginID, connID, userID)
	if err != nil {
		errf(c, http.StatusNotFound, "Connection not found")
		return
	}
	p, ok := s.lookup(pluginID, true)
	if !ok {
		errf(c, http.StatusNotFound, "Plugin not found or disabled")
		return
	}
	op, ok := p.m.Ops[opName]
	if !ok {
		errf(c, http.StatusNotFound, "Unknown op %q", opName)
		return
	}
	isAgent := middleware.IsInProcessRequest(c.Request.Context()) || c.GetHeader(mcpClientHeader) == "mcp"
	// refuse answers 403 and keeps the refused attempt in the audit trail.
	refuse := func(msg string) {
		act := models.PluginActivity{PluginID: pluginID, PluginVersion: p.row.Version, ConnectionID: cn.ID,
			Action: "op", Op: opName, ErrorText: "refused: " + msg}
		if role != "owner" {
			owner := cn.UserID
			act.DelegatedOwner = &owner
		}
		s.audit(c, act)
		errf(c, http.StatusForbidden, "%s", msg)
	}
	if op.Write {
		if role != "owner" && role != "write" {
			refuse("This op needs write access on the connection.")
			return
		}
		if !cn.AllowWrite {
			refuse("This connection is read-only. Enable read-write on it to run write ops.")
			return
		}
		if isAgent && c.Query("allowWrite") != "true" {
			refuse("Write op: pass allowWrite=true to confirm.")
			return
		}
	}
	if op.Sensitive && isAgent {
		refuse("Sensitive plugin ops need a person to confirm; agents cannot run them.")
		return
	}
	input, err := io.ReadAll(io.LimitReader(c.Request.Body, maxOpInput+1))
	if err != nil || len(input) > maxOpInput {
		errf(c, http.StatusRequestEntityTooLarge, "input too large")
		return
	}
	if len(input) == 0 {
		input = []byte("{}")
	}
	if len(op.Input) > 0 {
		if err := validateJSON(op.Input, input); err != nil {
			errf(c, http.StatusBadRequest, "input: %v", err)
			return
		}
	}
	rc, err := s.runtimeConn(*cn)
	if err != nil {
		errf(c, http.StatusInternalServerError, "%v", err)
		return
	}
	out, info, callErr := s.rt.Call(c.Request.Context(), pluginID, rc, opName, input)

	act := models.PluginActivity{PluginID: pluginID, PluginVersion: p.row.Version, ConnectionID: cn.ID,
		Action: "op", Op: opName, DurationMs: info.Duration.Milliseconds()}
	if role != "owner" {
		owner := cn.UserID
		act.DelegatedOwner = &owner
	}
	if len(info.Logs) > 0 {
		logs := info.Logs
		if len(logs) > 20 {
			logs = logs[:20]
		}
		act.DetailJSON = detail(gin.H{"logs": logs})
	}
	// Output schema: a declared output must be honoured (agents and views
	// rely on its shape).
	if callErr == nil && len(op.Output) > 0 {
		if err := validateJSON(op.Output, out); err != nil {
			callErr = fmt.Errorf("plugin %s/%s: output does not match its schema: %v", pluginID, opName, err)
		}
	}
	if callErr != nil {
		act.ErrorText = callErr.Error()
	}
	s.audit(c, act)

	if callErr != nil {
		code := http.StatusBadGateway
		switch {
		case errors.Is(callErr, runtime.ErrNotFound):
			code = http.StatusNotFound
		case errors.Is(callErr, context.DeadlineExceeded):
			code = http.StatusGatewayTimeout
		}
		c.JSON(code, gin.H{"error": callErr.Error(), "logs": info.Logs,
			"source": gin.H{"plugin": pluginID, "version": p.row.Version, "untrusted": true}})
		return
	}
	// Plugin output is third-party data: the envelope says so for agents.
	c.JSON(http.StatusOK, gin.H{"data": rawOrText(out),
		"source": gin.H{"plugin": pluginID, "version": p.row.Version, "untrusted": true}})
}

// validateJSON checks raw JSON against a JSON Schema.
func validateJSON(schema json.RawMessage, raw []byte) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("not JSON: %v", err)
	}
	var sch jsonschema.Schema
	if err := json.Unmarshal(schema, &sch); err != nil {
		return err
	}
	rs, err := sch.Resolve(nil)
	if err != nil {
		return err
	}
	return rs.Validate(v)
}
