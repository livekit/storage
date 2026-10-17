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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

type localObjects struct {
	u *localUploader
}

func NewLocalObjects(conf *LocalConfig) (ObjectStorage, error) {
	u, err := NewLocal(conf)
	if err != nil {
		return nil, err
	}
	return &localObjects{u: u.(*localUploader)}, nil
}

func (o *localObjects) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	return o.UploadObject(ctx, storagePath, bytes.NewReader(data), opts)
}

// UploadObject writes a temporary file and links or renames it into place, so
// readers never see a partial object and IfAbsent is atomic.
func (o *localObjects) UploadObject(_ context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	dst := o.u.location(storagePath)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = io.Copy(tmp, body); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if !opts.IfAbsent {
		return os.Rename(tmp.Name(), dst)
	}
	if err = os.Link(tmp.Name(), dst); errors.Is(err, fs.ErrExist) {
		return ErrObjectExists
	}
	return err
}

func (o *localObjects) GetObject(_ context.Context, storagePath string) ([]byte, error) {
	data, err := os.ReadFile(o.u.location(storagePath))
	return data, localObjectError(err)
}

func (o *localObjects) GetObjectRange(_ context.Context, storagePath string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("storage: invalid range offset %d length %d", offset, length)
	}
	f, err := os.Open(o.u.location(storagePath))
	if err != nil {
		return nil, localObjectError(err)
	}
	defer f.Close()
	buf := make([]byte, length)
	n, err := f.ReadAt(buf, offset)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return buf[:n], err
}

func (o *localObjects) DownloadObject(_ context.Context, storagePath string, w io.Writer) (int64, error) {
	f, err := os.Open(o.u.location(storagePath))
	if err != nil {
		return 0, localObjectError(err)
	}
	defer f.Close()
	return io.Copy(w, f)
}

func (o *localObjects) ObjectExists(_ context.Context, storagePath string) (bool, error) {
	_, err := os.Stat(o.u.location(storagePath))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (o *localObjects) ListObjects(_ context.Context, prefix string) ([]string, error) {
	return o.u.ListObjects(prefix)
}

func (o *localObjects) DeleteObjects(_ context.Context, storagePaths []string) error {
	return o.u.DeleteObjects(storagePaths)
}

func (o *localObjects) GeneratePresignedUrl(_ context.Context, storagePath string, expiration time.Duration) (string, error) {
	return o.u.GeneratePresignedUrl(storagePath, expiration)
}

func localObjectError(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %w", ErrObjectNotFound, err)
	}
	return err
}
