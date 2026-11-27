package s3client

import (
	"context"
	"testing"

	"patchman-be/internal/models"
)

// A connection limited to a bucket answers its bucket without calling
// ListBuckets (path-limited keys usually may not).
func TestBucketsPinned(t *testing.T) {
	r := &Registry{}
	got, err := r.Buckets(context.Background(), models.S3Connection{OptionsJSON: `{"bucket":"data","prefix":"team/a"}`})
	if err != nil || len(got) != 1 || got[0].Name != "data" || got[0].Prefix != "team/a/" {
		t.Fatalf("got %+v %v", got, err)
	}
}
