package models

import "testing"

func TestS3Pin(t *testing.T) {
	cases := []struct {
		opts string
		want S3Pin
	}{
		{``, S3Pin{}},
		{`{}`, S3Pin{}},
		{`{"bucket":"data","prefix":"/team/x"}`, S3Pin{Bucket: "data", Prefix: "team/x/"}},
		{`{"bucket":" data ","prefix":"team/"}`, S3Pin{Bucket: "data", Prefix: "team/"}},
		{`{"prefix":"team/"}`, S3Pin{}}, // a path without a bucket means nothing
	}
	for _, c := range cases {
		if got := (S3Connection{OptionsJSON: c.opts}).Pin(); got != c.want {
			t.Errorf("%q: got %+v want %+v", c.opts, got, c.want)
		}
	}
}
