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
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"cloud.google.com/go/storage"
	"github.com/googleapis/gax-go/v2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

const storageScope = "https://www.googleapis.com/auth/devstorage.read_write"

func wrapGCPError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrBucketNotExist) || errors.Is(err, storage.ErrObjectNotExist) {
		return &ErrorWithStatusCode{
			Err:        err,
			StatusCode: http.StatusNotFound,
		}
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return &ErrorWithStatusCode{
			Err:        err,
			StatusCode: apiErr.Code,
		}
	}
	return err
}

type gcpStorage struct {
	conf   *GCPConfig
	client *storage.Client
}

func NewGCP(conf *GCPConfig) (Storage, error) {
	u := &gcpStorage{
		conf: conf,
	}

	var opts []option.ClientOption
	if conf.CredentialsJSON != "" {
		jwtConfig, err := google.JWTConfigFromJSON([]byte(conf.CredentialsJSON), storageScope)
		if err != nil {
			return nil, err
		}
		opts = append(opts, option.WithTokenSource(jwtConfig.TokenSource(context.Background())))
	}

	defaultTransport := http.DefaultTransport.(*http.Transport)
	transportClone := defaultTransport.Clone()

	if conf.ProxyConfig != nil {
		proxyUrl, err := url.Parse(conf.ProxyConfig.Url)
		if err != nil {
			return nil, err
		}
		defaultTransport.Proxy = http.ProxyURL(proxyUrl)
		if conf.ProxyConfig.Username != "" && conf.ProxyConfig.Password != "" {
			auth := fmt.Sprintf("%s:%s", conf.ProxyConfig.Username, conf.ProxyConfig.Password)
			basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(auth))
			defaultTransport.ProxyConnectHeader = http.Header{}
			defaultTransport.ProxyConnectHeader.Add("Proxy-Authorization", basicAuth)
		}
	}
	client, err := storage.NewClient(context.Background(), opts...)

	// restore default transport
	http.DefaultTransport = transportClone
	if err != nil {
		return nil, err
	}

	u.client = client
	return u, nil
}

func (s *gcpStorage) UploadData(data []byte, storagePath, contentType string) (string, int64, error) {
	return s.upload(bytes.NewReader(data), storagePath, contentType)
}

func (s *gcpStorage) UploadFile(filepath, storagePath, contentType string) (string, int64, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()

	return s.upload(file, storagePath, contentType)
}

func (s *gcpStorage) upload(reader io.Reader, storagePath, contentType string) (string, int64, error) {
	wc := s.client.Bucket(s.conf.Bucket).Object(storagePath).Retryer(
		storage.WithBackoff(gax.Backoff{
			Initial:    time.Millisecond * 100,
			Max:        time.Second * 5,
			Multiplier: 2,
		}),
		storage.WithMaxAttempts(5),
		storage.WithPolicy(storage.RetryAlways),
	).NewWriter(context.Background())
	wc.ContentType = contentType
	wc.ChunkRetryDeadline = 0

	n, err := io.Copy(wc, reader)
	if err != nil {
		return "", 0, wrapGCPError(err)
	}

	if err = wc.Close(); err != nil {
		return "", 0, wrapGCPError(err)
	}

	return s.location(storagePath), n, nil
}

func (s *gcpStorage) location(storagePath string) string {
	return (&url.URL{
		Scheme: "https",
		Host:   s.conf.Bucket + ".storage.googleapis.com",
		Path:   storagePath,
	}).String()
}

func (s *gcpStorage) DownloadData(storagePath string) ([]byte, error) {
	rc, err := s.download(storagePath)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, wrapGCPError(err)
	}
	return data, nil
}

