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
	"crypto/rsa"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
	"golang.org/x/sync/singleflight"
)

type ociStorage struct {
	conf     *OCIConfig
	provider common.ConfigurationProvider

	// connected is set once the fields below are, by connect, which
	// connecting runs one at a time.
	connected  atomic.Bool
	connecting singleflight.Group
	built      bool
	client     objectstorage.ObjectStorageClient
	// grantRetry is the policy of a call that leaves a grant behind.
	grantRetry common.RetryPolicy
	namespace  string
	host       string
	// insecure is an http:// Endpoint, such as an emulator's.
	insecure bool
}

// NewOCI makes no request. With workload identity the SDK fetches a token as
// its client is built, so the first call builds it. A namespace the config
// leaves out is looked up by the first call as well.
func NewOCI(conf *OCIConfig) (Storage, error) {
	provider, err := newConfigProvider(conf)
	if err != nil {
		return nil, err
	}

	s := &ociStorage{conf: conf, provider: provider, namespace: conf.Namespace}
	if !conf.UseWorkloadIdentity {
		if err = s.build(); err != nil {
			return nil, err
		}
		s.connected.Store(s.namespace != "")
	}
	return s, nil
}

// ready has the client built and the namespace known. The calls waiting on
// that share one connect, and a failure is not kept, so a later call starts
// another. A call whose ctx ends is released and leaves nothing running of its
// own: the connect goes on for the calls still waiting, and for the next.
func (s *ociStorage) ready(ctx context.Context) error {
	if s.connected.Load() {
		return nil
	}
	done := s.connecting.DoChan("", func() (any, error) { return nil, s.connect() })
	select {
	case r := <-done:
		return r.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *ociStorage) connect() error {
	if s.connected.Load() {
		return nil
	}

	if !s.built {
		if err := s.build(); err != nil {
			return err
		}
	}
	if s.namespace == "" {
		req := objectstorage.GetNamespaceRequest{}
		if s.conf.CompartmentID != "" {
			req.CompartmentId = &s.conf.CompartmentID
		}
		namespace, err := lookupNamespace(s.client, req)
		if err != nil {
			return err
		}
		s.namespace = namespace
	}
	s.connected.Store(true)
	return nil
}

// build makes the SDK client, which with workload identity fetches a token.
func (s *ociStorage) build() error {
	conf := s.conf
	client, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(s.provider)
	if err != nil {
		return err
	}

	if conf.Region != "" {
		client.SetRegion(conf.Region)
	}
	if conf.Endpoint != "" {
		client.Host = conf.Endpoint
	}
	// The SDK's 60s http.Client.Timeout covers reading the body and would cut a
	// long transfer; the idle timeout catches a dead connection instead.
	// Its transport wrapper stays, as it loads OCI_DEFAULT_CERTS_PATH and
	// refreshes the TLS config.
	if hc, ok := client.HTTPClient.(*http.Client); ok {
		if w, ok := hc.Transport.(*common.OciHTTPTransportWrapper); ok {
			w.TransportTemplate = func(tlsConfig *tls.Config) (http.RoundTripper, error) {
				t, err := common.DefaultTransport(tlsConfig)
				if err != nil {
					return nil, err
				}
				return transportWithTimeouts(t), nil
			}
		}
		c := &http.Client{Transport: hc.Transport, CheckRedirect: hc.CheckRedirect, Jar: hc.Jar}
		if os.Getenv(common.CustomClientTimeoutEnvVar) != "" {
			c.Timeout = hc.Timeout
		}
		client.HTTPClient = c
	}
	// The SDK has set the policy the environment or common.GlobalRetry asks for.
	configured := client.Configuration.RetryPolicy
	policy := ociRetryPolicy(configured, ociAlsoClosed)
	client.Configuration.RetryPolicy = &policy

	// client.Host keeps endpoint options such as {dualStack?ds.oci.:}, which the
	// SDK resolves on a copy per request, so resolve them the same way here.
	resolved := client.BaseClient
	common.UpdateEndpointTemplateForOptions(&resolved)
	host := strings.TrimPrefix(resolved.Host, "https://")
	insecure := strings.HasPrefix(host, "http://")
	host = strings.TrimPrefix(host, "http://")

	s.client, s.host, s.insecure, s.built = client, host, insecure, true
	s.grantRetry = ociRetryPolicy(configured, ociOnlyAnswered)
	return nil
}

// lookupNamespace is the tenancy's namespace, or an error after a minute, as
// with the SDK's own client timeout. A canceled OCI call races in the SDK, so
// the lookup is not canceled but left to finish.
func lookupNamespace(client objectstorage.ObjectStorageClient, req objectstorage.GetNamespaceRequest) (string, error) {
	type result struct {
		namespace string
		err       error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.GetNamespace(context.Background(), req)
		if err != nil {
			done <- result{err: wrapOCIError(err)}
			return
		}
		done <- result{namespace: *resp.Value}
	}()
	select {
	case r := <-done:
		return r.namespace, r.err
	case <-time.After(time.Minute):
		return "", errors.New("storage: OCI namespace lookup took over a minute")
	}
}

type ociPredicate = func(common.OCIOperationResponse) bool

// ociRetryPolicy is the policy in effect, configured or else the SDK's
// default, with its predicate changed by change: its attempts and backoff stay.
// The SDK runs the inner policy outside an eventual-consistency window, so
// the predicates of both change.
func ociRetryPolicy(configured *common.RetryPolicy, change func(ociPredicate) ociPredicate) common.RetryPolicy {
	policy := common.DefaultRetryPolicy()
	if configured != nil {
		policy = *configured
	}
	policy.ShouldRetryOperation = change(policy.ShouldRetryOperation)
	if policy.NonEventuallyConsistentPolicy != nil {
		inner := *policy.NonEventuallyConsistentPolicy
		inner.ShouldRetryOperation = change(inner.ShouldRetryOperation)
		policy.NonEventuallyConsistentPolicy = &inner
	}
	return policy
}

// ociAlsoClosed adds to retry a connection the server closed under the
// request, which the SDK matches only when nothing wraps the error. Such a
// request may have been received, as with a timeout the SDK retries.
func ociAlsoClosed(retry ociPredicate) ociPredicate {
	return func(r common.OCIOperationResponse) bool {
		if retry(r) {
			return true
		}
		err := r.Error
		if err == nil {
			return false
		}
		return errors.Is(err, io.EOF) ||
			errors.Is(err, io.ErrUnexpectedEOF) ||
			strings.Contains(err.Error(), "http: server closed idle connection")
	}
}

// ociOnlyAnswered keeps of retry the failures the server answered with. A
// request that got no answer may have been carried out, and one sent again
// leaves a second grant, the first with a URI nobody was given.
func ociOnlyAnswered(retry ociPredicate) ociPredicate {
	return func(r common.OCIOperationResponse) bool {
		_, answered := common.IsServiceError(r.Error)
		return answered && retry(r)
	}
}

const (
	ociTokenAttempts = 5
	// A fetch that fails this long after the first began has timed out, and
	// is not repeated.
	ociTokenRetryWindow = 15 * time.Second
)

// ociTokenBackoff is the wait before the first retry of a token fetch. Each
// later wait doubles it, and each is jittered.
var ociTokenBackoff = 250 * time.Millisecond

// ociTokenProvider retries the workload identity token fetch, which the SDK
// makes once, inside whichever of these calls finds no valid token.
type ociTokenProvider struct {
	auth.ConfigurationProviderWithClaimAccess
}

func (p ociTokenProvider) TenancyOCID() (string, error) {
	return retryOCIToken(p.ConfigurationProviderWithClaimAccess.TenancyOCID)
}

func (p ociTokenProvider) KeyID() (string, error) {
	return retryOCIToken(p.ConfigurationProviderWithClaimAccess.KeyID)
}

func (p ociTokenProvider) PrivateRSAKey() (*rsa.PrivateKey, error) {
	return retryOCIToken(p.ConfigurationProviderWithClaimAccess.PrivateRSAKey)
}

func (p ociTokenProvider) GetClaim(key string) (interface{}, error) {
	return retryOCIToken(func() (interface{}, error) {
		return p.ConfigurationProviderWithClaimAccess.GetClaim(key)
	})
}

// Refreshable keeps the SDK's retry of a 401 on a token it can renew.
func (p ociTokenProvider) Refreshable() bool {
	r, ok := p.ConfigurationProviderWithClaimAccess.(common.RefreshableConfigurationProvider)
	return ok && r.Refreshable()
}

// The SDK asks for a token with no context, so the fetch of a call that has
// ended runs on, for under 4s.
func retryOCIToken[T any](fetch func() (T, error)) (T, error) {
	start := time.Now()
	backoff := ociTokenBackoff
	for attempt := 1; ; attempt++ {
		v, err := fetch()
		if err == nil || attempt == ociTokenAttempts || !ociTokenRetryable(err) || time.Since(start) > ociTokenRetryWindow {
			return v, err
		}
		time.Sleep(backoff/2 + rand.N(backoff/2+1))
		backoff *= 2
	}
}

// ociTokenRetryable is any failure but the 403 of a cluster without workload
// identity. The SDK flattens the response's status into the error's text.
func ociTokenRetryable(err error) bool {
	return !strings.Contains(err.Error(), "Status: 403")
}

func newConfigProvider(conf *OCIConfig) (common.ConfigurationProvider, error) {
	if conf.UseWorkloadIdentity {
		provider, err := auth.OkeWorkloadIdentityConfigurationProvider()
		if err != nil {
			return nil, err
		}
		return ociTokenProvider{provider}, nil
	}

	if conf.TenancyOCID != "" && conf.UserOCID != "" && conf.PrivateKey != "" {
		var passphrase *string
		if conf.PrivateKeyPassphrase != "" {
			passphrase = &conf.PrivateKeyPassphrase
		}
		return common.NewRawConfigurationProvider(
			conf.TenancyOCID,
			conf.UserOCID,
			conf.Region,
			conf.Fingerprint,
			conf.PrivateKey,
			passphrase,
		), nil
	}

	return common.DefaultConfigProvider(), nil
}

// UploadFile sends the file in one PutObject, up to OCI's 50 GiB, which the SDK
// can retry by seeking it, as on main: a multipart upload, which OCI advises
// past 100 MiB, needs more permissions and can leave uncommitted parts behind.
func (s *ociStorage) UploadFile(ctx context.Context, storagePath, filepath string, opts PutOptions) (string, int64, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	if err = s.put(ctx, storagePath, file, stat.Size(), opts); err != nil {
		return "", 0, err
	}
	return s.location(storagePath), stat.Size(), nil
}

func (s *ociStorage) location(storagePath string) string {
	return s.url(path.Join("/n", s.namespace, "b", s.conf.Bucket, "o", storagePath))
}

func (s *ociStorage) url(p string) string {
	scheme := "https"
	if s.insecure {
		scheme = "http"
	}
	return (&url.URL{Scheme: scheme, Host: s.host, Path: p}).String()
}

func (s *ociStorage) DownloadFile(ctx context.Context, storagePath, filepath string) (int64, error) {
	return downloadFile(filepath, func(f *os.File) (int64, error) {
		return s.DownloadObject(ctx, storagePath, f)
	})
}

// PresignedURL creates a pre-authenticated request. Unlike an S3
// presigned URL, a PAR is a durable object: it is listable and revocable, it
// costs an API call to mint, and it needs PAR_MANAGE on the bucket. Its access
// URI is returned only here and cannot be read back afterwards.
func (s *ociStorage) PresignedURL(ctx context.Context, storagePath string, expiration time.Duration) (string, error) {
	if err := s.ready(ctx); err != nil {
		return "", err
	}
	name := fmt.Sprintf("livekit-storage-%s-%d", storagePath, time.Now().UnixNano())
	expires := common.SDKTime{Time: time.Now().Add(expiration)}

	resp, err := s.client.CreatePreauthenticatedRequest(ctx, objectstorage.CreatePreauthenticatedRequestRequest{
		NamespaceName:   &s.namespace,
		BucketName:      &s.conf.Bucket,
		RequestMetadata: common.RequestMetadata{RetryPolicy: &s.grantRetry},
		CreatePreauthenticatedRequestDetails: objectstorage.CreatePreauthenticatedRequestDetails{
			Name:        &name,
			AccessType:  objectstorage.CreatePreauthenticatedRequestDetailsAccessTypeObjectread,
			ObjectName:  &storagePath,
			TimeExpires: &expires,
		},
	})
	if err != nil {
		return "", wrapOCIError(err)
	}

	return s.url(*resp.AccessUri), nil
}

func wrapOCIError(err error) error {
	if err == nil {
		return nil
	}
	var se common.ServiceError
	if errors.As(err, &se) {
		return &ErrorWithStatusCode{
			Err:        err,
			StatusCode: se.GetHTTPStatusCode(),
		}
	}
	return err
}

const ociUploadPartSize = 16 << 20

func (s *ociStorage) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	body := common.NewOCIReadSeekCloser(io.NopCloser(bytes.NewReader(data)))
	return s.put(ctx, storagePath, body, int64(len(data)), opts)
}

