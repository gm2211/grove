package artifacts

import (
	"testing"

	"github.com/minio/minio-go/v7"

	"github.com/gm2211/grove/internal/config"
)

func TestSplitEndpoint(t *testing.T) {
	cases := []struct {
		in         string
		wantHost   string
		wantSecure bool
	}{
		{"http://minio.internal:9000", "minio.internal:9000", false},
		{"https://minio.internal:9000", "minio.internal:9000", true},
		{"minio.internal:9000", "minio.internal:9000", true},
		{"s3.example.com", "s3.example.com", true},
	}
	for _, c := range cases {
		host, secure := splitEndpoint(c.in)
		if host != c.wantHost || secure != c.wantSecure {
			t.Errorf("splitEndpoint(%q) = (%q, %v), want (%q, %v)", c.in, host, secure, c.wantHost, c.wantSecure)
		}
	}
}

func TestContentTypeOf(t *testing.T) {
	cases := []struct {
		name string
		obj  minio.ObjectInfo
		want string
	}{
		{"explicit content type wins", minio.ObjectInfo{Key: "out.bin", ContentType: "application/x-custom"}, "application/x-custom"},
		{"falls back to extension", minio.ObjectInfo{Key: "report.json"}, "application/json"},
		{"unknown extension falls back to octet-stream", minio.ObjectInfo{Key: "out.unknownext"}, "application/octet-stream"},
		{"no extension falls back to octet-stream", minio.ObjectInfo{Key: "out"}, "application/octet-stream"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := contentTypeOf(c.obj); got != c.want {
				t.Errorf("contentTypeOf(%+v) = %q, want %q", c.obj, got, c.want)
			}
		})
	}
}

func TestNew_NotConfigured(t *testing.T) {
	cases := []config.ArtifactsConfig{
		{},
		{Endpoint: "http://minio:9000"},
		{Bucket: "grove-artifacts"},
	}
	for _, cfg := range cases {
		if _, err := New(cfg); err != ErrNotConfigured {
			t.Errorf("New(%+v) err = %v, want ErrNotConfigured", cfg, err)
		}
	}
}

func TestNew_Configured(t *testing.T) {
	c, err := New(config.ArtifactsConfig{Endpoint: "http://minio:9000", Bucket: "grove-artifacts", AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c == nil {
		t.Fatal("New returned nil Client with nil error")
	}
}
