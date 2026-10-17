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

// Package objects is the storage interfaces and errors without any provider
// SDK, for code that implements or accepts them without creating one, such as
// a fake: the root package imports every provider's SDK.
package objects

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Bucket is object storage for a service that keeps and manages its own
// objects. A read of a missing object reports ErrNotFound and a write IfAbsent
// refuses ErrExists; test them with errors.Is. Where a store differs, every
// backend follows S3.
//
// A key is a store's key as given. The local directory is the exception, as a
// filesystem: it drops a leading "/", cleans "//" and "." segments, refuses a
// key that leads out of it, cannot hold both "a" and "a/b", and keeps its
// temporary files under names starting with ".upload-", which it never lists.
type Bucket interface {
	// PutObject sends data, which is retried as a whole where a call fails.
	PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error
	// UploadObject streams body in parts and does not rewind it, so a retry
	// needs a new reader. Memory is about the part size times the parts in
	// flight: 16 MiB x 6 on S3, 4 MiB x 16 on Azure, one 16 MiB part on GCS and
	// OCI, none on AliOSS (one chunked request, up to 5 GB, sent once) and local.
	UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error

	GetObject(ctx context.Context, storagePath string) ([]byte, error)
	// GetObjectRange reads length bytes from offset, with offset >= 0 and
	// length > 0. A range running past the end returns what the object holds,
	// one starting at or past it no bytes.
	GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error)
	// DownloadObject streams the object to w, which holds part of it when an
	// error comes back. An object replaced during a read fails it, with no
	// sentinel.
	DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error)
	// ObjectExists reports a missing object as false, not ErrNotFound.
	ObjectExists(ctx context.Context, storagePath string) (bool, error)

	// ListObjects returns every key under prefix, recursively, as full paths,
	// in byte order.
	ListObjects(ctx context.Context, prefix string) ([]string, error)
	// DeleteObjects succeeds for paths that hold nothing, as S3's delete does,
	// and fails when a path is not deleted; some may be deleted by then, and a
	// retry is safe. GCS, Azure and OCI send one request per path, 16 at once.
	DeleteObjects(ctx context.Context, storagePaths []string) error

	// PresignedURL is a GET URL; local returns a file:// URL.
	PresignedURL(ctx context.Context, storagePath string, expiration time.Duration) (string, error)
}

// Storage is Bucket and the calls that publish and fetch whole files.
type Storage interface {
	Bucket

	// UploadFile writes the file at storagePath and returns its location: a
	// URL, or the file's path on local.
	UploadFile(ctx context.Context, storagePath, filepath string, opts PutOptions) (location string, size int64, err error)
	// DownloadFile writes the object to filepath, making its directory, only
	// once the whole object is read: a failed download leaves filepath as it
	// was.
	DownloadFile(ctx context.Context, storagePath, filepath string) (size int64, err error)
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

// ErrorWithStatusCode carries the HTTP status a store answered with, under
// ErrNotFound or ErrExists where those apply; find it with errors.As.
type ErrorWithStatusCode struct {
	Err        error
	StatusCode int
}

func (e *ErrorWithStatusCode) Error() string {
	return fmt.Sprintf("err: %s, status code: %d", e.Err, e.StatusCode)
}

func (e *ErrorWithStatusCode) Unwrap() error {
	return e.Err
}