// ociContentDisposition has browsers show an object rather than download it,
// as on S3.
var ociContentDisposition = common.String("inline")

// put sends body in one request. It must be seekable, so the SDK can re-read
// it when it retries.
func (s *ociStorage) put(ctx context.Context, storagePath string, body io.ReadCloser, size int64, opts PutOptions) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	req := objectstorage.PutObjectRequest{
		NamespaceName:      &s.namespace,
		BucketName:         &s.conf.Bucket,
		ObjectName:         &storagePath,
		PutObjectBody:      body,
		ContentLength:      &size,
		ContentDisposition: ociContentDisposition,
	}
	if opts.ContentType != "" {
		req.ContentType = &opts.ContentType
	}
	if opts.IfAbsent {
		req.IfNoneMatch = common.String("*")
	}
	_, err := s.client.PutObject(ctx, req)
	return wrapOCIObjectError(err)
}

// UploadObject sends a body smaller than a part with PutObject, otherwise a
// multipart upload of sequential parts through one buffer. IfAbsent is checked
// at create and again at commit, so it is atomic here.
func (s *ociStorage) UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	// The condition is checked again at create and commit; this check spares a
	// refused upload its traffic, and refuses it where a store ignores
	// if-none-match on multipart uploads, as floci-oci does.
	if opts.IfAbsent && presentBeforeUpload(ctx, s, storagePath) {
		return ErrExists
	}
	// The buffer grows only as far as the body goes, so a small one costs no
	// whole part.
	var first bytes.Buffer
	if _, err := first.ReadFrom(io.LimitReader(body, ociUploadPartSize)); err != nil {
		return err
	}
	if first.Len() < ociUploadPartSize {
		return s.PutObject(ctx, storagePath, first.Bytes(), opts)
	}
	part := first.Bytes()

	var ifNoneMatch, contentType *string
	if opts.IfAbsent {
		ifNoneMatch = common.String("*")
	}
	if opts.ContentType != "" {
		contentType = &opts.ContentType
	}
	created, err := s.client.CreateMultipartUpload(ctx, objectstorage.CreateMultipartUploadRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.conf.Bucket,
		IfNoneMatch:   ifNoneMatch,
		CreateMultipartUploadDetails: objectstorage.CreateMultipartUploadDetails{
			Object:             &storagePath,
			ContentType:        contentType,
			ContentDisposition: ociContentDisposition,
		},
	})
	if err != nil {
		return wrapOCIObjectError(err)
	}

	// The abort runs on: it cannot be bounded by canceling it, which races in
	// the SDK, and the caller's deadline may be what failed the upload.
	if err = s.uploadParts(ctx, storagePath, created.UploadId, body, part, len(part), ifNoneMatch); err != nil {
		go func() {
			_, _ = s.client.AbortMultipartUpload(context.WithoutCancel(ctx), objectstorage.AbortMultipartUploadRequest{
				NamespaceName: &s.namespace,
				BucketName:    &s.conf.Bucket,
				ObjectName:    &storagePath,
				UploadId:      created.UploadId,
			})
		}()
	}
	return err
}

