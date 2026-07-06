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

package storage

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAliOSSLocation(t *testing.T) {
	cases := []struct {
		name        string
		conf        AliOSSConfig
		storagePath string
		want        string
	}{
		{
			name:        "normal",
			conf:        AliOSSConfig{Bucket: "mybucket", Endpoint: "oss-region.aliyuncs.com"},
			storagePath: "foo.mp4",
			want:        "https://mybucket.oss-region.aliyuncs.com/foo.mp4",
		},
		{
			name:        "endpoint with https:// prefix",
			conf:        AliOSSConfig{Bucket: "mybucket", Endpoint: "https://oss-region.aliyuncs.com"},
			storagePath: "foo.mp4",
			want:        "https://mybucket.oss-region.aliyuncs.com/foo.mp4",
		},
		{
			name:        "endpoint with http:// prefix",
			conf:        AliOSSConfig{Bucket: "mybucket", Endpoint: "http://oss-region.aliyuncs.com"},
			storagePath: "foo.mp4",
			want:        "https://mybucket.oss-region.aliyuncs.com/foo.mp4",
		},
		{
			name:        "nested path",
			conf:        AliOSSConfig{Bucket: "mybucket", Endpoint: "oss-region.aliyuncs.com"},
			storagePath: "folder/sub/foo.mp4",
			want:        "https://mybucket.oss-region.aliyuncs.com/folder/sub/foo.mp4",
		},
		{
			name:        "storagePath with leading slash",
			conf:        AliOSSConfig{Bucket: "mybucket", Endpoint: "oss-region.aliyuncs.com"},
			storagePath: "/foo.mp4",
			want:        "https://mybucket.oss-region.aliyuncs.com/foo.mp4",
		},
		{
			name:        "space in key",
			conf:        AliOSSConfig{Bucket: "mybucket", Endpoint: "oss-region.aliyuncs.com"},
			storagePath: "folder/my file.mp4",
			want:        "https://mybucket.oss-region.aliyuncs.com/folder/my%20file.mp4",
		},
		{
			name:        "unicode in key",
			conf:        AliOSSConfig{Bucket: "mybucket", Endpoint: "oss-region.aliyuncs.com"},
			storagePath: "café/résumé.mp4",
			want:        "https://mybucket.oss-region.aliyuncs.com/caf%C3%A9/r%C3%A9sum%C3%A9.mp4",
		},
	}
	for _, tc := range cases {
		s := &aliOSSStorage{conf: &tc.conf}
		got := s.location(tc.storagePath)
		require.Equal(t, tc.want, got, tc.name)
		requireWellFormedHTTPLocation(t, got, tc.name)
	}
}

func TestAzureLocation(t *testing.T) {
	cases := []struct {
		name        string
		conf        AzureConfig
		storagePath string
		want        string
	}{
		{
			name:        "normal",
			conf:        AzureConfig{AccountName: "acct", ContainerName: "mycontainer"},
			storagePath: "foo.mp4",
			want:        "https://acct.blob.core.windows.net/mycontainer/foo.mp4",
		},
		{
			name:        "empty container (customer regression)",
			conf:        AzureConfig{AccountName: "acct", ContainerName: ""},
			storagePath: "recordings/production/video_call_recordings/6893038.ogg",
			want:        "https://acct.blob.core.windows.net/recordings/production/video_call_recordings/6893038.ogg",
		},
		{
			name:        "container with leading slash",
			conf:        AzureConfig{AccountName: "acct", ContainerName: "/mycontainer"},
			storagePath: "foo.mp4",
			want:        "https://acct.blob.core.windows.net/mycontainer/foo.mp4",
		},
		{
			name:        "container with trailing slash",
			conf:        AzureConfig{AccountName: "acct", ContainerName: "mycontainer/"},
			storagePath: "foo.mp4",
			want:        "https://acct.blob.core.windows.net/mycontainer/foo.mp4",
		},
		{
			name:        "storagePath with leading slash",
			conf:        AzureConfig{AccountName: "acct", ContainerName: "mycontainer"},
			storagePath: "/foo.mp4",
			want:        "https://acct.blob.core.windows.net/mycontainer/foo.mp4",
		},
		{
			name:        "nested path",
			conf:        AzureConfig{AccountName: "acct", ContainerName: "mycontainer"},
			storagePath: "a/b/c.mp4",
			want:        "https://acct.blob.core.windows.net/mycontainer/a/b/c.mp4",
		},
		{
			name:        "space in key",
			conf:        AzureConfig{AccountName: "acct", ContainerName: "mycontainer"},
			storagePath: "my file.mp4",
			want:        "https://acct.blob.core.windows.net/mycontainer/my%20file.mp4",
		},
		{
			name:        "unicode in key",
			conf:        AzureConfig{AccountName: "acct", ContainerName: "mycontainer"},
			storagePath: "café/résumé.mp4",
			want:        "https://acct.blob.core.windows.net/mycontainer/caf%C3%A9/r%C3%A9sum%C3%A9.mp4",
		},
	}
	for _, tc := range cases {
		s := &azureBLOBStorage{conf: &tc.conf}
		got := s.location(tc.storagePath)
		require.Equal(t, tc.want, got, tc.name)
		requireWellFormedHTTPLocation(t, got, tc.name)
	}
}

