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
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/storage"
)

func testObjects(t *testing.T, s storage.Storage) {
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
		_, err = s.DownloadFile(ctx, filepath.Join(t.TempDir(), "missing"), missing)
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
