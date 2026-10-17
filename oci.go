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
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
)

type ociStorage struct {
	conf      *OCIConfig
	client    objectstorage.ObjectStorageClient
	namespace string
	host      string
}

func NewOCI(conf *OCIConfig) (Storage, error) {
	provider, err := newConfigProvider(conf)
	if err != nil {
		return nil, err
	}

	client, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}

	if conf.Region != "" {
		client.SetRegion(conf.Region)
	}
	if conf.Endpoint != "" {
		client.Host = conf.Endpoint
	}
	// The SDK's 60s http.Client.Timeout covers reading the body and would cut a
	// long transfer; a response header timeout catches a dead connection instead.
	if hc, ok := client.HTTPClient.(*http.Client); ok {
		client.HTTPClient = &http.Client{Transport: transportWithTimeouts(hc.Transport), CheckRedirect: hc.CheckRedirect, Jar: hc.Jar}
	}

	// The SDK only prepends a scheme to client.Host on the first request, so
	// read it before GetNamespace below.
	host := strings.TrimPrefix(client.Host, "https://")
	host = strings.TrimPrefix(host, "http://")

	s := &ociStorage{
		conf:      conf,
		client:    client,
		namespace: conf.Namespace,
		host:      host,
	}

	if s.namespace == "" {
		req := objectstorage.GetNamespaceRequest{}
		if conf.CompartmentID != "" {
			req.CompartmentId = &conf.CompartmentID
		}
		resp, err := client.GetNamespace(context.Background(), req)
		if err != nil {
			return nil, wrapOCIError(err)
		}
		s.namespace = *resp.Value
	}

	return s, nil
}

func newConfigProvider(conf *OCIConfig) (common.ConfigurationProvider, error) {
	if conf.UseWorkloadIdentity {
		return auth.OkeWorkloadIdentityConfigurationProvider()
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

func (s *ociStorage) UploadData(ctx context.Context, data []byte, storagePath, contentType string) (string, int64, error) {
	if err := s.PutObject(ctx, storagePath, data, PutOptions{ContentType: contentType}); err != nil {
		return "", 0, err
	}
	return s.location(storagePath), int64(len(data)), nil
}

func (s *ociStorage) UploadFile(ctx context.Context, filepath, storagePath, contentType string) (string, int64, error) {
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

func (s *ociStorage) location(storagePath string) string {
	return (&url.URL{
		Scheme: "https",
		Host:   s.host,
		Path:   path.Join("/n", s.namespace, "b", s.conf.Bucket, "o", storagePath),
	}).String()
}

func (s *ociStorage) DownloadFile(ctx context.Context, filepath, storagePath string) (int64, error) {
	file, err := os.Create(filepath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	return s.DownloadObject(ctx, storagePath, file)
}

// GeneratePresignedUrl creates a pre-authenticated request. Unlike an S3
// presigned URL, a PAR is a durable object: it is listable and revocable, it
// costs an API call to mint, and it needs PAR_MANAGE on the bucket. Its access
// URI is returned only here and cannot be read back afterwards.
func (s *ociStorage) presignedUrl(ctx context.Context, storagePath string, expiration time.Duration) (string, error) {
	name := fmt.Sprintf("livekit-storage-%s-%d", storagePath, time.Now().UnixNano())
	expires := common.SDKTime{Time: time.Now().Add(expiration)}

	resp, err := s.client.CreatePreauthenticatedRequest(ctx, objectstorage.CreatePreauthenticatedRequestRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.conf.Bucket,
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

	return (&url.URL{
		Scheme: "https",
		Host:   s.host,
		Path:   *resp.AccessUri,
	}).String(), nil
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
	size := int64(len(data))
	req := objectstorage.PutObjectRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.conf.Bucket,
		ObjectName:    &storagePath,
		// Seekable, so the SDK can re-read it when it retries.
		PutObjectBody: common.NewOCIReadSeekCloser(io.NopCloser(bytes.NewReader(data))),
		ContentLength: &size,
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

// UploadObject sends a body that fits one part with PutObject, otherwise a
// multipart upload of sequential parts through one buffer. IfAbsent is checked
// at create and again at commit, so it is atomic here.
func (s *ociStorage) UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	part := make([]byte, ociUploadPartSize)
	n, err := io.ReadFull(body, part)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return s.PutObject(ctx, storagePath, part[:n], opts)
	}
	if err != nil {
		return err
	}

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
			Object:      &storagePath,
			ContentType: contentType,
		},
	})
	if err != nil {
		return wrapOCIObjectError(err)
	}

	if err = s.uploadParts(ctx, storagePath, created.UploadId, body, part, n, ifNoneMatch); err != nil {
		_, _ = s.client.AbortMultipartUpload(context.WithoutCancel(ctx), objectstorage.AbortMultipartUploadRequest{
			NamespaceName: &s.namespace,
			BucketName:    &s.conf.Bucket,
			ObjectName:    &storagePath,
			UploadId:      created.UploadId,
		})
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
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *ociStorage) DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error) {
	return s.download(ctx, storagePath, w, nil)
}

func (s *ociStorage) download(ctx context.Context, storagePath string, w io.Writer, rng *string) (int64, error) {
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
	_, err := s.client.HeadObject(ctx, objectstorage.HeadObjectRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.conf.Bucket,
		ObjectName:    &storagePath,
	})
	err = wrapOCIObjectError(err)
	if errors.Is(err, ErrObjectNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *ociStorage) ListObjects(ctx context.Context, prefix string) ([]string, error) {
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
			return keys, nil
		}
		start = resp.NextStartWith
	}
}

func (s *ociStorage) DeleteObjects(ctx context.Context, storagePaths []string) error {
	for _, p := range storagePaths {
		_, err := s.client.DeleteObject(ctx, objectstorage.DeleteObjectRequest{
			NamespaceName: &s.namespace,
			BucketName:    &s.conf.Bucket,
			ObjectName:    &p,
		})
		if err = wrapOCIObjectError(err); err != nil && !errors.Is(err, ErrObjectNotFound) {
			return err
		}
	}
	return nil
}

func (s *ociStorage) GeneratePresignedUrl(ctx context.Context, storagePath string, expiration time.Duration) (string, error) {
	return s.presignedUrl(ctx, storagePath, expiration)
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
				return fmt.Errorf("%w: %w", ErrObjectNotFound, wrapOCIError(err))
			}
		case http.StatusPreconditionFailed:
			return fmt.Errorf("%w: %w", ErrObjectExists, wrapOCIError(err))
		}
	}
	return wrapOCIError(err)
}
