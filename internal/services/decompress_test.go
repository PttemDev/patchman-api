package services

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const payload = "the quick brown fox jumps over the lazy dog, " +
	"the quick brown fox jumps over the lazy dog"

func encodeGzip(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeZlib(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeRawDeflate(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeBrotli(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeZstd(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mockResponse(encoding string, body []byte) *http.Response {
	h := http.Header{}
	if encoding != "" {
		h.Set("Content-Encoding", encoding)
	}
	return &http.Response{
		Header: h,
		Body:   io.NopCloser(bytes.NewReader(body)),
	}
}

func TestDecodeResponseBody(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
		body     []byte
	}{
		{"identity", "", []byte(payload)},
		{"gzip", "gzip", encodeGzip(t, payload)},
		{"deflate-zlib", "deflate", encodeZlib(t, payload)},
		{"deflate-raw", "deflate", encodeRawDeflate(t, payload)},
		{"brotli", "br", encodeBrotli(t, payload)},
		{"zstd", "zstd", encodeZstd(t, payload)},
		{"case-insensitive", "GZIP", encodeGzip(t, payload)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := mockResponse(tc.encoding, tc.body)
			got, err := DecodeResponseBody(resp, 0)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != payload {
				t.Errorf("got %q, want %q", got, payload)
			}
			// Encoding header should be cleared on successful decode so
			// forwarders don't double-declare it.
			if tc.encoding != "" && resp.Header.Get("Content-Encoding") != "" {
				t.Errorf("Content-Encoding not cleared: %q", resp.Header.Get("Content-Encoding"))
			}
		})
	}
}

func TestDecodeResponseBodyUnknownEncoding(t *testing.T) {
	// Unknown encoding should fall through and return raw bytes, not error.
	raw := []byte("totally not encoded")
	resp := mockResponse("snappy", raw)
	got, err := DecodeResponseBody(resp, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("got %q, want %q", got, raw)
	}
}

func TestDecodeResponseBodyLimit(t *testing.T) {
	big := strings.Repeat("x", 2048)
	resp := mockResponse("gzip", encodeGzip(t, big))
	got, err := DecodeResponseBody(resp, 512)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 512 {
		t.Errorf("limit not honored: got %d bytes, want 512", len(got))
	}
}

func TestDecodeResponseBodyNil(t *testing.T) {
	if _, err := DecodeResponseBody(nil, 0); err != nil {
		t.Fatalf("nil response should not error: %v", err)
	}
}
