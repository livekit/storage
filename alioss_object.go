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

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

type aliOSSObjects struct {
	s *aliOSSStorage
}

func newAliOSSObjects(conf *AliOSSConfig) (ObjectStorage, error) {
	s, err := NewAliOSS(conf)
	if err != nil {
		return nil, err
	}
	return &aliOSSObjects{s: s.(*aliOSSStorage)}, nil
}

func (o *aliOSSObjects) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	return o.UploadObject(ctx, storagePath, bytes.NewReader(data), opts)
}

// UploadObject streams body in one PutObject, chunked when its length is
// unknown, so it holds no part in memory and caps an object at 5 GB. IfAbsent
// sets x-oss-forbid-overwrite, atomic here, but OSS ignores it on a bucket with
// versioning enabled or suspended.
func (o *aliOSSObjects) UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	options := []oss.Option{oss.WithContext(ctx)}
	if opts.ContentType != "" {
		options = append(options, oss.ContentType(opts.ContentType))
	}
	if opts.IfAbsent {
		options = append(options, oss.ForbidOverWrite(true))
	}
	return wrapAliOSSObjectError(o.s.bucket.PutObject(storagePath, body, options...))
}

func (o *aliOSSObjects) GetObject(ctx context.Context, storagePath string) ([]byte, error) {
	return getObject(ctx, o, storagePath)
}

// GetObjectRange asks for standard range behavior: by default OSS answers an
// unsatisfiable range with the whole object.
func (o *aliOSSObjects) GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("storage: invalid range offset %d length %d", offset, length)
	}
	var buf bytes.Buffer
	_, err := o.download(ctx, storagePath, &buf,
		oss.Range(offset, offset+length-1), oss.RangeBehavior("standard"))
	var svcErr oss.ServiceError
	if errors.As(err, &svcErr) && svcErr.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return []byte{}, nil
	}
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (o *aliOSSObjects) DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error) {
	return o.download(ctx, storagePath, w)
}

func (o *aliOSSObjects) download(ctx context.Context, storagePath string, w io.Writer, options ...oss.Option) (int64, error) {
	r, err := o.s.bucket.GetObject(storagePath, append(options, oss.WithContext(ctx))...)
	if err != nil {
		return 0, wrapAliOSSObjectError(err)
	}
	defer r.Close()
	n, err := io.Copy(w, r)
	return n, wrapAliOSSObjectError(err)
}

func (o *aliOSSObjects) ObjectExists(ctx context.Context, storagePath string) (bool, error) {
	exists, err := o.s.bucket.IsObjectExist(storagePath, oss.WithContext(ctx))
	return exists, wrapAliOSSObjectError(err)
}

func (o *aliOSSObjects) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	options := []oss.Option{oss.Prefix(prefix), oss.MaxKeys(1000), oss.WithContext(ctx)}
	for {
		res, err := o.s.bucket.ListObjectsV2(options...)
		if err != nil {
			return nil, wrapAliOSSObjectError(err)
		}
		for _, obj := range res.Objects {
			keys = append(keys, obj.Key)
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			return keys, nil
		}
		options = []oss.Option{oss.Prefix(prefix), oss.MaxKeys(1000), oss.ContinuationToken(res.NextContinuationToken), oss.WithContext(ctx)}
	}
}

// DeleteObjects sends batches of 1000, the most one request takes. OSS reports
// no per-key errors, only the keys it deleted, so a key it leaves out fails the
// call unless it holds nothing.
func (o *aliOSSObjects) DeleteObjects(ctx context.Context, storagePaths []string) error {
	for i := 0; i < len(storagePaths); i += 1000 {
		batch := storagePaths[i:min(i+1000, len(storagePaths))]
		res, err := o.s.bucket.DeleteObjects(batch, oss.WithContext(ctx))
		if err != nil {
			return wrapAliOSSObjectError(err)
		}
		deleted := make(map[string]bool, len(res.DeletedObjects))
		for _, k := range res.DeletedObjects {
			deleted[k] = true
		}
		for _, k := range batch {
			if deleted[k] {
				continue
			}
			exists, err := o.ObjectExists(ctx, k)
			if err != nil {
				return err
			}
			if exists {
				return fmt.Errorf("storage: delete of %s not confirmed", k)
			}
		}
	}
	return nil
}

func (o *aliOSSObjects) GeneratePresignedUrl(_ context.Context, storagePath string, expiration time.Duration) (string, error) {
	return o.s.GeneratePresignedUrl(storagePath, expiration)
}

func wrapAliOSSObjectError(err error) error {
	if err == nil {
		return nil
	}
	var svcErr oss.ServiceError
	if errors.As(err, &svcErr) {
		switch {
		// A HEAD answers a missing key with a bare 404.
		case svcErr.Code == "NoSuchKey" || (svcErr.StatusCode == http.StatusNotFound && svcErr.Code == ""):
			return fmt.Errorf("%w: %w", ErrObjectNotFound, wrapAliOSSError(err))
		case svcErr.Code == "FileAlreadyExists":
			return fmt.Errorf("%w: %w", ErrObjectExists, wrapAliOSSError(err))
		}
	}
	return wrapAliOSSError(err)
}