func TestAzureLocationCustomEndpoint(t *testing.T) {
	cases := []struct {
		name        string
		conf        AzureConfig
		storagePath string
		want        string
	}{
		{
			name:        "azurite emulator preserves scheme and account path",
			conf:        AzureConfig{AccountName: "devstoreaccount1", ContainerName: "mycontainer", Endpoint: "http://127.0.0.1:10000/devstoreaccount1"},
			storagePath: "foo.mp4",
			want:        "http://127.0.0.1:10000/devstoreaccount1/mycontainer/foo.mp4",
		},
		{
			name:        "azurite emulator nested path",
			conf:        AzureConfig{AccountName: "devstoreaccount1", ContainerName: "mycontainer", Endpoint: "http://127.0.0.1:10000/devstoreaccount1"},
			storagePath: "a/b/c.mp4",
			want:        "http://127.0.0.1:10000/devstoreaccount1/mycontainer/a/b/c.mp4",
		},
		{
			name:        "sovereign cloud host",
			conf:        AzureConfig{AccountName: "acct", ContainerName: "mycontainer", Endpoint: "https://acct.blob.core.chinacloudapi.cn"},
			storagePath: "foo.mp4",
			want:        "https://acct.blob.core.chinacloudapi.cn/mycontainer/foo.mp4",
		},
	}
	for _, tc := range cases {
		s := &azureBLOBStorage{conf: &tc.conf}
		require.Equal(t, tc.want, s.location(tc.storagePath), tc.name)
	}
}

func TestGCPLocation(t *testing.T) {
	cases := []struct {
		name        string
		conf        GCPConfig
		storagePath string
		want        string
	}{
		{
			name:        "normal",
			conf:        GCPConfig{Bucket: "mybucket"},
			storagePath: "foo.mp4",
			want:        "https://mybucket.storage.googleapis.com/foo.mp4",
		},
		{
			name:        "nested path",
			conf:        GCPConfig{Bucket: "mybucket"},
			storagePath: "a/b/c.mp4",
			want:        "https://mybucket.storage.googleapis.com/a/b/c.mp4",
		},
		{
			name:        "storagePath with leading slash",
			conf:        GCPConfig{Bucket: "mybucket"},
			storagePath: "/foo.mp4",
			want:        "https://mybucket.storage.googleapis.com/foo.mp4",
		},
		{
			name:        "space in key",
			conf:        GCPConfig{Bucket: "mybucket"},
			storagePath: "folder/my file.mp4",
			want:        "https://mybucket.storage.googleapis.com/folder/my%20file.mp4",
		},
		{
			name:        "unicode in key",
			conf:        GCPConfig{Bucket: "mybucket"},
			storagePath: "café/résumé.mp4",
			want:        "https://mybucket.storage.googleapis.com/caf%C3%A9/r%C3%A9sum%C3%A9.mp4",
		},
	}
	for _, tc := range cases {
		s := &gcpStorage{conf: &tc.conf}
		got := s.location(tc.storagePath)
		require.Equal(t, tc.want, got, tc.name)
		requireWellFormedHTTPLocation(t, got, tc.name)
	}
}

