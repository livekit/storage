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

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
)

const ociUploadPartSize = 16 << 20

type ociObjects struct {
	s      *ociStorage
	client objectstorage.ObjectStorageClient
}

// NewOCIObjects gives its calls a client of their own without the SDK's 60s
// request timeout, which covers reading a body and would cut a long download:
// the caller's context bounds each call instead.
func NewOCIObjects(conf *OCIConfig) (ObjectStorage, error) {
	s, err := NewOCI(conf)
	if err != nil {
		return nil, err
	}
	o := &ociObjects{s: s.(*ociStorage)}
	o.client = o.s.client
	if hc, ok := o.client.HTTPClient.(*http.Client); ok {
		o.client.HTTPClient = &http.Client{Transport: hc.Transport, CheckRedirect: hc.CheckRedirect, Jar: hc.Jar}
	}
	return o, nil
}

func (o *ociObjects) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	size := int64(len(data))
	req := objectstorage.PutObjectRequest{
		NamespaceName: &o.s.namespace,
		BucketName:    &o.s.conf.Bucket,
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
	_, err := o.client.PutObject(ctx, req)
	return wrapOCIObjectError(err)
}

// UploadObject sends a body that fits one part with PutObject, otherwise a
// multipart upload of sequential parts through one buffer. IfAbsent is checked
// at create and again at commit, so it is atomic here.
func (o *ociObjects) UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	part := make([]byte, ociUploadPartSize)
	n, err := io.ReadFull(body, part)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return o.PutObject(ctx, storagePath, part[:n], opts)
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
	created, err := o.client.CreateMultipartUpload(ctx, objectstorage.CreateMultipartUploadRequest{
		NamespaceName: &o.s.namespace,
		BucketName:    &o.s.conf.Bucket,
		IfNoneMatch:   ifNoneMatch,
		CreateMultipartUploadDetails: objectstorage.CreateMultipartUploadDetails{
			Object:      &storagePath,
			ContentType: contentType,
		},
	})
	if err != nil {
		return wrapOCIObjectError(err)
	}

	if err = o.uploadParts(ctx, storagePath, created.UploadId, body, part, n, ifNoneMatch); err != nil {
		_, _ = o.client.AbortMultipartUpload(context.WithoutCancel(ctx), objectstorage.AbortMultipartUploadRequest{
			NamespaceName: &o.s.namespace,
			BucketName:    &o.s.conf.Bucket,
			ObjectName:    &storagePath,
			UploadId:      created.UploadId,
		})
	}
	return err
}

// uploadParts sends part[:n], then the rest of body a part at a time, and
// commits. The condition goes on the commit only: on a part, a retry after a
// part that landed would read as the object existing.
func (o *ociObjects) uploadParts(ctx context.Context, storagePath string, uploadID *string, body io.Reader, part []byte, n int, ifNoneMatch *string) error {
	var parts []objectstorage.CommitMultipartUploadPartDetails
	for num := 1; n > 0; num++ {
		size := int64(n)
		resp, err := o.client.UploadPart(ctx, objectstorage.UploadPartRequest{
			NamespaceName:  &o.s.namespace,
			BucketName:     &o.s.conf.Bucket,
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

	_, err := o.client.CommitMultipartUpload(ctx, objectstorage.CommitMultipartUploadRequest{
		NamespaceName:                &o.s.namespace,
		BucketName:                   &o.s.conf.Bucket,
		ObjectName:                   &storagePath,
		UploadId:                     uploadID,
		IfNoneMatch:                  ifNoneMatch,
		CommitMultipartUploadDetails: objectstorage.CommitMultipartUploadDetails{PartsToCommit: parts},
	})
	return wrapOCIObjectError(err)
}

func (o *ociObjects) GetObject(ctx context.Context, storagePath string) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := o.DownloadObject(ctx, storagePath, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (o *ociObjects) GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("storage: invalid range offset %d length %d", offset, length)
	}
	var buf bytes.Buffer
	rng := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	_, err := o.download(ctx, storagePath, &buf, &rng)
	var se common.ServiceError
	if errors.As(err, &se) && se.GetHTTPStatusCode() == http.StatusRequestedRangeNotSatisfiable {
		return []byte{}, nil
	}
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (o *ociObjects) DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error) {
	return o.download(ctx, storagePath, w, nil)
}

func (o *ociObjects) download(ctx context.Context, storagePath string, w io.Writer, rng *string) (int64, error) {
	resp, err := o.client.GetObject(ctx, objectstorage.GetObjectRequest{
		NamespaceName: &o.s.namespace,
		BucketName:    &o.s.conf.Bucket,
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

func (o *ociObjects) ObjectExists(ctx context.Context, storagePath string) (bool, error) {
	_, err := o.client.HeadObject(ctx, objectstorage.HeadObjectRequest{
		NamespaceName: &o.s.namespace,
		BucketName:    &o.s.conf.Bucket,
		ObjectName:    &storagePath,
	})
	err = wrapOCIObjectError(err)
	if errors.Is(err, ErrObjectNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (o *ociObjects) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var start *string
	for {
		resp, err := o.client.ListObjects(ctx, objectstorage.ListObjectsRequest{
			NamespaceName: &o.s.namespace,
			BucketName:    &o.s.conf.Bucket,
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

func (o *ociObjects) DeleteObjects(ctx context.Context, storagePaths []string) error {
	for _, p := range storagePaths {
		_, err := o.client.DeleteObject(ctx, objectstorage.DeleteObjectRequest{
			NamespaceName: &o.s.namespace,
			BucketName:    &o.s.conf.Bucket,
			ObjectName:    &p,
		})
		if err = wrapOCIObjectError(err); err != nil && !errors.Is(err, ErrObjectNotFound) {
			return err
		}
	}
	return nil
}

func (o *ociObjects) GeneratePresignedUrl(_ context.Context, storagePath string, expiration time.Duration) (string, error) {
	return o.s.GeneratePresignedUrl(storagePath, expiration)
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
