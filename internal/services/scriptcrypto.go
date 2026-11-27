package services

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"

	"github.com/dop251/goja"
)

// Postman scripts sign requests with CryptoJS (SHA256 / HMAC / Base64) and
// btoa/atob. These are the pieces of that API PatchMan provides: hashing and
// encoding run in Go; a small JS shim gives them CryptoJS's shape
// (WordArray, CryptoJS.enc.*, toString(encoder)).

func hashFor(alg string) (func() hash.Hash, error) {
	switch strings.ToUpper(alg) {
	case "MD5":
		return md5.New, nil
	case "SHA1":
		return sha1.New, nil
	case "SHA224":
		return sha256.New224, nil
	case "SHA256":
		return sha256.New, nil
	case "SHA384":
		return sha512.New384, nil
	case "SHA512":
		return sha512.New, nil
	}
	return nil, fmt.Errorf("unsupported hash %q", alg)
}

// decodeAs turns text in an encoding into bytes.
func decodeAs(s, enc string) ([]byte, error) {
	switch strings.ToLower(enc) {
	case "utf8":
		return []byte(s), nil
	case "latin1":
		b := make([]byte, 0, len(s))
		for _, r := range s {
			b = append(b, byte(r))
		}
		return b, nil
	case "hex":
		return hex.DecodeString(s)
	case "base64":
		return base64.StdEncoding.DecodeString(s)
	case "base64url":
		return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	}
	return nil, fmt.Errorf("unsupported encoding %q", enc)
}

// encodeAs turns bytes into text in an encoding.
func encodeAs(b []byte, enc string) (string, error) {
	switch strings.ToLower(enc) {
	case "utf8":
		return string(b), nil
	case "latin1":
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		return string(r), nil
	case "hex":
		return hex.EncodeToString(b), nil
	case "base64":
		return base64.StdEncoding.EncodeToString(b), nil
	case "base64url":
		return base64.RawURLEncoding.EncodeToString(b), nil
	}
	return "", fmt.Errorf("unsupported encoding %q", enc)
}

const cryptoShim = `
var CryptoJS = (function () {
  function WA(hex) { this.__hex = hex; this.sigBytes = hex.length / 2; }
  WA.prototype.toString = function (enc) { return (enc || C.enc.Hex).stringify(this); };
  WA.prototype.concat = function (o) { this.__hex += toWA(o).__hex; this.sigBytes = this.__hex.length / 2; return this; };
  WA.prototype.clone = function () { return new WA(this.__hex); };
  function toWA(v) { return v instanceof WA ? v : new WA(__pmDecode(String(v), 'utf8')); }
  function enc(name) {
    return {
      stringify: function (wa) { return __pmEncode(toWA(wa).__hex, name); },
      parse: function (s) { return new WA(__pmDecode(String(s), name)); }
    };
  }
  var C = {
    enc: { Hex: enc('hex'), Base64: enc('base64'), Base64url: enc('base64url'), Utf8: enc('utf8'), Latin1: enc('latin1') },
    lib: { WordArray: { create: function () { return new WA(''); } } }
  };
  ['MD5', 'SHA1', 'SHA224', 'SHA256', 'SHA384', 'SHA512'].forEach(function (a) {
    C[a] = function (m) { return new WA(__pmHash(a, toWA(m).__hex)); };
    C['Hmac' + a] = function (m, k) { return new WA(__pmHmac(a, toWA(m).__hex, toWA(k).__hex)); };
  });
  return C;
})();
`

// installCrypto adds CryptoJS, btoa, atob and require('crypto-js').
func installCrypto(vm *goja.Runtime) error {
	throw := func(err error) { panic(vm.NewGoError(err)) }
	_ = vm.Set("__pmDecode", func(s, enc string) string {
		b, err := decodeAs(s, enc)
		if err != nil {
			throw(err)
		}
		return hex.EncodeToString(b)
	})
	_ = vm.Set("__pmEncode", func(h, enc string) string {
		b, err := hex.DecodeString(h)
		if err != nil {
			throw(err)
		}
		s, err := encodeAs(b, enc)
		if err != nil {
			throw(err)
		}
		return s
	})
	_ = vm.Set("__pmHash", func(alg, h string) string {
		newHash, err := hashFor(alg)
		if err != nil {
			throw(err)
		}
		b, _ := hex.DecodeString(h)
		hh := newHash()
		hh.Write(b)
		return hex.EncodeToString(hh.Sum(nil))
	})
	_ = vm.Set("__pmHmac", func(alg, msgHex, keyHex string) string {
		newHash, err := hashFor(alg)
		if err != nil {
			throw(err)
		}
		msg, _ := hex.DecodeString(msgHex)
		key, _ := hex.DecodeString(keyHex)
		m := hmac.New(newHash, key)
		m.Write(msg)
		return hex.EncodeToString(m.Sum(nil))
	})
	// btoa / atob work on "binary strings" (one char per byte), like browsers.
	_ = vm.Set("btoa", func(s string) string {
		b, err := decodeAs(s, "latin1")
		if err != nil {
			throw(err)
		}
		return base64.StdEncoding.EncodeToString(b)
	})
	_ = vm.Set("atob", func(s string) string {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
		if err != nil {
			throw(fmt.Errorf("atob: %w", err))
		}
		out, _ := encodeAs(b, "latin1")
		return out
	})
	if _, err := vm.RunString(cryptoShim); err != nil {
		return err
	}
	_ = vm.Set("require", func(name string) goja.Value {
		if name == "crypto-js" {
			return vm.Get("CryptoJS")
		}
		throw(fmt.Errorf("require(%q): only 'crypto-js' is available in PatchMan scripts", name))
		return goja.Undefined()
	})
	return nil
}
