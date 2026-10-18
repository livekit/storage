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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

func wrapAliOSSError(err error) error {
	if err == nil {
		return nil
	}
	var svcErr oss.ServiceError
	if errors.As(err, &svcErr) {
		return &ErrorWithStatusCode{
			Err:        err,
			StatusCode: svcErr.StatusCode,
		}
	}
	var unexpectedErr oss.UnexpectedStatusCodeError
	if errors.As(err, &unexpectedErr) {
		return &ErrorWithStatusCode{
			Err:        err,
			StatusCode: unexpectedErr.Got(),
		}
	}
	return err
}

type aliOSSStorage struct {
	conf   *AliOSSConfig
	bucket *oss.Bucket
}

func NewAliOSS(conf *AliOSSConfig) (Storage, error) {
	client, err := oss.New(conf.Endpoint, conf.AccessKey, conf.Secret)
	if err != nil {
		return nil, err
	}

	bucket, err := client.Bucket(conf.Bucket)
	if err != nil {
		return nil, err
	}

	return &aliOSSStorage{
		conf:   conf,
		bucket: bucket,
	}, nil
}

func (s *aliOSSStorage) UploadFile(ctx context.Context, storagePath, filepath string, opts PutOptions) (string, int64, error) {
	info, err := os.Stat(filepath)
	if err != nil {
		return "", 0, err
	}
	if err = aliOSSRetry(ctx, func() error {
		return s.bucket.PutObjectFromFile(storagePath, filepath, aliOSSWriteOptions(ctx, opts)...)
	}); err != nil {
		return "", 0, wrapAliOSSObjectError(err)
	}
	return s.location(storagePath), info.Size(), nil
}

func (s *aliOSSStorage) location(storagePath string) string {
	endpoint := strings.TrimPrefix(s.conf.Endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	u := url.URL{Scheme: "https", Host: s.conf.Bucket + "." + endpoint, Path: storagePath}
	return u.String()
}

func (s *aliOSSStorage) DownloadFile(ctx context.Context, storagePath, filepath string) (int64, error) {
	return downloadFile(filepath, func(f *os.File) (int64, error) {
		return s.download(ctx, storagePath, f)
	})
}

func (s *aliOSSStorage) PresignedURL(_ context.Context, storagePath string, expiration time.Duration) (string, error) {
	u, err := s.bucket.SignURL(storagePath, oss.HTTPGet, int64(expiration.Seconds()))
	if err != nil {
		return "", wrapAliOSSError(err)
	}
	return u, nil
}

func (s *aliOSSStorage) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	return aliOSSRetry(ctx, func() error {
		return s.UploadObject(ctx, storagePath, bytes.NewReader(data), opts)
	})
}

// UploadObject streams body in one PutObject, chunked when its length is
// unknown, so it holds no part in memory and caps an object at 5 GB. It is
// sent once, as the body is read as it goes. IfAbsent sets
// x-oss-forbid-overwrite, atomic here, but OSS ignores it on a bucket with
// versioning enabled or suspended.
func (s *aliOSSStorage) UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	return wrapAliOSSObjectError(s.bucket.PutObject(storagePath, body, aliOSSWriteOptions(ctx, opts)...))
}

// aliOSSWriteOptions leaves out an empty content type, which the SDK then
// takes from the key's extension.
func aliOSSWriteOptions(ctx context.Context, opts PutOptions) []oss.Option {
	options := []oss.Option{oss.WithContext(ctx)}
	if opts.ContentType != "" {
		options = append(options, oss.ContentType(opts.ContentType))
	}
	if opts.IfAbsent {
		options = append(options, oss.ForbidOverWrite(true))
	}
	return options
}

func (s *aliOSSStorage) GetObject(ctx context.Context, storagePath string) ([]byte, error) {
	return getObject(ctx, s, storagePath)
}

// GetObjectRange asks for standard range behavior: by default OSS answers an
// unsatisfiable range with the whole object.
func (s *aliOSSStorage) GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("storage: invalid range offset %d length %d", offset, length)
	}
	var buf bytes.Buffer
	_, err := s.download(ctx, storagePath, &buf,
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

func (s *aliOSSStorage) DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error) {
	return s.download(ctx, storagePath, w)
}

