// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package storage_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/storage"
)

func TestLocalObjects(t *testing.T) {
	s, err := storage.NewObjectStorage(&storage.LocalConfig{StorageDir: t.TempDir()})
	require.NoError(t, err)
	testObjectStorage(t, s)
}

// TestS3Objects reaches AWS, or an S3-compatible store at S3_ENDPOINT.
func TestS3Objects(t *testing.T) {
	key := os.Getenv("AWS_ACCESS_KEY")
	secret := os.Getenv("AWS_SECRET")
	bucket := os.Getenv("S3_BUCKET")
	if key == "" || secret == "" || bucket == "" {
		t.Skip("Missing env vars")
	}
	endpoint := os.Getenv("S3_ENDPOINT")
	s, err := storage.NewObjectStorage(&storage.S3Config{
		AccessKey:      key,
		Secret:         secret,
		SessionToken:   os.Getenv("AWS_SESSION_TOKEN"),
		Region:         os.Getenv("AWS_REGION"),
		Endpoint:       endpoint,
		ForcePathStyle: endpoint != "",
		Bucket:         bucket,
	})
	require.NoError(t, err)
	testObjectStorage(t, s)
}

func testObjectStorage(t *testing.T, s storage.ObjectStorage) {
	ctx := context.Background()
	prefix := fmt.Sprintf("objects-test-%d/", time.Now().UnixNano())
	small := prefix + "small"
	large := prefix + "large"
	missing := prefix + "missing"
	payload := []byte("hello from PutObject")
	// Larger than one upload part, so UploadObject takes the multipart path.
	big := bytes.Repeat([]byte("0123456789abcdef"), (17<<20)/16)

	t.Cleanup(func() {
		_ = s.DeleteObjects(ctx, []string{small, large})
	})

	t.Run("PutObject IfAbsent", func(t *testing.T) {
		require.NoError(t, s.PutObject(ctx, small, payload, storage.PutOptions{IfAbsent: true}))
		err := s.PutObject(ctx, small, []byte("other"), storage.PutOptions{IfAbsent: true})
		require.ErrorIs(t, err, storage.ErrObjectExists)
		got, err := s.GetObject(ctx, small)
		require.NoError(t, err)
		require.Equal(t, payload, got)
	})

	t.Run("UploadObject", func(t *testing.T) {
		require.NoError(t, s.UploadObject(ctx, large, bytes.NewReader(big), storage.PutOptions{IfAbsent: true}))
		err := s.UploadObject(ctx, large, bytes.NewReader(big), storage.PutOptions{IfAbsent: true})
		require.ErrorIs(t, err, storage.ErrObjectExists)
		var buf bytes.Buffer
		n, err := s.DownloadObject(ctx, large, &buf)
		require.NoError(t, err)
		require.Equal(t, int64(len(big)), n)
		require.Equal(t, big, buf.Bytes())
	})

	t.Run("GetObjectRange", func(t *testing.T) {
		got, err := s.GetObjectRange(ctx, small, 6, 4)
		require.NoError(t, err)
		require.Equal(t, payload[6:10], got)
		got, err = s.GetObjectRange(ctx, small, 6, 1000)
		require.NoError(t, err)
		require.Equal(t, payload[6:], got)
		got, err = s.GetObjectRange(ctx, small, int64(len(payload)), 4)
		require.NoError(t, err)
		require.Empty(t, got)
	})

	t.Run("not found", func(t *testing.T) {
		_, err := s.GetObject(ctx, missing)
		require.ErrorIs(t, err, storage.ErrObjectNotFound)
		_, err = s.GetObjectRange(ctx, missing, 0, 1)
		require.ErrorIs(t, err, storage.ErrObjectNotFound)
		exists, err := s.ObjectExists(ctx, missing)
		require.NoError(t, err)
		require.False(t, exists)
		exists, err = s.ObjectExists(ctx, small)
		require.NoError(t, err)
		require.True(t, exists)
	})

	t.Run("ListObjects and DeleteObjects", func(t *testing.T) {
		keys, err := s.ListObjects(ctx, prefix)
		require.NoError(t, err)
		sort.Strings(keys)
		require.Equal(t, []string{large, small}, keys)
		require.NoError(t, s.DeleteObjects(ctx, []string{small, large, missing}))
		keys, err = s.ListObjects(ctx, prefix)
		require.NoError(t, err)
		require.Empty(t, keys)
	})

	t.Run("GeneratePresignedUrl", func(t *testing.T) {
		url, err := s.GeneratePresignedUrl(ctx, small, time.Minute)
		require.NoError(t, err)
		require.NotEmpty(t, url)
	})
}

