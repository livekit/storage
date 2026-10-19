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

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/storage"
)

// objectsSuite is what testObjectsWith runs beyond the core cases. Its gaps
// name what an emulator gets wrong, and skip the cases that would show it; a
// real store has none.
type objectsSuite struct {
	// bulk deletes more keys than one batch takes, too slow for a real account.
	bulk bool
	// singlePartIfAbsentRaces: two racing IfAbsent puts can both win.
	singlePartIfAbsentRaces bool
	// multipartIfAbsentRaces: two racing IfAbsent multipart uploads can both win.
	multipartIfAbsentRaces bool
	// multipartIgnoresIfAbsent: a multipart upload's commit ignores IfAbsent.
	multipartIgnoresIfAbsent bool
	// overwritesInPlace: a write replaces an object as it goes, so a failed
	// one leaves none (local, as a filesystem).
	overwritesInPlace bool
	// commit tells the request that makes a multipart upload's object, and
	// whether it carries IfAbsent's condition, which a store that ignores the
	// condition cannot show.
	commit func(r *http.Request) (commit, conditional bool)
}

func testObjects(t *testing.T, s storage.Storage) {
	testObjectsWith(t, s, objectsSuite{})
}

func testObjectsWith(t *testing.T, s storage.Storage, suite objectsSuite) {
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
		require.ErrorIs(t, err, storage.ErrExists)
		got, err := s.GetObject(ctx, small)
		require.NoError(t, err)
		require.Equal(t, payload, got)
	})

	t.Run("UploadObject", func(t *testing.T) {
		require.NoError(t, s.UploadObject(ctx, large, bytes.NewReader(big), storage.PutOptions{IfAbsent: true}))
		err := s.UploadObject(ctx, large, bytes.NewReader(big), storage.PutOptions{IfAbsent: true})
		require.ErrorIs(t, err, storage.ErrExists)
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
		require.ErrorIs(t, err, storage.ErrNotFound)
		_, err = s.GetObjectRange(ctx, missing, 0, 1)
		require.ErrorIs(t, err, storage.ErrNotFound)
		_, err = s.DownloadObject(ctx, missing, io.Discard)
		require.ErrorIs(t, err, storage.ErrNotFound)
		// A failed download leaves the file it would have written as it was.
		kept := filepath.Join(t.TempDir(), "kept")
		require.NoError(t, os.WriteFile(kept, []byte("kept"), 0o600))
		_, err = s.DownloadFile(ctx, missing, kept)
		require.ErrorIs(t, err, storage.ErrNotFound)
		got, err := os.ReadFile(kept)
		require.NoError(t, err)
		require.Equal(t, []byte("kept"), got)
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

	t.Run("PresignedURL", func(t *testing.T) {
		url, err := s.PresignedURL(ctx, small, time.Minute)
		require.NoError(t, err)
		require.NotEmpty(t, url)
	})

	t.Run("IfAbsent race", func(t *testing.T) {
		sizes := map[string][]byte{}
		if !suite.singlePartIfAbsentRaces {
			sizes["single part"] = payload
		}
		if !suite.multipartIfAbsentRaces && !suite.multipartIgnoresIfAbsent {
			sizes["multipart"] = big
		}
		for name, body := range sizes {
			t.Run(name, func(t *testing.T) {
				testIfAbsentRace(t, s, prefix+"race-"+name, body)
			})
		}
	})

	// An upload whose body fails partway stores nothing: a key it would create
	// stays empty, and an object it would replace keeps its bytes.
	t.Run("failed body", func(t *testing.T) {
		for name, size := range map[string]int{"single part": 1 << 10, "multipart": 17 << 20} {
			t.Run(name, func(t *testing.T) {
				path := prefix + "failed-body/" + strings.ReplaceAll(name, " ", "-")
				t.Cleanup(func() { _ = s.DeleteObjects(context.Background(), []string{path}) })
				failing := func() io.Reader {
					return io.MultiReader(bytes.NewReader(make([]byte, size)), iotest.ErrReader(errors.New("body failed")))
				}

				require.Error(t, s.UploadObject(ctx, path, failing(), storage.PutOptions{}))
				exists, err := s.ObjectExists(ctx, path)
				require.NoError(t, err)
				require.False(t, exists, "a failed upload created the object")

				require.NoError(t, s.PutObject(ctx, path, []byte("old"), storage.PutOptions{}))
				require.Error(t, s.UploadObject(ctx, path, failing(), storage.PutOptions{}))
				got, err := s.GetObject(ctx, path)
				if suite.overwritesInPlace {
					require.ErrorIs(t, err, storage.ErrNotFound)
					return
				}
				require.NoError(t, err)
				require.Equal(t, []byte("old"), got, "a failed upload replaced the object")
			})
		}
	})

	t.Run("edge objects", func(t *testing.T) {
		empty := prefix + "edge/empty"
		odd := prefix + "edge/a dir/caf\u00e9 & r\u00e9sum\u00e9+1.txt"
		nested := prefix + "edge/a dir/deeper/leaf"
		t.Cleanup(func() { _ = s.DeleteObjects(ctx, []string{empty, odd, nested}) })

		require.NoError(t, s.PutObject(ctx, empty, nil, storage.PutOptions{}))
		got, err := s.GetObject(ctx, empty)
		require.NoError(t, err)
		require.Empty(t, got)
		got, err = s.GetObjectRange(ctx, empty, 0, 4)
		require.NoError(t, err)
		require.Empty(t, got)

		require.NoError(t, s.PutObject(ctx, odd, []byte("first"), storage.PutOptions{}))
		require.NoError(t, s.PutObject(ctx, odd, []byte("second"), storage.PutOptions{}))
		got, err = s.GetObject(ctx, odd)
		require.NoError(t, err)
		require.Equal(t, []byte("second"), got)
		require.NoError(t, s.PutObject(ctx, nested, []byte("leaf"), storage.PutOptions{}))

		keys, err := s.ListObjects(ctx, prefix+"edge/")
		require.NoError(t, err)
		require.Equal(t, []string{odd, nested, empty}, keys)
		exists, err := s.ObjectExists(ctx, prefix+"edge/a dir")
		require.NoError(t, err)
		require.False(t, exists, "a prefix is not an object")

		// A prefix matches keys byte for byte, as on S3.
		sibling := prefix + "edge/a dirt"
		t.Cleanup(func() { _ = s.DeleteObjects(ctx, []string{sibling}) })
		require.NoError(t, s.PutObject(ctx, sibling, []byte("sibling"), storage.PutOptions{}))
		keys, err = s.ListObjects(ctx, prefix+"edge/a dir/")
		require.NoError(t, err)
		require.Equal(t, []string{odd, nested}, keys)
		keys, err = s.ListObjects(ctx, prefix+"edge/a dir")
		require.NoError(t, err)
		require.Equal(t, []string{odd, nested, sibling}, keys)

		_, err = s.GetObject(ctx, odd+"/child")
		require.ErrorIs(t, err, storage.ErrNotFound, "a path through an object")
		// A prefix holds no object, so deleting it succeeds and removes nothing.
		require.NoError(t, s.DeleteObjects(ctx, []string{prefix + "edge/a dir"}))
		exists, err = s.ObjectExists(ctx, nested)
		require.NoError(t, err)
		require.True(t, exists)
		require.NoError(t, s.DeleteObjects(ctx, []string{sibling}))

		require.NoError(t, s.DeleteObjects(ctx, []string{empty, odd, nested}))
		keys, err = s.ListObjects(ctx, prefix+"edge/")
		require.NoError(t, err)
		require.Empty(t, keys)
	})

	if suite.bulk {
		t.Run("bulk delete", func(t *testing.T) {
			// One past S3's and OSS's 1000 keys per request.
			paths := make([]string, 1001)
			for i := range paths {
				paths[i] = fmt.Sprintf("%sbulk/%04d", prefix, i)
				require.NoError(t, s.PutObject(ctx, paths[i], []byte("x"), storage.PutOptions{}))
			}
			keys, err := s.ListObjects(ctx, prefix+"bulk/")
			require.NoError(t, err)
			require.True(t, sort.StringsAreSorted(keys))
			require.Equal(t, paths, keys)
			require.NoError(t, s.DeleteObjects(ctx, paths))
			keys, err = s.ListObjects(ctx, prefix+"bulk/")
			require.NoError(t, err)
			require.Empty(t, keys)
		})
	}
}

// testIfAbsentRace sends two IfAbsent uploads of body to path that both pass
// any check before uploading: each body holds its first read until the other's
// starts. Only the store's condition can then refuse one, and exactly one must
// win.
func testIfAbsentRace(t *testing.T, s storage.Storage, path string, body []byte) {
	ctx := context.Background()
	t.Cleanup(func() { _ = s.DeleteObjects(ctx, []string{path}) })

	var arrived sync.WaitGroup
	var late atomic.Bool
	arrived.Add(2)
	errs := make(chan error, 2)
	for i := range 2 {
		r := &heldReader{r: bytes.NewReader(append([]byte{byte(i)}, body...)), arrived: &arrived, late: &late}
		go func() { errs <- s.UploadObject(ctx, path, r, storage.PutOptions{IfAbsent: true}) }()
	}
	var won []byte
	for range 2 {
		err := <-errs
		if err == nil {
			require.Nil(t, won, "both uploads won")
			won = []byte{}
			continue
		}
		require.ErrorIs(t, err, storage.ErrExists)
	}
	require.NotNil(t, won, "no upload won")
	require.False(t, late.Load(), "the uploads never overlapped, so nothing raced")
	// The object is one upload's whole body: the loser wrote nothing.
	got, err := s.GetObject(ctx, path)
	require.NoError(t, err)
	require.Contains(t, [][]byte{append([]byte{0}, body...), append([]byte{1}, body...)}, got)
}

// heldReader holds its first read until every reader sharing arrived has
// started one, or a while has passed, when it sets late.
type heldReader struct {
	r       io.Reader
	arrived *sync.WaitGroup
	late    *atomic.Bool
	once    sync.Once
}

func (h *heldReader) Read(p []byte) (int, error) {
	h.once.Do(func() {
		h.arrived.Done()
		done := make(chan struct{})
		go func() { h.arrived.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			h.late.Store(true)
		}
	})
	return h.r.Read(p)
}