// uploadParts sends part[:n], then the rest of body a part at a time, and
// commits. The condition goes on the commit only: on a part, a retry after a
// part that landed would read as the object existing.
func (s *ociStorage) uploadParts(ctx context.Context, storagePath string, uploadID *string, body io.Reader, part []byte, n int, ifNoneMatch *string) error {
	var parts []objectstorage.CommitMultipartUploadPartDetails
	for num := 1; n > 0; num++ {
		size := int64(n)
		resp, err := s.client.UploadPart(ctx, objectstorage.UploadPartRequest{
			NamespaceName:  &s.namespace,
			BucketName:     &s.conf.Bucket,
			ObjectName:     &storagePath,
			UploadId:       uploadID,
			UploadPartNum:  common.Int(num),
			ContentLength:  &size,
			UploadPartBody: io.NopCloser(bytes.NewReader(part[:n])),
		})
		if err != nil {
			return wrapOCIObjectError(err)
		}
		parts = append(parts, objectstorage.CommitMultipartUploadPartDetails{PartNum: common.Int(num), Etag: resp.ETag})

		n, err = io.ReadFull(body, part)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return err
		}
	}

	_, err := s.client.CommitMultipartUpload(ctx, objectstorage.CommitMultipartUploadRequest{
		NamespaceName:                &s.namespace,
		BucketName:                   &s.conf.Bucket,
		ObjectName:                   &storagePath,
		UploadId:                     uploadID,
		IfNoneMatch:                  ifNoneMatch,
		CommitMultipartUploadDetails: objectstorage.CommitMultipartUploadDetails{PartsToCommit: parts},
	})
	return wrapOCIObjectError(err)
}

