package services

import (
	"io"
	"mime"
	"mime/multipart"
	"testing"
)

// TestBuildMultipartFormTextOnly is a regression test for a 415 response:
// a curl -F form with no file fields must still go out as
// multipart/form-data (boundary + parts), not url-encoded.
func TestBuildMultipartFormTextOnly(t *testing.T) {
	fields := []FormField{
		{Key: "orderId", Value: "ORD-ABC123XYZ-250426", Type: "text", Enabled: true},
		{Key: "rate", Value: "5", Type: "text", Enabled: true},
		{Key: "skipMe", Value: "x", Type: "text", Enabled: false},
	}
	buf, contentType := buildMultipartForm(fields)

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("content type does not parse: %v", err)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("media type = %q, want multipart/form-data", mediaType)
	}
	if params["boundary"] == "" {
		t.Fatalf("no boundary in content type %q", contentType)
	}

	// Re-parse the body with the announced boundary and assert the enabled
	// text fields are present and the disabled one is dropped.
	mr := multipart.NewReader(buf, params["boundary"])
	got := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		data, _ := io.ReadAll(part)
		got[part.FormName()] = string(data)
	}
	if got["orderId"] != "ORD-ABC123XYZ-250426" {
		t.Fatalf("orderId part = %q", got["orderId"])
	}
	if got["rate"] != "5" {
		t.Fatalf("rate part = %q", got["rate"])
	}
	if _, ok := got["skipMe"]; ok {
		t.Fatalf("disabled field skipMe should not be sent")
	}
}
