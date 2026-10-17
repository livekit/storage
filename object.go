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
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// ObjectStorage is Storage for services that keep their own objects: every
// call takes a context, writes can refuse to replace an object, and reads can
// take a range or stream to a writer.
type ObjectStorage interface {
	PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error
	// UploadObject streams body in parts, holding about one part in memory.
	UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error

	GetObject(ctx context.Context, storagePath string) ([]byte, error)
	// GetObjectRange reads length bytes from offset; past the end it returns
	// what the object holds.
	GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error)
	DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error)
	ObjectExists(ctx context.Context, storagePath string) (bool, error)

	ListObjects(ctx context.Context, prefix string) ([]string, error)
	DeleteObjects(ctx context.Context, storagePaths []string) error

	GeneratePresignedUrl(ctx context.Context, storagePath string, expiration time.Duration) (string, error)
}

type PutOptions struct {
	ContentType string
	// IfAbsent fails with ErrObjectExists when the path holds an object.
	// PutObject checks it atomically. UploadObject checks it before uploading,
	// so two concurrent uploads to one path can both succeed.
	IfAbsent bool
}

var (
	ErrObjectNotFound = errors.New("storage: object not found")
	ErrObjectExists   = errors.New("storage: object exists")
)

// NewObjectStorage supports S3 (and S3-compatible endpoints) and local storage.
func NewObjectStorage(conf Config) (ObjectStorage, error) {
	switch c := conf.(type) {
	case *S3Config:
		return NewS3Objects(c)
	case *LocalConfig:
		return NewLocalObjects(c)
	default:
		return nil, fmt.Errorf("object storage is not supported for %T", conf)
	}
}
