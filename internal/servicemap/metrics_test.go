package servicemap

import "testing"

// The agent's status label: HTTP "1xx".."5xx", gRPC "grpc:<code>", other
// protocols "ok" / "failed" / "unknown" (a response it could not parse).
func TestIsErrorStatus(t *testing.T) {
	for status, want := range map[string]bool{
		"5xx": true, "500": true, "failed": true, "grpc:UNAVAILABLE": true,
		"2xx": false, "4xx": false, "ok": false, "unknown": false, "grpc:OK": false, "": false,
	} {
		if got := isErrorStatus(status); got != want {
			t.Errorf("isErrorStatus(%q) = %v, want %v", status, got, want)
		}
	}
}