func (s *ociStorage) GetObject(ctx context.Context, storagePath string) ([]byte, error) {
	return getObject(ctx, s, storagePath)
}

func (s *ociStorage) GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("storage: invalid range offset %d length %d", offset, length)
	}
	var buf bytes.Buffer
	rng := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	_, err := s.download(ctx, storagePath, &buf, &rng)
	var se common.ServiceError
	if errors.As(err, &se) && se.GetHTTPStatusCode() == http.StatusRequestedRangeNotSatisfiable {
		return []byte{}, nil
	}
	// A range starting at the end can also come back 400 InvalidParameter, as on
	// floci-oci; the object's size tells it from a bad request.
	if errors.As(err, &se) && se.GetHTTPStatusCode() == http.StatusBadRequest {
		head, headErr := s.client.HeadObject(ctx, objectstorage.HeadObjectRequest{
			NamespaceName: &s.namespace,
			BucketName:    &s.conf.Bucket,
			ObjectName:    &storagePath,
		})
		if headErr == nil && head.ContentLength != nil && offset >= *head.ContentLength {
			return []byte{}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *ociStorage) DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error) {
	return s.download(ctx, storagePath, w, nil)
}

func (s *ociStorage) download(ctx context.Context, storagePath string, w io.Writer, rng *string) (int64, error) {
	if err := s.ready(ctx); err != nil {
		return 0, err
	}
	resp, err := s.client.GetObject(ctx, objectstorage.GetObjectRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.conf.Bucket,
		ObjectName:    &storagePath,
		Range:         rng,
	})
	if err != nil {
		return 0, wrapOCIObjectError(err)
	}
	defer resp.Content.Close()
	n, err := io.Copy(w, resp.Content)
	return n, wrapOCIObjectError(err)
}

