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

package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type localUploader struct {
	StorageDir string
}

func NewLocal(conf *LocalConfig) (Storage, error) {
	dir, err := filepath.Abs(conf.StorageDir)
	if err != nil {
		return nil, err
	}

	return &localUploader{
		StorageDir: dir,
	}, nil
}

func (u *localUploader) UploadFile(ctx context.Context, localPath, storagePath, _ string) (string, int64, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	if err = u.UploadObject(ctx, storagePath, file, PutOptions{}); err != nil {
		return "", 0, err
	}
	return u.location(storagePath), stat.Size(), nil
}

func (u *localUploader) UploadData(ctx context.Context, data []byte, storagePath, _ string) (string, int64, error) {
	if err := u.PutObject(ctx, storagePath, data, PutOptions{}); err != nil {
		return "", 0, err
	}
	return u.location(storagePath), int64(len(data)), nil
}

func (u *localUploader) location(storagePath string) string {
	return path.Join(u.StorageDir, storagePath)
}

func (u *localUploader) listObjects(prefix string) ([]string, error) {
	absPrefix := path.Join(u.StorageDir, prefix)
	dir, filenamePrefix := path.Split(absPrefix)

	var files []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), filenamePrefix) {
			continue
		}

		entryPath := path.Join(dir, entry.Name())
		if entry.IsDir() {
			if err = filepath.Walk(entryPath, func(p string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if !info.IsDir() {
					rel, err := filepath.Rel(u.StorageDir, p)
					if err != nil {
						return err
					}
					files = append(files, rel)
				}
				return nil
			}); err != nil {
				return nil, err
			}
		} else {
			rel, err := filepath.Rel(u.StorageDir, entryPath)
			if err != nil {
				return nil, err
			}
			files = append(files, rel)
		}
	}

	return files, nil
}

func (u *localUploader) DownloadFile(_ context.Context, localPath, storagePath string) (int64, error) {
	storagePath = path.Join(u.StorageDir, storagePath)

	storage, err := os.Open(storagePath)
	if err != nil {
		return 0, localObjectError(err)
	}
	defer storage.Close()

	if dir, _ := path.Split(localPath); dir != "" {
		if err = os.MkdirAll(dir, 0755); err != nil {
			return 0, err
		}
	}

	local, err := os.Create(localPath)
	if err != nil {
		return 0, err
	}
	defer local.Close()

	size, err := io.Copy(local, storage)
	if err != nil {
		return 0, err
	}

	return size, nil
}

func (u *localUploader) presignedUrl(storagePath string, _ time.Duration) (string, error) {
	abs := filepath.Join(u.StorageDir, storagePath)
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String(), nil
}

// deleteObject removes the file, then each parent directory it leaves empty.
func (u *localUploader) deleteObject(storagePath string) error {
	storagePath = path.Join(u.StorageDir, storagePath)

	for {
		if err := os.Remove(storagePath); err != nil {
			return err
		}

		storagePath, _ = path.Split(storagePath)
		storagePath = storagePath[:len(storagePath)-1] // remove trailing slash
		entries, err := os.ReadDir(storagePath)
		if err != nil {
			return err
		}

		if storagePath == u.StorageDir || len(entries) > 0 {
			return nil
		}
	}
}

const localUploadPrefix = ".upload-"

func (u *localUploader) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	return u.UploadObject(ctx, storagePath, bytes.NewReader(data), opts)
}

// UploadObject writes a temporary file and links or renames it into place, so
// readers never see a partial object and IfAbsent is atomic.
func (u *localUploader) UploadObject(_ context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	dst := u.location(storagePath)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), localUploadPrefix+"*")
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

func (u *localUploader) GetObject(_ context.Context, storagePath string) ([]byte, error) {
	data, err := os.ReadFile(u.location(storagePath))
	return data, localObjectError(err)
}

func (u *localUploader) GetObjectRange(_ context.Context, storagePath string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("storage: invalid range offset %d length %d", offset, length)
	}
	f, err := os.Open(u.location(storagePath))
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

func (u *localUploader) DownloadObject(_ context.Context, storagePath string, w io.Writer) (int64, error) {
	f, err := os.Open(u.location(storagePath))
	if err != nil {
		return 0, localObjectError(err)
	}
	defer f.Close()
	return io.Copy(w, f)
}

func (u *localUploader) ObjectExists(_ context.Context, storagePath string) (bool, error) {
	_, err := os.Stat(u.location(storagePath))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// ListObjects leaves out UploadObject's temporary files, in progress or left by
// a crash.
func (u *localUploader) ListObjects(_ context.Context, prefix string) ([]string, error) {
	keys, err := u.listObjects(prefix)
	keys = slices.DeleteFunc(keys, func(k string) bool {
		return strings.HasPrefix(filepath.Base(k), localUploadPrefix)
	})
	slices.Sort(keys)
	return keys, err
}

func (u *localUploader) DeleteObjects(_ context.Context, storagePaths []string) error {
	for _, p := range storagePaths {
		if err := u.deleteObject(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (u *localUploader) GeneratePresignedUrl(_ context.Context, storagePath string, expiration time.Duration) (string, error) {
	return u.presignedUrl(storagePath, expiration)
}

func localObjectError(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %w", ErrObjectNotFound, err)
	}
	return err
}
