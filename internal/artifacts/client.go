package artifacts

import (
	"context"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/gm2211/grove/internal/config"
)

type client struct {
	raw    *minio.Client
	bucket string
}

// New builds a Client from cfg. Returns ErrNotConfigured (not an error you should treat as fatal —
// callers should degrade gracefully, e.g. dispatch just skips populating Job.Artifacts, and the
// server's artifact-download handler returns 404) when cfg.Endpoint or cfg.Bucket is empty.
func New(cfg config.ArtifactsConfig) (Client, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, ErrNotConfigured
	}
	host, secure := splitEndpoint(cfg.Endpoint)
	raw, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: secure,
	})
	if err != nil {
		return nil, fmt.Errorf("artifacts: %w", err)
	}
	return &client{raw: raw, bucket: cfg.Bucket}, nil
}

// splitEndpoint strips an http(s):// scheme (minio.New wants a bare host[:port]) and reports
// whether TLS should be used — explicit "http://" means no, everything else (including a bare
// host with no scheme) defaults to secure, matching MinIO's own client conventions.
func splitEndpoint(raw string) (host string, secure bool) {
	switch {
	case strings.HasPrefix(raw, "http://"):
		return strings.TrimPrefix(raw, "http://"), false
	case strings.HasPrefix(raw, "https://"):
		return strings.TrimPrefix(raw, "https://"), true
	default:
		return raw, true
	}
}

func (c *client) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	for obj := range c.raw.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("artifacts: list %s: %w", prefix, obj.Err)
		}
		out = append(out, Object{
			Path:        strings.TrimPrefix(obj.Key, prefix),
			Size:        obj.Size,
			ContentType: contentTypeOf(obj),
			ModTime:     obj.LastModified,
		})
	}
	return out, nil
}

func contentTypeOf(obj minio.ObjectInfo) string {
	if obj.ContentType != "" {
		return obj.ContentType
	}
	if ct := mime.TypeByExtension(filepath.Ext(obj.Key)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func (c *client) Get(ctx context.Context, key string) (io.ReadCloser, Object, error) {
	obj, err := c.raw.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, Object{}, fmt.Errorf("artifacts: get %s: %w", key, err)
	}
	info, err := obj.Stat()
	if err != nil {
		obj.Close()
		if resp := minio.ToErrorResponse(err); resp.Code == "NoSuchKey" {
			return nil, Object{}, ErrNotFound
		}
		return nil, Object{}, fmt.Errorf("artifacts: stat %s: %w", key, err)
	}
	return obj, Object{Path: key, Size: info.Size, ContentType: contentTypeOf(info), ModTime: info.LastModified}, nil
}
