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
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/googleapis/gax-go/v2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"
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
	// signer is CredentialsJSON's service account, which signs URLs: with the
	// credentials passed as a token source, the SDK finds no signer of its own
	// off GCE.
	signer *jwt.Config
}

func NewGCP(conf *GCPConfig) (Storage, error) {
	u := &gcpStorage{
		conf: conf,
	}

	// An http:// endpoint is an emulator, which takes no credentials; any other
	// endpoint, such as Private Service Connect, keeps them.
	emulator := strings.HasPrefix(conf.Endpoint, "http://")
	var opts []option.ClientOption
	if conf.CredentialsJSON != "" {
		jwtConfig, err := google.JWTConfigFromJSON([]byte(conf.CredentialsJSON), storageScope)
		if err != nil {
			return nil, err
		}
		u.signer = jwtConfig
		if !emulator {
			opts = append(opts, option.WithTokenSource(jwtConfig.TokenSource(context.Background())))
		}
	}
	if conf.Endpoint != "" {
		opts = append(opts, option.WithEndpoint(strings.TrimSuffix(conf.Endpoint, "/")+"/storage/v1/"))
	}
	if emulator {
		opts = append(opts, option.WithoutAuthentication())
	}

	setProxy, err := proxySetter(conf.ProxyConfig)
	if err != nil {
		return nil, err
	}
	client, err := newGCSClient(opts, setProxy)
	if err != nil {
		return nil, err
	}

	u.client = client
	return u, nil
}

func (s *gcpStorage) UploadFile(ctx context.Context, filepath, storagePath, contentType string) (string, int64, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	if err = s.UploadObject(ctx, storagePath, file, PutOptions{ContentType: contentType}); err != nil {
		return "", 0, err
	}
	return s.location(storagePath), stat.Size(), nil
}

func (s *gcpStorage) DownloadFile(ctx context.Context, filepath, storagePath string) (int64, error) {
	file, err := os.Create(filepath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	return s.DownloadObject(ctx, storagePath, file)
}

func (s *gcpStorage) location(storagePath string) string {
	return (&url.URL{
		Scheme: "https",
		Host:   s.conf.Bucket + ".storage.googleapis.com",
		Path:   storagePath,
	}).String()
}

func (s *gcpStorage) presignedURL(storagePath string, expiration time.Duration) (string, error) {
	opts := &storage.SignedURLOptions{
		Method:  "GET",
		Expires: time.Now().Add(expiration),
	}
	// A V2 URL, which has no limit on its expiry, is always https; an http://
	// emulator takes V4, which signs for at most 7 days.
	if strings.HasPrefix(s.conf.Endpoint, "http://") {
		opts.Scheme = storage.SigningSchemeV4
		opts.Insecure = true
	}
	if s.signer != nil {
		opts.GoogleAccessID = s.signer.Email
		opts.PrivateKey = s.signer.PrivateKey
	}
	u, err := s.client.Bucket(s.conf.Bucket).SignedURL(storagePath, opts)
	if err != nil {
		return "", wrapGCPError(err)
	}
	return u, nil
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
		obj = obj.Retryer(
			storage.WithBackoff(gax.Backoff{Initial: 100 * time.Millisecond, Max: 5 * time.Second, Multiplier: 2}),
			storage.WithMaxAttempts(5),
			storage.WithPolicy(storage.RetryAlways),
		)
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

// writeError maps a failed precondition to ErrExists only where IfAbsent
// set one: GCS answers other refusals with 412 too.
func (s *gcpStorage) writeError(err error, ifAbsent bool) error {
	var apiErr *googleapi.Error
	if ifAbsent && errors.As(err, &apiErr) && apiErr.Code == http.StatusPreconditionFailed {
		return fmt.Errorf("%w: %w", ErrExists, wrapGCPError(err))
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
	return deleteEach(ctx, storagePaths, func(ctx context.Context, p string) error {
		if err := s.object(p).Delete(ctx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
			return wrapGCPObjectError(err)
		}
		return nil
	})
}

func (s *gcpStorage) PresignedURL(_ context.Context, storagePath string, expiration time.Duration) (string, error) {
	return s.presignedURL(storagePath, expiration)
}

func wrapGCPObjectError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("%w: %w", ErrNotFound, wrapGCPError(err))
	}
	return wrapGCPError(err)
}

// gcsTransportMu serializes newGCSClient's swap of http.DefaultTransport.
var gcsTransportMu sync.Mutex

// newGCSClient builds a client on a transport with this package's timeouts and
// the proxy. The SDK copies http.DefaultTransport when it builds a client, and
// passing a client of our own would mean wiring its auth by hand, so the
// transport stands in as the default only while the client is built.
func newGCSClient(opts []option.ClientOption, setProxy func(*http.Transport)) (*storage.Client, error) {
	gcsTransportMu.Lock()
	defer gcsTransportMu.Unlock()
	orig := http.DefaultTransport
	t := transportWithTimeouts(orig)
	setProxy(t)
	http.DefaultTransport = t
	defer func() { http.DefaultTransport = orig }()
	return storage.NewClient(context.Background(), opts...)
}
