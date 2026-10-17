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
	"net/http"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

type gcpObjects struct {
	s *gcpStorage
}

func NewGCPObjects(conf *GCPConfig) (ObjectStorage, error) {
	s, err := NewGCP(conf)
	if err != nil {
		return nil, err
	}
	return &gcpObjects{s: s.(*gcpStorage)}, nil
}

func (o *gcpObjects) object(storagePath string) *storage.ObjectHandle {
	return o.s.client.Bucket(o.s.conf.Bucket).Object(storagePath)
}

// PutObject sizes the upload buffer to data, so a small put sends one request
// without a full chunk's allocation.
func (o *gcpObjects) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	return o.write(ctx, storagePath, bytes.NewReader(data), opts, min(len(data)+1, googleapi.DefaultUploadChunkSize))
}

// UploadObject sends body in resumable chunks; IfAbsent is a precondition on
// the whole upload, so it is atomic here.
func (o *gcpObjects) UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	return o.write(ctx, storagePath, body, opts, googleapi.DefaultUploadChunkSize)
}

func (o *gcpObjects) write(ctx context.Context, storagePath string, body io.Reader, opts PutOptions, chunkSize int) error {
	obj := o.object(storagePath)
	if opts.IfAbsent {
		// Conditional, so the SDK retries it by default.
		obj = obj.If(storage.Conditions{DoesNotExist: true})
	} else {
		obj = obj.Retryer(storage.WithPolicy(storage.RetryAlways))
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := obj.NewWriter(ctx)
	w.ContentType = opts.ContentType
	w.ChunkSize = chunkSize
	if _, err := io.Copy(w, body); err != nil {
		// Cancelling aborts the upload; Close would store what was sent.
		cancel()
		return o.writeError(err, opts.IfAbsent)
	}
	return o.writeError(w.Close(), opts.IfAbsent)
}

// writeError maps a failed precondition to ErrObjectExists only where IfAbsent
// set one: GCS answers other refusals with 412 too.
func (o *gcpObjects) writeError(err error, ifAbsent bool) error {
	var apiErr *googleapi.Error
	if ifAbsent && errors.As(err, &apiErr) && apiErr.Code == http.StatusPreconditionFailed {
		return fmt.Errorf("%w: %w", ErrObjectExists, wrapGCPError(err))
	}
	return wrapGCPObjectError(err)
}

func (o *gcpObjects) GetObject(ctx context.Context, storagePath string) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := o.DownloadObject(ctx, storagePath, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (o *gcpObjects) GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("storage: invalid range offset %d length %d", offset, length)
	}
	r, err := o.object(storagePath).NewRangeReader(ctx, offset, length)
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusRequestedRangeNotSatisfiable {
		return []byte{}, nil
	}
	if err != nil {
		return nil, wrapGCPObjectError(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	return data, wrapGCPObjectError(err)
}

func (o *gcpObjects) DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error) {
	r, err := o.object(storagePath).NewReader(ctx)
	if err != nil {
		return 0, wrapGCPObjectError(err)
	}
	defer r.Close()
	n, err := io.Copy(w, r)
	return n, wrapGCPObjectError(err)
}

func (o *gcpObjects) ObjectExists(ctx context.Context, storagePath string) (bool, error) {
	_, err := o.object(storagePath).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return false, nil
	}
	return err == nil, wrapGCPObjectError(err)
}

func (o *gcpObjects) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	it := o.s.client.Bucket(o.s.conf.Bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	var keys []string
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return keys, nil
		}
		if err != nil {
			return nil, wrapGCPObjectError(err)
		}
		keys = append(keys, attrs.Name)
	}
}

func (o *gcpObjects) DeleteObjects(ctx context.Context, storagePaths []string) error {
	for _, p := range storagePaths {
		if err := o.object(p).Delete(ctx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
			return wrapGCPObjectError(err)
		}
	}
	return nil
}

func (o *gcpObjects) GeneratePresignedUrl(_ context.Context, storagePath string, expiration time.Duration) (string, error) {
	return o.s.GeneratePresignedUrl(storagePath, expiration)
}

func wrapGCPObjectError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("%w: %w", ErrObjectNotFound, wrapGCPError(err))
	}
	return wrapGCPError(err)
}
