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
	"fmt"
	"io"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
)

type azureObjects struct {
	s *azureBLOBStorage
}

func newAzureObjects(conf *AzureConfig) (ObjectStorage, error) {
	s, err := NewAzure(conf)
	if err != nil {
		return nil, err
	}
	return &azureObjects{s: s.(*azureBLOBStorage)}, nil
}

func (o *azureObjects) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	return o.UploadObject(ctx, storagePath, bytes.NewReader(data), opts)
}

// UploadObject stages blocks and commits them; IfAbsent is a condition on the
// commit, so it is atomic here. The check before staging only spares a refused
// upload its traffic.
func (o *azureObjects) UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	if opts.IfAbsent {
		exists, err := o.ObjectExists(ctx, storagePath)
		if err != nil {
			return err
		}
		if exists {
			return ErrObjectExists
		}
	}
	uploadOpts := &azblob.UploadStreamOptions{BlockSize: azureBlockSize, Concurrency: azureParallelism}
	if opts.ContentType != "" {
		uploadOpts.HTTPHeaders = &blob.HTTPHeaders{BlobContentType: to.Ptr(opts.ContentType)}
	}
	if opts.IfAbsent {
		uploadOpts.AccessConditions = &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETagAny)},
		}
	}
	_, err := o.s.client.UploadStream(ctx, o.s.conf.ContainerName, storagePath, body, uploadOpts)
	// A racing upload's commit discards this one's staged blocks.
	if opts.IfAbsent && bloberror.HasCode(err, bloberror.InvalidBlockList) {
		return fmt.Errorf("%w: %w", ErrObjectExists, wrapAzureError(err))
	}
	return wrapAzureObjectError(err)
}

func (o *azureObjects) GetObject(ctx context.Context, storagePath string) ([]byte, error) {
	return getObject(ctx, o, storagePath)
}

func (o *azureObjects) GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("storage: invalid range offset %d length %d", offset, length)
	}
	var buf bytes.Buffer
	_, err := o.download(ctx, storagePath, &buf, blob.HTTPRange{Offset: offset, Count: length})
	if bloberror.HasCode(err, bloberror.InvalidRange) {
		return []byte{}, nil
	}
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (o *azureObjects) DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error) {
	return o.download(ctx, storagePath, w, blob.HTTPRange{})
}

func (o *azureObjects) download(ctx context.Context, storagePath string, w io.Writer, r blob.HTTPRange) (int64, error) {
	resp, err := o.s.client.DownloadStream(ctx, o.s.conf.ContainerName, storagePath, &azblob.DownloadStreamOptions{Range: r})
	if err != nil {
		return 0, wrapAzureObjectError(err)
	}
	body := resp.NewRetryReader(ctx, &blob.RetryReaderOptions{MaxRetries: 3})
	defer body.Close()
	n, err := io.Copy(w, body)
	return n, wrapAzureObjectError(err)
}

func (o *azureObjects) ObjectExists(ctx context.Context, storagePath string) (bool, error) {
	blobClient := o.s.client.ServiceClient().NewContainerClient(o.s.conf.ContainerName).NewBlobClient(storagePath)
	_, err := blobClient.GetProperties(ctx, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return false, nil
	}
	return err == nil, wrapAzureObjectError(err)
}

func (o *azureObjects) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	pager := o.s.client.NewListBlobsFlatPager(o.s.conf.ContainerName, &azblob.ListBlobsFlatOptions{Prefix: &prefix})
	var keys []string
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, wrapAzureObjectError(err)
		}
		for _, item := range page.Segment.BlobItems {
			keys = append(keys, *item.Name)
		}
	}
	return keys, nil
}

func (o *azureObjects) DeleteObjects(ctx context.Context, storagePaths []string) error {
	for _, p := range storagePaths {
		_, err := o.s.client.DeleteBlob(ctx, o.s.conf.ContainerName, p, &azblob.DeleteBlobOptions{
			DeleteSnapshots: to.Ptr(azblob.DeleteSnapshotsOptionTypeInclude),
		})
		if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
			return wrapAzureObjectError(err)
		}
	}
	return nil
}

// GeneratePresignedUrl signs with the account key unless a token credential is
// set, in which case it signs with a user delegation key as Storage does.
func (o *azureObjects) GeneratePresignedUrl(_ context.Context, storagePath string, expiration time.Duration) (string, error) {
	if o.s.conf.TokenCredential != nil {
		return o.s.GeneratePresignedUrl(storagePath, expiration)
	}
	b := o.s.client.ServiceClient().NewContainerClient(o.s.conf.ContainerName).NewBlobClient(storagePath)
	u, err := b.GetSASURL(sas.BlobPermissions{Read: true}, time.Now().Add(expiration), nil)
	return u, wrapAzureError(err)
}

func wrapAzureObjectError(err error) error {
	switch {
	case err == nil:
		return nil
	case bloberror.HasCode(err, bloberror.BlobNotFound):
		return fmt.Errorf("%w: %w", ErrObjectNotFound, wrapAzureError(err))
	case bloberror.HasCode(err, bloberror.BlobAlreadyExists, bloberror.ConditionNotMet):
		return fmt.Errorf("%w: %w", ErrObjectExists, wrapAzureError(err))
	}
	return wrapAzureError(err)
}
