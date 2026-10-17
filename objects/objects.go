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

// Package objects is the Storage interface without any provider SDK, for code
// that implements or accepts it without creating one, the way config holds the
// config structs.
package objects

import (
	"context"
	"errors"
	"io"
	"time"
)

type Storage interface {
	// UploadData and UploadFile replace any object at storagePath and return
	// its location.
	UploadData(data []byte, storagePath, contentType string) (location string, size int64, err error)
	UploadFile(filepath, storagePath, contentType string) (location string, size int64, err error)

	DownloadData(storagePath string) (data []byte, err error)
	DownloadFile(filepath, storagePath string) (size int64, err error)
	DeleteObject(storagePath string) error

	// The calls below take a context, and report a missing or existing object as
	// ErrNotFound or ErrExists.

	PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error
	// UploadObject streams body in parts: memory is bounded by the part size
	// times the upload concurrency, whatever the object's size.
	UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error

	GetObject(ctx context.Context, storagePath string) ([]byte, error)
	// GetObjectRange reads length bytes from offset. A range running past the
	// end returns what the object holds, one starting at or past it no bytes.
	GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error)
	DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error)
	ObjectExists(ctx context.Context, storagePath string) (bool, error)

	ListObjects(ctx context.Context, prefix string) ([]string, error)
	// DeleteObjects succeeds for paths that hold nothing and fails when a path
	// is not deleted.
	DeleteObjects(ctx context.Context, storagePaths []string) error

	GeneratePresignedUrl(ctx context.Context, storagePath string, expiration time.Duration) (string, error)
}

type PutOptions struct {
	ContentType string
	// IfAbsent fails with ErrExists when the path holds an object, atomically:
	// of concurrent writes to one path, one succeeds.
	IfAbsent bool
}

var (
	ErrNotFound = errors.New("storage: object not found")
	ErrExists   = errors.New("storage: object exists")
)
