package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The conditional and ranged operations behave the same on every backend
// that has them; run the suite against each one available.
func testConditionalAndRanged(t *testing.T, s Storage, prefix string) {
	t.Helper()
	ctx := context.Background()
	cu, rd := s, s
	key := prefix + "/conditional"
	t.Cleanup(func() { _ = s.DeleteObjects([]string{key, prefix + "/file", prefix + "/ranged"}) })

	if _, n, err := cu.UploadDataIfAbsent(ctx, []byte("first"), key, "text/plain"); err != nil || n != 5 {
		t.Fatalf("first conditional upload: %v %d", err, n)
	}
	if _, _, err := cu.UploadDataIfAbsent(ctx, []byte("second"), key, "text/plain"); !errors.Is(err, ErrObjectExists) {
		t.Fatalf("second conditional upload: %v, want ErrObjectExists", err)
	}
	if got, err := s.DownloadData(key); err != nil || string(got) != "first" {
		t.Fatalf("the refused upload changed the object: %q %v", got, err)
	}

	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte(strings.Repeat("x", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, n, err := cu.UploadFileIfAbsent(ctx, f, prefix+"/file", "application/octet-stream"); err != nil || n != 100 {
		t.Fatalf("conditional file upload: %v %d", err, n)
	}
	if _, _, err := cu.UploadFileIfAbsent(ctx, f, prefix+"/file", "application/octet-stream"); !errors.Is(err, ErrObjectExists) {
		t.Fatalf("conditional file upload over an object: %v", err)
	}

	if _, _, err := s.UploadData([]byte("0123456789"), prefix+"/ranged", "text/plain"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		off, n int64
		want   string
	}{
		{2, 3, "234"}, {7, 100, "789"}, {0, 10, "0123456789"},
	} {
		got, err := rd.DownloadRange(ctx, prefix+"/ranged", tc.off, tc.n)
		if err != nil || string(got) != tc.want {
			t.Fatalf("range %d+%d: %q %v", tc.off, tc.n, got, err)
		}
	}
	if _, err := rd.DownloadRange(ctx, prefix+"/ranged", 10, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("range past the end: %v, want ErrNotFound", err)
	}
	if _, err := rd.DownloadRange(ctx, prefix+"/absent", 0, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("range of a missing object: %v, want ErrNotFound", err)
	}
}

func TestLocalConditionalAndRanged(t *testing.T) {
	s, err := NewLocal(&LocalConfig{StorageDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	testConditionalAndRanged(t, s, "p")
}

// TestS3ConditionalAndRanged runs against any S3-compatible endpoint, for
// example MinIO: S3_ENDPOINT=http://127.0.0.1:9000 S3_BUCKET=b
// S3_ACCESS_KEY=... S3_SECRET=... go test -run S3.
func TestS3ConditionalAndRanged(t *testing.T) {
	endpoint, bucket := os.Getenv("S3_ENDPOINT"), os.Getenv("S3_BUCKET")
	key, secret := os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET")
	if bucket == "" || key == "" || secret == "" {
		t.Skip("Missing env vars")
	}
	s, err := NewS3(&S3Config{AccessKey: key, Secret: secret, Endpoint: endpoint, Bucket: bucket, Region: "us-east-1", ForcePathStyle: endpoint != "", MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}
	testConditionalAndRanged(t, s, "conditional-test")
}