// TestOCIObjects reaches OCI through its S3 compatibility API, as TestOCI does.
func TestOCIObjects(t *testing.T) {
	key := os.Getenv("OCI_ACCESS_KEY")
	secret := os.Getenv("OCI_SECRET")
	region := os.Getenv("OCI_REGION")
	endpoint := os.Getenv("OCI_ENDPOINT")
	bucket := os.Getenv("OCI_BUCKET")
	if key == "" || secret == "" || region == "" || endpoint == "" || bucket == "" {
		t.Skip("Missing env vars")
	}
	s, err := storage.NewObjectStorage(&storage.S3Config{
		AccessKey:      key,
		Secret:         secret,
		Region:         region,
		Endpoint:       endpoint,
		Bucket:         bucket,
		ForcePathStyle: true,
	})
	require.NoError(t, err)
	testObjectStorage(t, s)
}

// TestOCINativeObjects uses the OCI SDK with an API key.
func TestOCINativeObjects(t *testing.T) {
	conf := &storage.OCIConfig{
		TenancyOCID: os.Getenv("OCI_TENANCY_OCID"),
		UserOCID:    os.Getenv("OCI_USER_OCID"),
		Fingerprint: os.Getenv("OCI_FINGERPRINT"),
		PrivateKey:  os.Getenv("OCI_PRIVATE_KEY"),
		Region:      os.Getenv("OCI_REGION"),
		Bucket:      os.Getenv("OCI_BUCKET"),
	}
	if conf.TenancyOCID == "" || conf.UserOCID == "" || conf.Fingerprint == "" || conf.PrivateKey == "" || conf.Region == "" || conf.Bucket == "" {
		t.Skip("Missing env vars")
	}
	s, err := storage.NewObjectStorage(conf)
	require.NoError(t, err)
	testObjectStorage(t, s)
}

func TestGCPObjects(t *testing.T) {
	creds := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	bucket := os.Getenv("GCP_BUCKET")
	if creds == "" || bucket == "" {
		t.Skip("Missing env vars")
	}
	s, err := storage.NewObjectStorage(&storage.GCPConfig{CredentialsJSON: creds, Bucket: bucket})
	require.NoError(t, err)
	testObjectStorage(t, s)
}

func TestAzureObjects(t *testing.T) {
	name := os.Getenv("AZURE_ACCOUNT_NAME")
	key := os.Getenv("AZURE_ACCOUNT_KEY")
	container := os.Getenv("AZURE_CONTAINER_NAME")
	if name == "" || key == "" || container == "" {
		t.Skip("Missing env vars")
	}
	s, err := storage.NewObjectStorage(&storage.AzureConfig{AccountName: name, AccountKey: key, ContainerName: container})
	require.NoError(t, err)
	testObjectStorage(t, s)
}

func TestAliOSSObjects(t *testing.T) {
	key := os.Getenv("ALI_ACCESS_KEY")
	secret := os.Getenv("ALI_SECRET")
	endpoint := os.Getenv("ALI_ENDPOINT")
	bucket := os.Getenv("ALI_BUCKET")
	if key == "" || secret == "" || endpoint == "" || bucket == "" {
		t.Skip("Missing env vars")
	}
	s, err := storage.NewObjectStorage(&storage.AliOSSConfig{AccessKey: key, Secret: secret, Endpoint: endpoint, Bucket: bucket})
	require.NoError(t, err)
	testObjectStorage(t, s)
}
