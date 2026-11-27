package services

import (
	"reflect"
	"testing"
)

// The BFF signing script users paste from Postman: CryptoJS Base64/Utf8/
// SHA256 plus pm.environment.set. Expected values come from Python hashlib.
func TestScriptCryptoJSSigning(t *testing.T) {
	ctx := &ScriptContext{EnvVars: map[string]string{"bff_service_name": "svc", "bff_service_secret": "s3cr3t"}}
	script := `
const name = pm.environment.get('bff_service_name');
const secret = pm.environment.get('bff_service_secret');
const ts = '1700000000';
const b64 = CryptoJS.enc.Base64.stringify(CryptoJS.enc.Utf8.parse(name + ts + secret));
pm.environment.set('bff_service_hash', CryptoJS.SHA256(b64).toString(CryptoJS.enc.Hex));
pm.environment.set('hmac', CryptoJS.HmacSHA256('msg', 'key').toString());
pm.environment.set('b64digest', CryptoJS.SHA256('abc').toString(CryptoJS.enc.Base64));
pm.environment.set('md5', require('crypto-js').MD5('abc') + '');
pm.environment.set('roundtrip', atob(btoa('hello')));
pm.environment.set('url', pm.environment.replaceIn('{{bff_service_name}}/x/{{missing}}'));
`
	if err := NewScriptEngine().Execute(script, ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"bff_service_hash": "1a5313a417954bdcf0246c07034e9dbc46a64d75b91ce9886654b842ce42acab",
		"hmac":             "2d93cbc1be167bcb1637a4a23cbff01a7878f0c50ee833954ea5221bb1b8c628",
		"b64digest":        "ungWv48Bz+pBQUDeXa4iI7ADYaOWF3qctBD/YfIAFa0=",
		"md5":              "900150983cd24fb0d6963f7d28e17f72",
		"roundtrip":        "hello",
		"url":              "svc/x/{{missing}}",
	}
	for k, v := range want {
		if got := ctx.EnvVars[k]; got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// Postman's header list API: add/upsert with {key, value}, case-insensitive
// replace, remove — and only the changes are reported back.
func TestScriptHeaderChanges(t *testing.T) {
	ctx := &ScriptContext{Headers: map[string]string{"Accept": "application/json", "X-Old": "1", "x-hash": "stale"}}
	script := `
pm.request.headers.upsert({ key: 'X-Hash', value: 'fresh' });
pm.request.headers.add({ key: 'X-Ts', value: 123 });
pm.request.headers.add('X-Line: v');
pm.request.headers.remove('x-old');
if (pm.request.headers.get('accept') !== 'application/json') throw new Error('get is case-insensitive');
`
	if err := NewScriptEngine().Execute(script, ctx); err != nil {
		t.Fatal(err)
	}
	wantSet := map[string]string{"X-Hash": "fresh", "X-Ts": "123", "X-Line": "v"}
	if !reflect.DeepEqual(ctx.SetHeaders, wantSet) {
		t.Fatalf("SetHeaders = %v, want %v", ctx.SetHeaders, wantSet)
	}
	if !reflect.DeepEqual(ctx.RemovedHeaders, []string{"x-old"}) {
		t.Fatalf("RemovedHeaders = %v", ctx.RemovedHeaders)
	}
	if _, ok := ctx.Headers["x-hash"]; ok || ctx.Headers["X-Hash"] != "fresh" {
		t.Fatalf("upsert must replace case-insensitively: %v", ctx.Headers)
	}
}

func TestScriptRequireUnknown(t *testing.T) {
	err := NewScriptEngine().Execute(`require('moment')`, &ScriptContext{})
	if err == nil {
		t.Fatal("unknown module should fail with a clear error")
	}
}
