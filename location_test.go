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