func (s *gcpStorage) DownloadFile(filepath, storagePath string) (int64, error) {
	file, err := os.Create(filepath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	rc, err := s.download(storagePath)
	if err != nil {
		return 0, err
	}

	_, err = io.Copy(file, rc)
	_ = rc.Close()
	if err != nil {
		return 0, wrapGCPError(err)
	}

	return rc.Attrs.Size, nil
}

func (s *gcpStorage) download(storagePath string) (*storage.Reader, error) {
	r, err := s.client.Bucket(s.conf.Bucket).Object(storagePath).Retryer(
		storage.WithBackoff(
			gax.Backoff{
				Initial:    time.Millisecond * 100,
				Max:        time.Second * 5,
				Multiplier: 2,
			}),
		storage.WithPolicy(storage.RetryAlways),
	).NewReader(context.Background())
	if err != nil {
		return nil, wrapGCPObjectError(err)
	}
	return r, nil
}

func (s *gcpStorage) presignedUrl(storagePath string, expiration time.Duration) (string, error) {
	u, err := s.client.Bucket(s.conf.Bucket).SignedURL(storagePath, &storage.SignedURLOptions{
		Method:  "GET",
		Expires: time.Now().Add(expiration),
	})
	if err != nil {
		return "", wrapGCPError(err)
	}
	return u, nil
}

func (s *gcpStorage) DeleteObject(storagePath string) error {
	return wrapGCPError(s.client.Bucket(s.conf.Bucket).Object(storagePath).Delete(context.Background()))
}

func (s *gcpStorage) object(storagePath string) *storage.ObjectHandle {
	return s.client.Bucket(s.conf.Bucket).Object(storagePath)
}

// PutObject sizes the upload buffer to data, so a small put sends one request
// without a full chunk's allocation.
func (s *gcpStorage) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	return s.write(ctx, storagePath, bytes.NewReader(data), opts, min(len(data)+1, googleapi.DefaultUploadChunkSize))
}

// UploadObject sends body in resumable chunks; IfAbsent is a precondition on
// the whole upload, so it is atomic here.
func (s *gcpStorage) UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	return s.write(ctx, storagePath, body, opts, googleapi.DefaultUploadChunkSize)
}

func (s *gcpStorage) write(ctx context.Context, storagePath string, body io.Reader, opts PutOptions, chunkSize int) error {
	obj := s.object(storagePath)
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
		return s.writeError(err, opts.IfAbsent)
	}
	return s.writeError(w.Close(), opts.IfAbsent)
}

// writeError maps a failed precondition to ErrObjectExists only where IfAbsent
// set one: GCS answers other refusals with 412 too.
func (s *gcpStorage) writeError(err error, ifAbsent bool) error {
	var apiErr *googleapi.Error
	if ifAbsent && errors.As(err, &apiErr) && apiErr.Code == http.StatusPreconditionFailed {
		return fmt.Errorf("%w: %w", ErrObjectExists, wrapGCPError(err))
	}
	return wrapGCPObjectError(err)
}

func (s *gcpStorage) GetObject(ctx context.Context, storagePath string) ([]byte, error) {
	return getObject(ctx, s, storagePath)
}

func (s *gcpStorage) GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("storage: invalid range offset %d length %d", offset, length)
	}
	r, err := s.object(storagePath).NewRangeReader(ctx, offset, length)
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

func (s *gcpStorage) DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error) {
	r, err := s.object(storagePath).NewReader(ctx)
	if err != nil {
		return 0, wrapGCPObjectError(err)
	}
	defer r.Close()
	n, err := io.Copy(w, r)
	return n, wrapGCPObjectError(err)
}

func (s *gcpStorage) ObjectExists(ctx context.Context, storagePath string) (bool, error) {
	_, err := s.object(storagePath).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return false, nil
	}
	return err == nil, wrapGCPObjectError(err)
}

func (s *gcpStorage) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	it := s.client.Bucket(s.conf.Bucket).Objects(ctx, &storage.Query{Prefix: prefix})
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

func (s *gcpStorage) DeleteObjects(ctx context.Context, storagePaths []string) error {
	for _, p := range storagePaths {
		if err := s.object(p).Delete(ctx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
			return wrapGCPObjectError(err)
		}
	}
	return nil
}

func (s *gcpStorage) GeneratePresignedUrl(_ context.Context, storagePath string, expiration time.Duration) (string, error) {
	return s.presignedUrl(storagePath, expiration)
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