func (s *ociStorage) ObjectExists(ctx context.Context, storagePath string) (bool, error) {
	if err := s.ready(ctx); err != nil {
		return false, err
	}
	_, err := s.client.HeadObject(ctx, objectstorage.HeadObjectRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.conf.Bucket,
		ObjectName:    &storagePath,
	})
	err = wrapOCIObjectError(err)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *ociStorage) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	var keys []string
	var start *string
	for {
		resp, err := s.client.ListObjects(ctx, objectstorage.ListObjectsRequest{
			NamespaceName: &s.namespace,
			BucketName:    &s.conf.Bucket,
			Prefix:        &prefix,
			Start:         start,
		})
		if err != nil {
			return nil, wrapOCIObjectError(err)
		}
		for _, obj := range resp.Objects {
			keys = append(keys, *obj.Name)
		}
		if resp.NextStartWith == nil {
			slices.Sort(keys)
			return keys, nil
		}
		start = resp.NextStartWith
	}
}

// DeleteObjects checks the client once for all its paths, and names every
// path in its error where there is none, as for any path it does not delete.
func (s *ociStorage) DeleteObjects(ctx context.Context, storagePaths []string) error {
	if err := s.ready(ctx); err != nil {
		failed := make(map[string]error, len(storagePaths))
		for _, p := range storagePaths {
			failed[p] = err
		}
		return deleteError(failed)
	}
	return deleteEach(ctx, storagePaths, func(ctx context.Context, p string) error {
		_, err := s.client.DeleteObject(ctx, objectstorage.DeleteObjectRequest{
			NamespaceName: &s.namespace,
			BucketName:    &s.conf.Bucket,
			ObjectName:    &p,
		})
		if err = wrapOCIObjectError(err); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		return nil
	})
}

func wrapOCIObjectError(err error) error {
	if err == nil {
		return nil
	}
	var se common.ServiceError
	if errors.As(err, &se) {
		switch se.GetHTTPStatusCode() {
		case http.StatusNotFound:
			// A HEAD has no body, so its code is the SDK's BadErrorResponse.
			// NotAuthorizedOrNotFound stays an error.
			switch se.GetCode() {
			case "ObjectNotFound", "BadErrorResponse":
				return fmt.Errorf("%w: %w", ErrNotFound, wrapOCIError(err))
			}
		case http.StatusPreconditionFailed:
			return fmt.Errorf("%w: %w", ErrExists, wrapOCIError(err))
		}
	}
	return wrapOCIError(err)
}
