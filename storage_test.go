// Copyright 2025 LiveKit, Inc.
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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/joho/godotenv/autoload"
	"github.com/stretchr/testify/require"

	"github.com/livekit/storage"
)

func TestAliOSS(t *testing.T) {
	key := os.Getenv("ALI_ACCESS_KEY")
	secret := os.Getenv("ALI_SECRET")
	endpoint := os.Getenv("ALI_ENDPOINT")
	bucket := os.Getenv("ALI_BUCKET")

	if key == "" || secret == "" || endpoint == "" || bucket == "" {
		t.Skip("Missing env vars")
	}

	s, err := storage.New(&storage.AliOSSConfig{
		AccessKey: key,
		Secret:    secret,
		Endpoint:  endpoint,
		Bucket:    bucket,
	})
	require.NoError(t, err)

	testStorage(t, s)
}

func TestAzure(t *testing.T) {
	name := os.Getenv("AZURE_ACCOUNT_NAME")
	key := os.Getenv("AZURE_ACCOUNT_KEY")
	container := os.Getenv("AZURE_CONTAINER_NAME")

	if name == "" || key == "" || container == "" {
		t.Skip("Missing env vars")
	}

	s, err := storage.New(&storage.AzureConfig{
		AccountName:   name,
		AccountKey:    key,
		ContainerName: container,
	})
	require.NoError(t, err)

	testStorage(t, s)
}

func TestGCP(t *testing.T) {
	creds := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	bucket := os.Getenv("GCP_BUCKET")

	if creds == "" || bucket == "" {
		t.Skip("Missing env vars")
	}

	s, err := storage.New(&storage.GCPConfig{
		CredentialsJSON: creds,
		Bucket:          bucket,
	})
	require.NoError(t, err)

	testStorage(t, s)
}

func TestLocal(t *testing.T) {
	s, err := storage.New(&storage.LocalConfig{})
	require.NoError(t, err)

	testStorage(t, s)
}

func TestOCI(t *testing.T) {
	key := os.Getenv("OCI_ACCESS_KEY")
	secret := os.Getenv("OCI_SECRET")
	region := os.Getenv("OCI_REGION")
	endpoint := os.Getenv("OCI_ENDPOINT")
	bucket := os.Getenv("OCI_BUCKET")

	if key == "" || secret == "" || region == "" || endpoint == "" || bucket == "" {
		t.Skip("Missing env vars")
	}

	s, err := storage.New(&storage.S3Config{
		AccessKey:      key,
		Secret:         secret,
		Region:         region,
		Endpoint:       endpoint,
		Bucket:         bucket,
		ForcePathStyle: true,
	})
	require.NoError(t, err)

	testStorage(t, s)
}

func TestSupabase(t *testing.T) {
	key := os.Getenv("SUPABASE_ACCESS_KEY")
	secret := os.Getenv("SUPABASE_SECRET")
	region := os.Getenv("SUPABASE_REGION")
	endpoint := os.Getenv("SUPABASE_ENDPOINT")
	bucket := os.Getenv("SUPABASE_BUCKET")

	if key == "" || secret == "" || region == "" || endpoint == "" || bucket == "" {
		t.Skip("Missing env vars")
	}

	s, err := storage.New(&storage.S3Config{
		AccessKey:      key,
		Secret:         secret,
		Region:         region,
		Endpoint:       endpoint,
		Bucket:         bucket,
		ForcePathStyle: true,
	})
	require.NoError(t, err)

	testStorage(t, s)
}

func TestS3(t *testing.T) {
	key := os.Getenv("AWS_ACCESS_KEY")
	secret := os.Getenv("AWS_SECRET")
	bucket := os.Getenv("S3_BUCKET")

	if key == "" || secret == "" || bucket == "" {
		t.Skip("Missing env vars")
	}

	s, err := storage.New(&storage.S3Config{
		AccessKey:    key,
		Secret:       secret,
		SessionToken: os.Getenv("AWS_SESSION_TOKEN"),
		Region:       os.Getenv("AWS_REGION"),
		Bucket:       bucket,
	})
	require.NoError(t, err)

	testStorage(t, s)
}

