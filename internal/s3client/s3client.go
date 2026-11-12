// Package s3client is the BE-side adapter for the user-facing "Storage"
// (S3-compatible object storage) tool. It mirrors internal/dbquery +
// internal/kafkaquery: a Registry pools one *s3.Client per connection,
// decrypting the secret key just-in-time. Works against AWS S3 and any
// S3-compatible on-prem/cloud provider (MinIO, Ceph RGW, Wasabi, R2, B2,
// DigitalOcean Spaces, …) via an endpoint override + path-style toggle.
package s3client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Registry pools one *s3.Client per connection ID (clients are cheap +
// stateless, so we mostly cache to avoid re-decrypting on every call).
type Registry struct {
	cipher *services.TokenCipher

	mu      sync.Mutex
	clients map[string]*pooledClient
}

type pooledClient struct {
	client   *s3.Client
	lastUsed time.Time
}

const clientIdleTimeout = 10 * time.Minute

func NewRegistry(cipher *services.TokenCipher) *Registry {
	r := &Registry{cipher: cipher, clients: map[string]*pooledClient{}}
	go r.reapLoop()
	return r
}

// config holds the decrypted, resolved connection settings.
type resolvedConfig struct {
	Endpoint           string
	Region             string
	AccessKeyID        string
	SecretKey          string
	SessionToken       string
	ForcePathStyle     bool
	InsecureSkipVerify bool
}

func (r *Registry) resolve(conn models.S3Connection) (resolvedConfig, error) {
	secret := ""
	if conn.SecretKeyEnc != "" {
		s, err := r.cipher.Decrypt(conn.SecretKeyEnc)
		if err != nil {
			return resolvedConfig{}, fmt.Errorf("decrypt secret key: %w", err)
		}
		secret = s
	}
	token := ""
	if conn.SessionTokenEnc != "" {
		t, err := r.cipher.Decrypt(conn.SessionTokenEnc)
		if err != nil {
			return resolvedConfig{}, fmt.Errorf("decrypt session token: %w", err)
		}
		token = t
	}
	region := conn.Region
	if region == "" {
		region = "us-east-1"
	}
	return resolvedConfig{
		Endpoint:           conn.Endpoint,
		Region:             region,
		AccessKeyID:        conn.AccessKeyID,
		SecretKey:          secret,
		SessionToken:       token,
		ForcePathStyle:     conn.ForcePathStyle,
		InsecureSkipVerify: conn.InsecureSkipVerify,
	}, nil
}

func buildClient(ctx context.Context, rc resolvedConfig) (*s3.Client, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(rc.Region),
	}
	if rc.AccessKeyID != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(rc.AccessKeyID, rc.SecretKey, rc.SessionToken),
		))
	}
	if rc.InsecureSkipVerify {
		httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(tr *http.Transport) {
			if tr.TLSClientConfig == nil {
				tr.TLSClientConfig = &tls.Config{}
			}
			tr.TLSClientConfig.InsecureSkipVerify = true
		})
		opts = append(opts, config.WithHTTPClient(httpClient))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if rc.Endpoint != "" {
			o.BaseEndpoint = awssdk.String(rc.Endpoint)
		}
		o.UsePathStyle = rc.ForcePathStyle
	})
	return client, nil
}

// client returns the pooled client for a connection.
func (r *Registry) client(ctx context.Context, conn models.S3Connection) (*s3.Client, error) {
	r.mu.Lock()
	if pc, ok := r.clients[conn.ID]; ok {
		pc.lastUsed = time.Now()
		r.mu.Unlock()
		return pc.client, nil
	}
	r.mu.Unlock()

	rc, err := r.resolve(conn)
	if err != nil {
		return nil, err
	}
	cl, err := buildClient(ctx, rc)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.clients[conn.ID] = &pooledClient{client: cl, lastUsed: time.Now()}
	r.mu.Unlock()
	return cl, nil
}

// Presigner returns a presign client for time-limited URLs.
func (r *Registry) presigner(ctx context.Context, conn models.S3Connection) (*s3.PresignClient, error) {
	cl, err := r.client(ctx, conn)
	if err != nil {
		return nil, err
	}
	return s3.NewPresignClient(cl), nil
}

// Test lists buckets to confirm credentials + endpoint reachability.
func (r *Registry) Test(ctx context.Context, conn models.S3Connection) error {
	rc, err := r.resolve(conn)
	if err != nil {
		return err
	}
	cl, err := buildClient(ctx, rc)
	if err != nil {
		return err
	}
	_, err = cl.ListBuckets(ctx, &s3.ListBucketsInput{})
	return err
}

func (r *Registry) Close(connectionID string) {
	r.mu.Lock()
	delete(r.clients, connectionID)
	r.mu.Unlock()
}

func (r *Registry) reapLoop() {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-clientIdleTimeout)
		r.mu.Lock()
		for id, pc := range r.clients {
			if pc.lastUsed.Before(cutoff) {
				delete(r.clients, id)
			}
		}
		r.mu.Unlock()
	}
}

func parseOptions(raw string) map[string]any {
	if raw == "" {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

var _ = parseOptions // reserved for provider-specific knobs (SSE/KMS)
