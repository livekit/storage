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

// Package objects is the storage interfaces without any provider SDK, for code
// that implements or accepts them without creating one, such as a fake: the
// root package imports every provider's SDK. config does the same for the
// config structs.
package objects

import (
	"context"
	"errors"
	"io"
	"time"
)

// Bucket is object storage for a service that keeps and manages its own
// objects. A read of a missing object reports ErrNotFound and a refused write
// ErrExists; test them with errors.Is.
type Bucket interface {
	// PutObject sends data in one request, which the provider's SDK can retry.
	PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error
	// UploadObject streams body in parts and does not rewind it, so a retry
	// needs a new reader. Memory is the part size times the upload concurrency:
	// 16 MiB x 5 on S3, 4 MiB x 16 on Azure, one 16 MiB part on GCS and OCI,
	// none on AliOSS (one chunked request, up to 5 GB) and local.
	UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error

	GetObject(ctx context.Context, storagePath string) ([]byte, error)
	// GetObjectRange reads length bytes from offset, with offset >= 0 and
	// length > 0. A range running past the end returns what the object holds,
	// one starting at or past it no bytes.
	GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error)
	// DownloadObject streams the object to w, which holds part of it when an
	// error comes back.
	DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error)
	// ObjectExists reports a missing object as false, not ErrNotFound.
	ObjectExists(ctx context.Context, storagePath string) (bool, error)

	// ListObjects returns every key under prefix, recursively, as full paths,
	// in byte order.
	ListObjects(ctx context.Context, prefix string) ([]string, error)
	// DeleteObjects succeeds for paths that hold nothing and fails when a path
	// is not deleted; some may be deleted by then, and a retry is safe. GCS,
	// Azure and OCI send one request per path.
	DeleteObjects(ctx context.Context, storagePaths []string) error

	// GeneratePresignedUrl is a GET URL; local returns a file:// URL.
	GeneratePresignedUrl(ctx context.Context, storagePath string, expiration time.Duration) (string, error)
}

// Storage is Bucket and the calls that publish and fetch whole files.
type Storage interface {
	Bucket

	// UploadData and UploadFile replace any object at storagePath and return
	// its location, a URL to it.
	UploadData(ctx context.Context, data []byte, storagePath, contentType string) (location string, size int64, err error)
	UploadFile(ctx context.Context, filepath, storagePath, contentType string) (location string, size int64, err error)
	DownloadFile(ctx context.Context, filepath, storagePath string) (size int64, err error)
}

type PutOptions struct {
	ContentType string
	// IfAbsent fails with ErrExists when the path holds an object, atomically
	// where the store honors the condition: an S3-compatible store that ignores
	// If-None-Match overwrites, and AliOSS ignores it on a versioned bucket. A
	// retry after a lost response can see the caller's own write as existing.
	IfAbsent bool
}

var (
	ErrNotFound = errors.New("storage: object not found")
	ErrExists   = errors.New("storage: object exists")
)