func testStorage(t *testing.T, s storage.Storage) {
	prefix := fmt.Sprintf("test-%d", time.Now().UnixNano())
	pathData := prefix + "-data.txt"
	pathFile := prefix + "-file.txt"
	pathBulk1 := prefix + "-bulk-1.txt"
	pathBulk2 := prefix + "-bulk-2.txt"

	dataPayload := []byte("hello from UploadData")
	filePayload := []byte("hello from UploadFile")
	bulk1Payload := []byte("bulk delete 1")
	bulk2Payload := []byte("bulk delete 2")

	// best-effort cleanup so a mid-test failure doesn't leave orphans in the bucket
	t.Cleanup(func() {
		_ = s.DeleteObjects([]string{pathData, pathFile, pathBulk1, pathBulk2})
	})

	t.Run("UploadData", func(t *testing.T) {
		loc, size, err := s.UploadData(dataPayload, pathData, "text/plain")
		require.NoError(t, err)
		require.Equal(t, int64(len(dataPayload)), size)
		assertLocation(t, loc, pathData)
	})

	t.Run("UploadFile", func(t *testing.T) {
		tmp, err := os.CreateTemp("", "storage-upload-*.txt")
		require.NoError(t, err)
		defer os.Remove(tmp.Name())

		_, err = tmp.Write(filePayload)
		require.NoError(t, err)
		require.NoError(t, tmp.Close())

		loc, size, err := s.UploadFile(tmp.Name(), pathFile, "text/plain")
		require.NoError(t, err)
		require.Equal(t, int64(len(filePayload)), size)
		assertLocation(t, loc, pathFile)
	})

	t.Run("ListObjects", func(t *testing.T) {
		items, err := s.ListObjects(prefix)
		require.NoError(t, err)
		require.Len(t, items, 2)

		require.True(t, hasSuffixIn(items, pathData), "expected %s in %v", pathData, items)
		require.True(t, hasSuffixIn(items, pathFile), "expected %s in %v", pathFile, items)
	})

	t.Run("DownloadData", func(t *testing.T) {
		downloaded, err := s.DownloadData(pathData)
		require.NoError(t, err)
		require.Equal(t, dataPayload, downloaded)
	})

	t.Run("DownloadFile", func(t *testing.T) {
		tmp, err := os.CreateTemp("", "storage-download-*.txt")
		require.NoError(t, err)
		require.NoError(t, tmp.Close())
		defer os.Remove(tmp.Name())

		size, err := s.DownloadFile(tmp.Name(), pathFile)
		require.NoError(t, err)
		require.Equal(t, int64(len(filePayload)), size)

		got, err := os.ReadFile(tmp.Name())
		require.NoError(t, err)
		require.Equal(t, filePayload, got)
	})

	t.Run("GeneratePresignedUrl", func(t *testing.T) {
		rawURL, err := s.GeneratePresignedUrl(pathData, 5*time.Minute)
		if err != nil {
			// Some configurations (e.g. Azure without OAuth) don't support presigning;
			// the method exists and reported a clear error, which is the contract.
			t.Logf("presigned URL not available in this config: %v", err)
			return
		}
		require.NotEmpty(t, rawURL)

		// The URL should at least be parseable and reference the key we asked for.
		parsed, err := url.Parse(rawURL)
		require.NoError(t, err)
		require.True(t, strings.HasSuffix(parsed.Path, pathData), "url path %q should end with %q", parsed.Path, pathData)

		// Only fetch when the URL is an HTTP(S) URL — local storage returns file://.
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return
		}

		resp, err := http.Get(rawURL)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, dataPayload, body)
	})

	t.Run("DeleteObject", func(t *testing.T) {
		require.NoError(t, s.DeleteObject(pathData))

		items, err := s.ListObjects(prefix)
		require.NoError(t, err)
		require.Len(t, items, 1)
		require.True(t, strings.HasSuffix(items[0], pathFile))
	})

	t.Run("DeleteObjects", func(t *testing.T) {
		// Upload two more objects, then bulk-delete them along with the leftover pathFile.
		_, _, err := s.UploadData(bulk1Payload, pathBulk1, "text/plain")
		require.NoError(t, err)
		_, _, err = s.UploadData(bulk2Payload, pathBulk2, "text/plain")
		require.NoError(t, err)

		require.NoError(t, s.DeleteObjects([]string{pathFile, pathBulk1, pathBulk2}))

		items, err := s.ListObjects(prefix)
		require.NoError(t, err)
		require.Empty(t, items)
	})

	t.Run("Multipart", func(t *testing.T) {
		pathMultipart := prefix + "-multipart.bin"
		t.Cleanup(func() {
			_ = s.DeleteObject(pathMultipart)
		})

		// 6 MiB exceeds the S3 manager's default 5 MiB part size, forcing the
		// multipart upload path (CreateMultipartUpload / UploadPart / Complete).
		payload := make([]byte, 6<<20)
		for i := range payload {
			payload[i] = byte(i)
		}

		loc, size, err := s.UploadData(payload, pathMultipart, "application/octet-stream")
		require.NoError(t, err)
		require.Equal(t, int64(len(payload)), size)
		assertLocation(t, loc, pathMultipart)

		downloaded, err := s.DownloadData(pathMultipart)
		require.NoError(t, err)
		require.Equal(t, len(payload), len(downloaded))
		require.True(t, bytes.Equal(payload, downloaded), "downloaded multipart content does not match uploaded")
	})
}

func hasSuffixIn(items []string, suffix string) bool {
	for _, item := range items {
		if strings.HasSuffix(item, suffix) {
			return true
		}
	}
	return false
}

// assertLocation verifies the URL returned by UploadData/UploadFile is non-empty,
// parseable, and references the key we just uploaded.
func assertLocation(t *testing.T, loc, key string) {
	t.Helper()
	require.NotEmpty(t, loc)
	parsed, err := url.Parse(loc)
	require.NoError(t, err, "location %q should be a parseable URL", loc)
	require.True(t, strings.HasSuffix(parsed.Path, "/"+key) || strings.HasSuffix(parsed.Path, key),
		"location path %q should end with key %q (full url: %q)", parsed.Path, key, loc)
}
