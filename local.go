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
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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

// file is the path that holds storagePath's object. A key that is empty or
// leads out of StorageDir names no object and is refused.
func (u *localUploader) file(storagePath string) (string, error) {
	p := filepath.Join(u.StorageDir, filepath.FromSlash(storagePath))
	rel, err := filepath.Rel(u.StorageDir, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("storage: key %q is not under %s", storagePath, u.StorageDir)
	}
	return p, nil
}

func (u *localUploader) UploadFile(ctx context.Context, storagePath, localPath string, opts PutOptions) (string, int64, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	return u.upload(ctx, storagePath, file, opts)
}

func (u *localUploader) DownloadFile(_ context.Context, storagePath, localPath string) (int64, error) {
	f, err := u.open(storagePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return downloadFile(localPath, func(local *os.File) (int64, error) {
		return io.Copy(local, f)
	})
}

const localUploadPrefix = ".upload-"

func (u *localUploader) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	_, _, err := u.upload(ctx, storagePath, bytes.NewReader(data), opts)
	return err
}

func (u *localUploader) UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	_, _, err := u.upload(ctx, storagePath, body, opts)
	return err
}

// upload writes the object in place, as on main: a mounted volume (gcsfuse,
// s3fs) sends a rename to its store as a copy, and a consumer watching for the
// file to be closed sees it. A reader can see the object while it is written,
// and a failed write removes it. IfAbsent writes a temporary file and links it
// into place, which is atomic. It returns the object's path and the bytes
// written, synced.
func (u *localUploader) upload(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) (string, int64, error) {
	dst, err := u.file(storagePath)
	if err != nil {
		return "", 0, err
	}
	if err = ctx.Err(); err != nil {
		return "", 0, err
	}
	if opts.IfAbsent {
		return uploadIfAbsent(dst, body)
	}

	f, err := createFile(dst, os.O_TRUNC)
	if err != nil {
		return "", 0, err
	}
	n, err := writeFile(f, body)
	if err != nil {
		_ = os.Remove(dst)
		return "", 0, err
	}
	return dst, n, nil
}

func uploadIfAbsent(dst string, body io.Reader) (string, int64, error) {
	tmp, err := createTemp(filepath.Dir(dst), localUploadPrefix)
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())

	n, err := writeFile(tmp, body)
	if err != nil {
		return "", 0, err
	}
	// A hard link fails where it already exists, and on filesystems without
	// hard links, such as SMB and most FUSE mounts.
	if err = os.Link(tmp.Name(), dst); errors.Is(err, fs.ErrExist) {
		return "", 0, ErrExists
	}
	if err != nil {
		return "", 0, err
	}
	// The directory holds the new name: syncing it keeps the object across a
	// crash. Best effort, as some filesystems, FUSE mounts among them, refuse
	// to sync a directory.
	if d, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return dst, n, nil
}

func (u *localUploader) GetObject(_ context.Context, storagePath string) ([]byte, error) {
	f, err := u.open(storagePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (u *localUploader) GetObjectRange(_ context.Context, storagePath string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("storage: invalid range offset %d length %d", offset, length)
	}
	f, err := u.open(storagePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	buf := make([]byte, max(0, min(length, info.Size()-offset)))
	n, err := f.ReadAt(buf, offset)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return buf[:n], err
}

func (u *localUploader) DownloadObject(_ context.Context, storagePath string, w io.Writer) (int64, error) {
	f, err := u.open(storagePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(w, f)
}

// open is the object's file. A directory is no object, as a prefix is not on
// a store.
func (u *localUploader) open(storagePath string) (*os.File, error) {
	src, err := u.file(storagePath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(src)
	if err != nil {
		return nil, localObjectError(err)
	}
	if info, err := f.Stat(); err != nil || info.IsDir() {
		f.Close()
		if err == nil {
			err = syscall.EISDIR
		}
		return nil, localObjectError(err)
	}
	return f, nil
}

// ObjectExists reports a directory as no object, as a store reports a prefix.
func (u *localUploader) ObjectExists(_ context.Context, storagePath string) (bool, error) {
	src, err := u.file(storagePath)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(src)
	if errors.Is(localObjectError(err), ErrNotFound) {
		return false, nil
	}
	return err == nil && !info.IsDir(), err
}

// ListObjects walks the directory the prefix names and keeps the keys that
// start with it, as on S3: "a/" lists a's files, "a" those of a, ab and a.txt.
// A leading "/" is dropped, as from keys. UploadObject's temporary files, in
// progress or left by a crash, are left out.
func (u *localUploader) ListObjects(_ context.Context, prefix string) ([]string, error) {
	prefix = strings.TrimLeft(prefix, "/")
	root := u.StorageDir
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		dir, err := u.file(prefix[:i])
		if err != nil {
			return nil, err
		}
		root = dir
	}

	// The trailing separator has the walk follow a root that is a symlink.
	walkRoot := root + string(filepath.Separator)
	var keys []string
	err := filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory a delete removes mid-walk held no more keys; only a
			// missing root means the prefix holds none.
			if p != walkRoot && errors.Is(localObjectError(err), ErrNotFound) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(u.StorageDir, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if d.IsDir() {
			// A directory none of whose keys can start with the prefix is not
			// walked, so a listing reads only what it may return.
			if p != walkRoot && !strings.HasPrefix(key+"/", prefix) && !strings.HasPrefix(prefix, key+"/") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasPrefix(d.Name(), localUploadPrefix) && strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if errors.Is(localObjectError(err), ErrNotFound) {
		return nil, nil
	}
	slices.Sort(keys)
	return keys, err
}

func (u *localUploader) DeleteObjects(ctx context.Context, storagePaths []string) error {
	for _, p := range storagePaths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := u.deleteObject(p); err != nil {
			return err
		}
	}
	return nil
}

// deleteObject removes the file, then each parent directory it leaves empty. A
// key that holds no file, a directory among them, is already deleted.
func (u *localUploader) deleteObject(storagePath string) error {
	p, err := u.file(storagePath)
	if err != nil {
		return err
	}
	info, err := os.Lstat(p)
	if errors.Is(localObjectError(err), ErrNotFound) || (err == nil && info.IsDir()) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	// A directory another upload just wrote into is not empty, and stays.
	for dir := filepath.Dir(p); dir != u.StorageDir; dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			return nil
		}
	}
	return nil
}

func (u *localUploader) PresignedURL(_ context.Context, storagePath string, _ time.Duration) (string, error) {
	p, err := u.file(storagePath)
	if err != nil {
		return "", err
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(p)}).String(), nil
}

// localObjectError reports a missing file as ErrNotFound, and so a path
// through a file (ENOTDIR) or onto a directory (EISDIR), where no object is.
func localObjectError(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}
