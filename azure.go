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
	"net/url"
	"os"
	"path"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
)

func wrapAzureError(err error) error {
	if err == nil {
		return nil
	}
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return &ErrorWithStatusCode{
			Err:        err,
			StatusCode: respErr.StatusCode,
		}
	}
	return err
}

const azureBlockSize = 4 * 1024 * 1024
const azureParallelism = 16

type azureBLOBStorage struct {
	conf       *AzureConfig
	client     *azblob.Client
	serviceUrl string
}

func NewAzure(conf *AzureConfig) (Storage, error) {
	cred, err := azblob.NewSharedKeyCredential(conf.AccountName, conf.AccountKey)
	if err != nil {
		return nil, err
	}

	serviceUrl := fmt.Sprintf("https://%s.blob.core.windows.net/", conf.AccountName)
	client, err := azblob.NewClientWithSharedKeyCredential(serviceUrl, cred, nil)
	if err != nil {
		return nil, err
	}

	return &azureBLOBStorage{
		conf:       conf,
		client:     client,
		serviceUrl: serviceUrl,
	}, nil
}

func (s *azureBLOBStorage) location(storagePath string) string {
	return (&url.URL{
		Scheme: "https",
		Host:   fmt.Sprintf("%s.blob.core.windows.net", s.conf.AccountName),
		Path:   path.Join(s.conf.ContainerName, storagePath),
	}).String()
}

func (s *azureBLOBStorage) UploadData(data []byte, storagePath, contentType string) (string, int64, error) {
	_, err := s.client.UploadBuffer(context.Background(), s.conf.ContainerName, storagePath, data, &azblob.UploadBufferOptions{
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &contentType},
		BlockSize:   azureBlockSize,
		Concurrency: azureParallelism,
	})
	if err != nil {
		return "", 0, wrapAzureError(err)
	}
	return s.location(storagePath), int64(len(data)), nil
}

func (s *azureBLOBStorage) UploadFile(filepath, storagePath, contentType string) (string, int64, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		_ = file.Close()
	}()

	stat, err := file.Stat()
	if err != nil {
		return "", 0, err
	}

	_, err = s.client.UploadFile(context.Background(), s.conf.ContainerName, storagePath, file, &azblob.UploadFileOptions{
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &contentType},
		BlockSize:   azureBlockSize,
		Concurrency: azureParallelism,
	})
	if err != nil {
		return "", 0, wrapAzureError(err)
	}

	return s.location(storagePath), stat.Size(), nil
}

func (s *azureBLOBStorage) DownloadData(storagePath string) ([]byte, error) {
	ctx := context.Background()
	blobClient := s.client.ServiceClient().NewContainerClient(s.conf.ContainerName).NewBlobClient(storagePath)

	props, err := blobClient.GetProperties(ctx, nil)
	if err != nil {
		return nil, wrapAzureObjectError(err)
	}
	if props.ContentLength == nil {
		return nil, errors.New("azure: missing content length")
	}

	buf := make([]byte, *props.ContentLength)
	_, err = s.client.DownloadBuffer(ctx, s.conf.ContainerName, storagePath, buf, &azblob.DownloadBufferOptions{
		BlockSize:   azureBlockSize,
		Concurrency: azureParallelism,
	})
	if err != nil {
		return nil, wrapAzureObjectError(err)
	}
	return buf, nil
}

