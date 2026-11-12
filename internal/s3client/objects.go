package s3client

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"patchman-be/internal/models"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// BucketInfo is one row in the bucket list.
type BucketInfo struct {
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// ObjectEntry is one row in a prefix listing — either a "folder"
// (common prefix) or an object.
type ObjectEntry struct {
	Key          string `json:"key"`
	Name         string `json:"name"` // last path segment for display
	Size         int64  `json:"size"`
	LastModified string `json:"lastModified,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
	IsPrefix     bool   `json:"isPrefix"`
}

// ObjectListing is a paginated prefix listing (folder view).
type ObjectListing struct {
	Prefix      string        `json:"prefix"`
	Objects     []ObjectEntry `json:"objects"`
	NextToken   string        `json:"nextToken,omitempty"`
	IsTruncated bool          `json:"isTruncated"`
}

// ObjectMeta is the HeadObject response.
type ObjectMeta struct {
	Key          string            `json:"key"`
	Size         int64             `json:"size"`
	ContentType  string            `json:"contentType,omitempty"`
	ETag         string            `json:"etag,omitempty"`
	StorageClass string            `json:"storageClass,omitempty"`
	LastModified string            `json:"lastModified,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// Buckets lists all buckets on the connection.
func (r *Registry) Buckets(ctx context.Context, conn models.S3Connection) ([]BucketInfo, error) {
	cl, err := r.client(ctx, conn)
	if err != nil {
		return nil, err
	}
	out, err := cl.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, err
	}
	res := make([]BucketInfo, 0, len(out.Buckets))
	for _, b := range out.Buckets {
		bi := BucketInfo{Name: awssdk.ToString(b.Name)}
		if b.CreationDate != nil {
			bi.CreatedAt = b.CreationDate.UTC().Format(time.RFC3339)
		}
		res = append(res, bi)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Name < res[j].Name })
	return res, nil
}

// ListObjects returns one page of a prefix listing with '/' delimiter so
// the FE renders a folder tree. `token` continues a previous page.
func (r *Registry) ListObjects(ctx context.Context, conn models.S3Connection, bucket, prefix, token string, max int32) (*ObjectListing, error) {
	if bucket == "" {
		return nil, fmt.Errorf("bucket is required")
	}
	cl, err := r.client(ctx, conn)
	if err != nil {
		return nil, err
	}
	if max <= 0 || max > 1000 {
		max = 500
	}
	in := &s3.ListObjectsV2Input{
		Bucket:    awssdk.String(bucket),
		Prefix:    awssdk.String(prefix),
		Delimiter: awssdk.String("/"),
		MaxKeys:   awssdk.Int32(max),
	}
	if token != "" {
		in.ContinuationToken = awssdk.String(token)
	}
	out, err := cl.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, err
	}
	listing := &ObjectListing{Prefix: prefix, Objects: []ObjectEntry{}}
	// Folders first (common prefixes), then objects.
	for _, cp := range out.CommonPrefixes {
		key := awssdk.ToString(cp.Prefix)
		listing.Objects = append(listing.Objects, ObjectEntry{
			Key:      key,
			Name:     lastSegment(key, true),
			IsPrefix: true,
		})
	}
	for _, o := range out.Contents {
		key := awssdk.ToString(o.Key)
		if key == prefix {
			continue // the folder placeholder object itself
		}
		e := ObjectEntry{
			Key:          key,
			Name:         lastSegment(key, false),
			Size:         awssdk.ToInt64(o.Size),
			StorageClass: string(o.StorageClass),
		}
		if o.LastModified != nil {
			e.LastModified = o.LastModified.UTC().Format(time.RFC3339)
		}
		listing.Objects = append(listing.Objects, e)
	}
	listing.IsTruncated = awssdk.ToBool(out.IsTruncated)
	listing.NextToken = awssdk.ToString(out.NextContinuationToken)
	return listing, nil
}