// download copies the object to w and, for a whole object, checks the CRC64
// OSS sends against what arrived, as the SDK's GetObjectToFile does.
func (s *aliOSSStorage) download(ctx context.Context, storagePath string, w io.Writer, options ...oss.Option) (int64, error) {
	var result *oss.GetObjectResult
	err := aliOSSRetry(ctx, func() (err error) {
		result, err = s.bucket.DoGetObject(&oss.GetObjectRequest{ObjectKey: storagePath}, append(options, oss.WithContext(ctx)))
		return err
	})
	if err != nil {
		return 0, wrapAliOSSObjectError(err)
	}
	defer result.Response.Close()
	n, err := io.Copy(w, result.Response.Body)
	if err != nil {
		return n, wrapAliOSSObjectError(err)
	}
	if result.ClientCRC != nil {
		result.Response.ClientCRC = result.ClientCRC.Sum64()
		err = oss.CheckCRC(result.Response, "GetObject")
	}
	return n, err
}

func (s *aliOSSStorage) ObjectExists(ctx context.Context, storagePath string) (bool, error) {
	var exists bool
	err := aliOSSRetry(ctx, func() (err error) {
		exists, err = s.bucket.IsObjectExist(storagePath, oss.WithContext(ctx))
		return err
	})
	return exists, wrapAliOSSObjectError(err)
}

func (s *aliOSSStorage) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	options := []oss.Option{oss.Prefix(prefix), oss.MaxKeys(1000), oss.WithContext(ctx)}
	for {
		var res oss.ListObjectsResultV2
		err := aliOSSRetry(ctx, func() (err error) {
			res, err = s.bucket.ListObjectsV2(options...)
			return err
		})
		if err != nil {
			return nil, wrapAliOSSObjectError(err)
		}
		for _, obj := range res.Objects {
			keys = append(keys, obj.Key)
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			slices.Sort(keys)
			return keys, nil
		}
		options = []oss.Option{oss.Prefix(prefix), oss.MaxKeys(1000), oss.ContinuationToken(res.NextContinuationToken), oss.WithContext(ctx)}
	}
}

// DeleteObjects sends batches of 1000, the most one request takes. OSS reports
// no per-key errors, only the keys it deleted, so a key it leaves out fails the
// call unless it holds nothing.
func (s *aliOSSStorage) DeleteObjects(ctx context.Context, storagePaths []string) error {
	for i := 0; i < len(storagePaths); i += 1000 {
		batch := storagePaths[i:min(i+1000, len(storagePaths))]
		var res oss.DeleteObjectsResult
		err := aliOSSRetry(ctx, func() (err error) {
			res, err = s.bucket.DeleteObjects(batch, oss.WithContext(ctx))
			return err
		})
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
			exists, err := s.ObjectExists(ctx, k)
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

// aliOSSAttempts is how many times a call is sent: the v1 SDK never retries.
const aliOSSAttempts = 3

// aliOSSRetry runs call again after a 5xx, a 429 or a network error, with
// backoff, until it succeeds or aliOSSAttempts is spent. Only calls whose body,
// if any, is sent anew each time are retried.
func aliOSSRetry(ctx context.Context, call func() error) error {
	var err error
	for attempt := range aliOSSAttempts {
		if err = call(); err == nil || ctx.Err() != nil || !aliOSSRetryable(err) {
			return err
		}
		backoff := 200 * time.Millisecond << attempt
		select {
		case <-ctx.Done():
			return err
		case <-time.After(backoff/2 + rand.N(backoff)):
		}
	}
	return err
}

// aliOSSRetryable is a failure another attempt can get past: OSS throttling or
// a 5xx, or a connection that broke. A 4xx, as for a denied or bad request,
// would fail again.
func aliOSSRetryable(err error) bool {
	var svcErr oss.ServiceError
	if errors.As(err, &svcErr) {
		return svcErr.StatusCode >= 500 || svcErr.StatusCode == http.StatusTooManyRequests
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)
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
			return fmt.Errorf("%w: %w", ErrNotFound, wrapAliOSSError(err))
		case svcErr.Code == "FileAlreadyExists":
			return fmt.Errorf("%w: %w", ErrExists, wrapAliOSSError(err))
		}
	}
	return wrapAliOSSError(err)
}