func TestS3Location(t *testing.T) {
	cases := []struct {
		name        string
		conf        S3Config
		storagePath string
		want        string
	}{
		{
			name:        "vhost default endpoint",
			conf:        S3Config{Bucket: "mybucket"},
			storagePath: "foo.mp4",
			want:        "https://mybucket.s3.amazonaws.com/foo.mp4",
		},
		{
			name:        "vhost custom endpoint without scheme",
			conf:        S3Config{Bucket: "mybucket", Endpoint: "s3.example.com"},
			storagePath: "foo.mp4",
			want:        "https://mybucket.s3.example.com/foo.mp4",
		},
		{
			name:        "vhost custom endpoint with https://",
			conf:        S3Config{Bucket: "mybucket", Endpoint: "https://s3.example.com"},
			storagePath: "foo.mp4",
			want:        "https://mybucket.s3.example.com/foo.mp4",
		},
		{
			name:        "vhost custom endpoint with http://",
			conf:        S3Config{Bucket: "mybucket", Endpoint: "http://s3.example.com"},
			storagePath: "foo.mp4",
			want:        "https://mybucket.s3.example.com/foo.mp4",
		},
		{
			name:        "vhost nested path",
			conf:        S3Config{Bucket: "mybucket"},
			storagePath: "a/b/c.mp4",
			want:        "https://mybucket.s3.amazonaws.com/a/b/c.mp4",
		},
		{
			name:        "vhost storagePath with leading slash",
			conf:        S3Config{Bucket: "mybucket"},
			storagePath: "/foo.mp4",
			want:        "https://mybucket.s3.amazonaws.com/foo.mp4",
		},
		{
			name:        "vhost space in key",
			conf:        S3Config{Bucket: "mybucket"},
			storagePath: "folder/my file.mp4",
			want:        "https://mybucket.s3.amazonaws.com/folder/my%20file.mp4",
		},
		{
			name:        "vhost unicode in key",
			conf:        S3Config{Bucket: "mybucket"},
			storagePath: "café/résumé.mp4",
			want:        "https://mybucket.s3.amazonaws.com/caf%C3%A9/r%C3%A9sum%C3%A9.mp4",
		},
		{
			name:        "force path style default endpoint",
			conf:        S3Config{Bucket: "mybucket", ForcePathStyle: true},
			storagePath: "foo.mp4",
			want:        "https://s3.amazonaws.com/mybucket/foo.mp4",
		},
		{
			name:        "force path style custom endpoint",
			conf:        S3Config{Bucket: "mybucket", Endpoint: "https://minio.example.com", ForcePathStyle: true},
			storagePath: "foo.mp4",
			want:        "https://minio.example.com/mybucket/foo.mp4",
		},
		{
			name:        "force path style nested",
			conf:        S3Config{Bucket: "mybucket", ForcePathStyle: true},
			storagePath: "a/b/c.mp4",
			want:        "https://s3.amazonaws.com/mybucket/a/b/c.mp4",
		},
		{
			name:        "force path style storagePath with leading slash",
			conf:        S3Config{Bucket: "mybucket", ForcePathStyle: true},
			storagePath: "/foo.mp4",
			want:        "https://s3.amazonaws.com/mybucket/foo.mp4",
		},
	}
	for _, tc := range cases {
		s := &s3Storage{conf: &tc.conf}
		got := s.location(tc.storagePath)
		require.Equal(t, tc.want, got, tc.name)
		requireWellFormedHTTPLocation(t, got, tc.name)
	}
}

func TestLocalLocation(t *testing.T) {
	cases := []struct {
		name        string
		storageDir  string
		storagePath string
		want        string
	}{
		{
			name:        "normal",
			storageDir:  "/var/storage",
			storagePath: "foo.mp4",
			want:        "/var/storage/foo.mp4",
		},
		{
			name:        "nested path",
			storageDir:  "/var/storage",
			storagePath: "a/b/c.mp4",
			want:        "/var/storage/a/b/c.mp4",
		},
		{
			name:        "storagePath with leading slash",
			storageDir:  "/var/storage",
			storagePath: "/foo.mp4",
			want:        "/var/storage/foo.mp4",
		},
		{
			name:        "storageDir with trailing slash",
			storageDir:  "/var/storage/",
			storagePath: "foo.mp4",
			want:        "/var/storage/foo.mp4",
		},
	}
	for _, tc := range cases {
		u := &localUploader{StorageDir: tc.storageDir}
		require.Equal(t, tc.want, u.location(tc.storagePath))
	}
}

// requireWellFormedHTTPLocation verifies an https:// location URL is parseable,
// uses the https scheme, has a non-empty host, and contains no accidental
// double-slashes in its path (the original Azure bug).
func requireWellFormedHTTPLocation(t *testing.T, raw string, testName string) {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err, "%s: url should parse: %q", testName, raw)
	require.Equal(t, "https", u.Scheme, "%s: expected https scheme: %q", testName, raw)
	require.NotEmpty(t, u.Host, "%s: host should not be empty: %q", testName, raw)
	require.NotContains(t, u.Path, "//", "%s: url path should not contain //: %q", testName, raw)
}