// Head returns object metadata.
func (r *Registry) Head(ctx context.Context, conn models.S3Connection, bucket, key string) (*ObjectMeta, error) {
	cl, err := r.client(ctx, conn)
	if err != nil {
		return nil, err
	}
	out, err := cl.HeadObject(ctx, &s3.HeadObjectInput{Bucket: awssdk.String(bucket), Key: awssdk.String(key)})
	if err != nil {
		return nil, err
	}
	m := &ObjectMeta{
		Key:          key,
		Size:         awssdk.ToInt64(out.ContentLength),
		ContentType:  awssdk.ToString(out.ContentType),
		ETag:         awssdk.ToString(out.ETag),
		StorageClass: string(out.StorageClass),
		Metadata:     out.Metadata,
	}
	if out.LastModified != nil {
		m.LastModified = out.LastModified.UTC().Format(time.RFC3339)
	}
	return m, nil
}

// Get streams an object (optionally a byte Range for previews). The caller
// must Close the returned reader.
func (r *Registry) Get(ctx context.Context, conn models.S3Connection, bucket, key, rangeHeader string) (io.ReadCloser, string, int64, error) {
	cl, err := r.client(ctx, conn)
	if err != nil {
		return nil, "", 0, err
	}
	in := &s3.GetObjectInput{Bucket: awssdk.String(bucket), Key: awssdk.String(key)}
	if rangeHeader != "" {
		in.Range = awssdk.String(rangeHeader)
	}
	out, err := cl.GetObject(ctx, in)
	if err != nil {
		return nil, "", 0, err
	}
	return out.Body, awssdk.ToString(out.ContentType), awssdk.ToInt64(out.ContentLength), nil
}

// Put uploads an object (multipart via the manager for large bodies).
func (r *Registry) Put(ctx context.Context, conn models.S3Connection, bucket, key, contentType string, body io.Reader) error {
	cl, err := r.client(ctx, conn)
	if err != nil {
		return err
	}
	uploader := manager.NewUploader(cl)
	in := &s3.PutObjectInput{Bucket: awssdk.String(bucket), Key: awssdk.String(key), Body: body}
	if contentType != "" {
		in.ContentType = awssdk.String(contentType)
	}
	_, err = uploader.Upload(ctx, in)
	return err
}

// CreateFolder writes a zero-byte "prefix/" marker so an empty folder is
// visible in the browser.
func (r *Registry) CreateFolder(ctx context.Context, conn models.S3Connection, bucket, prefix string) error {
	if prefix == "" {
		return fmt.Errorf("prefix is required")
	}
	if prefix[len(prefix)-1] != '/' {
		prefix += "/"
	}
	cl, err := r.client(ctx, conn)
	if err != nil {
		return err
	}
	_, err = cl.PutObject(ctx, &s3.PutObjectInput{Bucket: awssdk.String(bucket), Key: awssdk.String(prefix)})
	return err
}

// Copy copies an object within/across buckets on the same connection.
func (r *Registry) Copy(ctx context.Context, conn models.S3Connection, srcBucket, srcKey, dstBucket, dstKey string) error {
	cl, err := r.client(ctx, conn)
	if err != nil {
		return err
	}
	_, err = cl.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     awssdk.String(dstBucket),
		Key:        awssdk.String(dstKey),
		CopySource: awssdk.String(srcBucket + "/" + srcKey),
	})
	return err
}

// Delete removes an object.
func (r *Registry) Delete(ctx context.Context, conn models.S3Connection, bucket, key string) error {
	cl, err := r.client(ctx, conn)
	if err != nil {
		return err
	}
	_, err = cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: awssdk.String(bucket), Key: awssdk.String(key)})
	return err
}

// Presign returns a time-limited URL for GET or PUT.
func (r *Registry) Presign(ctx context.Context, conn models.S3Connection, bucket, key, method string, ttl time.Duration) (string, error) {
	ps, err := r.presigner(ctx, conn)
	if err != nil {
		return "", err
	}
	if method == "PUT" {
		req, err := ps.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: awssdk.String(bucket), Key: awssdk.String(key)}, s3.WithPresignExpires(ttl))
		if err != nil {
			return "", err
		}
		return req.URL, nil
	}
	req, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: awssdk.String(bucket), Key: awssdk.String(key)}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// BaseName returns the final path segment of an object key (for download
// filenames). "a/b/c.txt" → "c.txt".
func BaseName(key string) string { return lastSegment(key, false) }

// lastSegment returns the display name for a key. For a prefix ("a/b/")
// it returns "b"; for an object ("a/b/c.txt") it returns "c.txt".
func lastSegment(key string, isPrefix bool) string {
	s := key
	if isPrefix && len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return s[i+1:]
		}
	}
	return s
}
