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
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
)

type localUploader struct {
}

func NewLocal() Storage {
	return &localUploader{}
}

func (u *localUploader) UploadFile(filepath, storagePath string, _ string) (string, int64, error) {
	stat, err := os.Stat(filepath)
	if err != nil {
		return "", 0, err
	}

	dir, _ := path.Split(storagePath)
	if err = os.MkdirAll(dir, 0755); err != nil {
		return "", 0, err
	}

	local, err := os.Open(filepath)
	if err != nil {
		return "", 0, err
	}
	defer local.Close()

	storage, err := os.Create(storagePath)
	if err != nil {
		return "", 0, err
	}
	defer storage.Close()

	_, err = io.Copy(storage, local)
	if err != nil {
		return "", 0, err
	}

	return storagePath, stat.Size(), nil
}

func (u *localUploader) UploadData(data []byte, storagePath, _ string) (string, int64, error) {
	dir, _ := path.Split(storagePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", 0, err
	}

	storage, err := os.Create(storagePath)
	if err != nil {
		return "", 0, err
	}
	defer storage.Close()

	n, err := storage.Write(data)
	if err != nil {
		return "", 0, err
	}

	return storagePath, int64(n), nil
}

func (u *localUploader) DownloadData(storagePath string) ([]byte, error) {
	return os.ReadFile(storagePath)
}

func (u *localUploader) DownloadFile(filepath, storagePath string) (int64, error) {
	_, size, err := u.UploadFile(storagePath, filepath, "")
	return size, err
}

func (u *localUploader) GeneratePresignedUrl(storagePath string) (string, error) {
	abs, err := filepath.Abs(storagePath)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("file://%s", abs), nil
}

func (u *localUploader) Delete(storagePath string) error {
	return os.Remove(storagePath)
}