func (s *azureBLOBStorage) DownloadFile(filepath, storagePath string) (int64, error) {
	file, err := os.Create(filepath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	_, err = s.client.DownloadFile(context.Background(), s.conf.ContainerName, storagePath, file, &azblob.DownloadFileOptions{
		BlockSize:   azureBlockSize,
		Concurrency: azureParallelism,
	})
	if err != nil {
		return 0, wrapAzureObjectError(err)
	}

	stat, err := file.Stat()
	if err != nil {
		return 0, err
	}
	return stat.Size(), nil
}

func (s *azureBLOBStorage) presignedUrlWithToken(storagePath string, expiration time.Duration) (string, error) {
	if s.conf.TokenCredential == nil {
		return "", errors.New("OAuth required")
	}

	now := time.Now().UTC()
	exp := now.Add(expiration)

	svcClient, err := service.NewClient(s.serviceUrl, s.conf.TokenCredential, nil)
	if err != nil {
		return "", err
	}

	udc, err := svcClient.GetUserDelegationCredential(context.Background(), service.KeyInfo{
		Start:  to.Ptr(now.Format(sas.TimeFormat)),
		Expiry: to.Ptr(exp.Format(sas.TimeFormat)),
	}, nil)
	if err != nil {
		return "", wrapAzureError(err)
	}

	qp, err := sas.BlobSignatureValues{
		Protocol:      sas.ProtocolHTTPS,
		StartTime:     now,
		ExpiryTime:    exp,
		Permissions:   (&sas.BlobPermissions{Read: true}).String(),
		ContainerName: s.conf.ContainerName,
		BlobName:      storagePath,
	}.SignWithUserDelegation(udc)
	if err != nil {
		return "", err
	}

	loc := &url.URL{
		Scheme:   "https",
		Host:     fmt.Sprintf("%s.blob.core.windows.net", s.conf.AccountName),
		Path:     path.Join(s.conf.ContainerName, storagePath),
		RawQuery: qp.Encode(),
	}
	return loc.String(), nil
}

func (s *azureBLOBStorage) DeleteObject(storagePath string) error {
	_, err := s.client.DeleteBlob(context.Background(), s.conf.ContainerName, storagePath, nil)
	return wrapAzureError(err)
}

func (s *azureBLOBStorage) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	return s.UploadObject(ctx, storagePath, bytes.NewReader(data), opts)
}

// UploadObject stages blocks and commits them; IfAbsent is a condition on the
// commit, so it is atomic here. The check before staging only spares a refused
// upload its traffic.
func (s *azureBLOBStorage) UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	if opts.IfAbsent {
		exists, err := s.ObjectExists(ctx, storagePath)
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
	_, err := s.client.UploadStream(ctx, s.conf.ContainerName, storagePath, body, uploadOpts)
	// A racing upload's commit discards this one's staged blocks.
	if opts.IfAbsent && bloberror.HasCode(err, bloberror.InvalidBlockList) {
		return fmt.Errorf("%w: %w", ErrObjectExists, wrapAzureError(err))
	}
	return wrapAzureObjectError(err)
}

func (s *azureBLOBStorage) GetObject(ctx context.Context, storagePath string) ([]byte, error) {
	return getObject(ctx, s, storagePath)
}

func (s *azureBLOBStorage) GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("storage: invalid range offset %d length %d", offset, length)
	}
	var buf bytes.Buffer
	_, err := s.download(ctx, storagePath, &buf, blob.HTTPRange{Offset: offset, Count: length})
	if bloberror.HasCode(err, bloberror.InvalidRange) {
		return []byte{}, nil
	}
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *azureBLOBStorage) DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error) {
	return s.download(ctx, storagePath, w, blob.HTTPRange{})
}

func (s *azureBLOBStorage) download(ctx context.Context, storagePath string, w io.Writer, r blob.HTTPRange) (int64, error) {
	resp, err := s.client.DownloadStream(ctx, s.conf.ContainerName, storagePath, &azblob.DownloadStreamOptions{Range: r})
	if err != nil {
		return 0, wrapAzureObjectError(err)
	}
	body := resp.NewRetryReader(ctx, &blob.RetryReaderOptions{MaxRetries: 3})
	defer body.Close()
	n, err := io.Copy(w, body)
	return n, wrapAzureObjectError(err)
}

func (s *azureBLOBStorage) ObjectExists(ctx context.Context, storagePath string) (bool, error) {
	blobClient := s.client.ServiceClient().NewContainerClient(s.conf.ContainerName).NewBlobClient(storagePath)
	_, err := blobClient.GetProperties(ctx, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return false, nil
	}
	return err == nil, wrapAzureObjectError(err)
}

func (s *azureBLOBStorage) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	pager := s.client.NewListBlobsFlatPager(s.conf.ContainerName, &azblob.ListBlobsFlatOptions{Prefix: &prefix})
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

func (s *azureBLOBStorage) DeleteObjects(ctx context.Context, storagePaths []string) error {
	for _, p := range storagePaths {
		_, err := s.client.DeleteBlob(ctx, s.conf.ContainerName, p, &azblob.DeleteBlobOptions{
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
func (s *azureBLOBStorage) GeneratePresignedUrl(_ context.Context, storagePath string, expiration time.Duration) (string, error) {
	if s.conf.TokenCredential != nil {
		return s.presignedUrlWithToken(storagePath, expiration)
	}
	b := s.client.ServiceClient().NewContainerClient(s.conf.ContainerName).NewBlobClient(storagePath)
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
