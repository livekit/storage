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
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
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

func (u *localUploader) UploadFile(localPath, storagePath string, _ string) (string, int64, error) {
	storagePath = path.Join(u.StorageDir, storagePath)

	local, err := os.Open(localPath)
	if err != nil {
		return "", 0, err
	}
	defer local.Close()

	if dir, _ := path.Split(storagePath); dir != "" {
		if err = os.MkdirAll(dir, 0755); err != nil {
			return "", 0, err
		}
	}

	storage, err := os.Create(storagePath)
	if err != nil {
		return "", 0, err
	}
	defer storage.Close()

	size, err := io.Copy(storage, local)
	if err != nil {
		return "", 0, err
	}

	return storagePath, size, nil
}

func (u *localUploader) UploadData(data []byte, storagePath, _ string) (string, int64, error) {
	storagePath = path.Join(u.StorageDir, storagePath)

	if dir, _ := path.Split(storagePath); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", 0, err
		}
	}

	storage, err := os.Create(storagePath)
	if err != nil {
		return "", 0, err
	}
	defer storage.Close()

	size, err := storage.Write(data)
	if err != nil {
		return "", 0, err
	}

	return storagePath, int64(size), nil
}

func (u *localUploader) ListObjects(prefix string) ([]string, error) {
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

func (u *localUploader) DownloadData(storagePath string) ([]byte, error) {
	return os.ReadFile(path.Join(u.StorageDir, storagePath))
}

func (u *localUploader) DownloadFile(localPath, storagePath string) (int64, error) {
	storagePath = path.Join(u.StorageDir, storagePath)

	storage, err := os.Open(storagePath)
	if err != nil {
		return 0, err
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

func (u *localUploader) GeneratePresignedUrl(storagePath string, _ time.Duration) (string, error) {
	abs := filepath.Join(u.StorageDir, storagePath)
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String(), nil
}

func (u *localUploader) DeleteObject(storagePath string) error {
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

func (u *localUploader) DeleteObjects(storagePaths []string) error {
	for _, p := range storagePaths {
		if err := u.DeleteObject(p); err != nil {
			return err
		}
	}
	return nil
}
