// Copyright 2024 LiveKit, Inc.
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
	"context"
	"errors"
	"time"
)

// Errors a backend maps its own to, so callers can decide with errors.Is
// whether an object was missing or a conditional upload lost. They come
// wrapped in the backend's error, which keeps the status code and message.
var (
	ErrNotFound     = errors.New("storage: object not found")
	ErrObjectExists = errors.New("storage: object already exists")
)

// ConditionalUploader is a Storage whose uploads can be made conditional on
// the object not existing. Two writers racing for one path see exactly one
// succeed; the other gets ErrObjectExists and changes nothing. S3 and local
// storage implement it.
type ConditionalUploader interface {
	UploadDataIfAbsent(ctx context.Context, data []byte, storagePath, contentType string) (location string, size int64, err error)
	UploadFileIfAbsent(ctx context.Context, filepath, storagePath, contentType string) (location string, size int64, err error)
}

// RangeDownloader is a Storage that can read part of an object: n bytes from
// offset off, fewer at the end of the object. An offset past the end, or a
// missing object, is ErrNotFound. S3 and local storage implement it.
type RangeDownloader interface {
	DownloadRange(ctx context.Context, storagePath string, off, n int64) ([]byte, error)
}

type Storage interface {
	UploadData(data []byte, storagePath, contentType string) (location string, size int64, err error)
	UploadFile(filepath, storagePath, contentType string) (location string, size int64, err error)

	ListObjects(prefix string) ([]string, error)

	DownloadData(storagePath string) (data []byte, err error)
	DownloadFile(filepath, storagePath string) (size int64, err error)

	GeneratePresignedUrl(storagePath string, expiration time.Duration) (url string, err error)

	DeleteObject(storagePath string) error
	DeleteObjects(storagePaths []string) error
}

func New(conf Config) (Storage, error) {
	return newStorage(conf)
}
