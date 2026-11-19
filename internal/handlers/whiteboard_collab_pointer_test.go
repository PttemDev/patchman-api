package handlers

import "testing"

// A viewer may only send cursor frames; anything else from them is dropped.
func TestIsPointerFrame(t *testing.T) {
	cases := map[string]bool{
		`{"type":"pointer","x":10,"y":-3.5}`:           true,
		`{"type":"scene","elements":[]}`:               false,
		`{"type":"pointer"}`:                           false,
		`{"type":"pointer","x":1,"y":2,"elements":[]}`: true, // extra fields are dropped by the receiver
		`not json`: false,
	}
	for in, want := range cases {
		if got := isPointerFrame([]byte(in)); got != want {
			t.Errorf("%s = %v, want %v", in, got, want)
		}
	}
	big := make([]byte, 600)
	for i := range big {
		big[i] = ' '
	}
	copy(big, `{"type":"pointer","x":1,"y":2}`)
	if isPointerFrame(big) {
		t.Error("an oversized frame is not a cursor update")
	}
}
