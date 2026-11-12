package services

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// DefaultAcceptEncoding advertises every encoding we know how to decode.
const DefaultAcceptEncoding = "gzip, deflate, br, zstd"

// DecodeResponseBody reads resp.Body and transparently decodes any
// Content-Encoding we understand (gzip, deflate, br, zstd). If the encoding
// is unknown the raw bytes are returned. On success the Content-Encoding and
// Content-Length headers on resp are cleared so callers that forward the
// response don't end up double-declaring the encoding.
//
// limit caps the number of decoded bytes read (<=0 means unlimited).
func DecodeResponseBody(resp *http.Response, limit int64) ([]byte, error) {
	data, _, err := DecodeResponseBodyEncoding(resp, limit)
	return data, err
}

// DecodeResponseBodyEncoding is like DecodeResponseBody but also returns the
// lowercased Content-Encoding that was decoded (empty for identity/unknown).
// Useful for surfacing "served as gzip/br/zstd" metadata to the UI.
func DecodeResponseBodyEncoding(resp *http.Response, limit int64) ([]byte, string, error) {
	if resp == nil || resp.Body == nil {
		return nil, "", nil
	}

	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))

	reader, err := wrapDecoder(resp.Body, encoding)
	if err != nil {
		// Fall back to the raw body so the caller can still see something
		// useful instead of erroring out on a malformed encoding header.
		raw, readErr := readAll(resp.Body, limit)
		if readErr != nil {
			return nil, "", readErr
		}
		return raw, "", nil
	}

	if closer, ok := reader.(io.Closer); ok && reader != resp.Body {
		defer closer.Close()
	}

	data, err := readAll(reader, limit)
	if err != nil {
		return nil, "", err
	}

	if encoding != "" && encoding != "identity" {
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		return data, encoding, nil
	}

	return data, "", nil
}

func wrapDecoder(body io.Reader, encoding string) (io.Reader, error) {
	switch encoding {
	case "", "identity":
		return body, nil
	case "gzip", "x-gzip":
		return gzip.NewReader(body)
	case "deflate":
		// Servers often send raw deflate even though the spec says zlib.
		// Peek at the data and pick the right decoder.
		buf, err := io.ReadAll(body)
		if err != nil {
			return nil, err
		}
		if zr, zerr := zlib.NewReader(bytes.NewReader(buf)); zerr == nil {
			return zr, nil
		}
		return flate.NewReader(bytes.NewReader(buf)), nil
	case "br":
		return brotli.NewReader(body), nil
	case "zstd":
		zr, err := zstd.NewReader(body)
		if err != nil {
			return nil, err
		}
		return zstdReader{zr}, nil
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding: %s", encoding)
	}
}

// zstdReader adapts zstd.Decoder.Close (no return value) to io.Closer.
type zstdReader struct {
	*zstd.Decoder
}

func (z zstdReader) Close() error {
	z.Decoder.Close()
	return nil
}

func readAll(r io.Reader, limit int64) ([]byte, error) {
	if limit > 0 {
		return io.ReadAll(io.LimitReader(r, limit))
	}
	return io.ReadAll(r)
}
