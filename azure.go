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
	"strings"
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
	cred       *azblob.SharedKeyCredential
	serviceURL string
}

func NewAzure(conf *AzureConfig) (Storage, error) {
	cred, err := azblob.NewSharedKeyCredential(conf.AccountName, conf.AccountKey)
	if err != nil {
		return nil, err
	}

	serviceURL := azureServiceURL(conf)
	if _, err := url.Parse(serviceURL); err != nil {
		return nil, fmt.Errorf("azure endpoint: %w", err)
	}
	client, err := azblob.NewClientWithSharedKeyCredential(serviceURL, cred, nil)
	if err != nil {
		return nil, err
	}

	return &azureBLOBStorage{
		conf:       conf,
		client:     client,
		cred:       cred,
		serviceURL: serviceURL,
	}, nil
}

func (s *azureBLOBStorage) location(storagePath string) string {
	return s.blobURL(storagePath).String()
}

func azureServiceURL(conf *AzureConfig) string {
	if conf.Endpoint != "" {
		return conf.Endpoint
	}
	return fmt.Sprintf("https://%s.blob.core.windows.net/", conf.AccountName)
}

// blobURL is storagePath under the service URL, path-style below an
// emulator's account segment. NewAzure has checked the URL parses.
func (s *azureBLOBStorage) blobURL(storagePath string) *url.URL {
	u, _ := url.Parse(azureServiceURL(s.conf))
	u.Path = path.Join(u.Path, s.conf.ContainerName, storagePath)
	return u
}

func (s *azureBLOBStorage) UploadFile(ctx context.Context, filepath, storagePath, contentType string) (string, int64, error) {
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

	_, err = s.client.UploadFile(ctx, s.conf.ContainerName, storagePath, file, &azblob.UploadFileOptions{
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &contentType},
		BlockSize:   azureBlockSize,
		Concurrency: azureParallelism,
	})
	if err != nil {
		return "", 0, wrapAzureError(err)
	}

	return s.location(storagePath), stat.Size(), nil
}

func (s *azureBLOBStorage) DownloadFile(ctx context.Context, filepath, storagePath string) (int64, error) {
	file, err := os.Create(filepath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	_, err = s.client.DownloadFile(ctx, s.conf.ContainerName, storagePath, file, &azblob.DownloadFileOptions{
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

func (s *azureBLOBStorage) presignedUrlWithToken(ctx context.Context, storagePath string, expiration time.Duration) (string, error) {
	if s.conf.TokenCredential == nil {
		return "", errors.New("OAuth required")
	}

	now := time.Now().UTC()
	exp := now.Add(expiration)

	svcClient, err := service.NewClient(s.serviceURL, s.conf.TokenCredential, nil)
	if err != nil {
		return "", err
	}

	udc, err := svcClient.GetUserDelegationCredential(ctx, service.KeyInfo{
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

	loc := s.blobURL(storagePath)
	loc.RawQuery = qp.Encode()
	return loc.String(), nil
}

// PutObject sends data up to 256 MiB in one Put Blob, IfAbsent as its
// condition, and larger data in blocks.
func (s *azureBLOBStorage) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	headers, conditions := azureWriteOptions(opts)
	_, err := s.client.UploadBuffer(ctx, s.conf.ContainerName, storagePath, data, &azblob.UploadBufferOptions{
		BlockSize:        azureBlockSize,
		Concurrency:      azureParallelism,
		HTTPHeaders:      headers,
		AccessConditions: conditions,
	})
	return wrapAzureObjectError(err)
}

func azureWriteOptions(opts PutOptions) (*blob.HTTPHeaders, *blob.AccessConditions) {
	var headers *blob.HTTPHeaders
	if opts.ContentType != "" {
		headers = &blob.HTTPHeaders{BlobContentType: to.Ptr(opts.ContentType)}
	}
	var conditions *blob.AccessConditions
	if opts.IfAbsent {
		conditions = &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETagAny)},
		}
	}
	return headers, conditions
}

// UploadObject stages blocks and commits them; IfAbsent is a condition on the
// commit, so it is atomic here. The check before staging only spares a refused
// upload its traffic.
func (s *azureBLOBStorage) UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	if opts.IfAbsent && presentBeforeUpload(ctx, s, storagePath) {
		return ErrExists
	}
	headers, conditions := azureWriteOptions(opts)
	_, err := s.client.UploadStream(ctx, s.conf.ContainerName, storagePath, body, &azblob.UploadStreamOptions{
		BlockSize:        azureBlockSize,
		Concurrency:      azureParallelism,
		HTTPHeaders:      headers,
		AccessConditions: conditions,
	})
	// A racing upload's commit discards this one's staged blocks.
	if opts.IfAbsent && bloberror.HasCode(err, bloberror.InvalidBlockList) {
		return fmt.Errorf("%w: %w", ErrExists, wrapAzureError(err))
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

// ObjectExists reports a directory of an account with a hierarchical namespace
// (ADLS Gen2) as no object, as a store reports a prefix.
func (s *azureBLOBStorage) ObjectExists(ctx context.Context, storagePath string) (bool, error) {
	blobClient := s.client.ServiceClient().NewContainerClient(s.conf.ContainerName).NewBlobClient(storagePath)
	props, err := blobClient.GetProperties(ctx, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return false, nil
	}
	if err != nil {
		return false, wrapAzureObjectError(err)
	}
	return !azureFolder(props.Metadata), nil
}

// azureFolder reports the metadata a hierarchical namespace gives its
// directories. Metadata keys come back in any case.
func azureFolder(metadata map[string]*string) bool {
	for k, v := range metadata {
		if strings.EqualFold(k, "hdi_isfolder") && v != nil && strings.EqualFold(*v, "true") {
			return true
		}
	}
	return false
}

func (s *azureBLOBStorage) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	// With metadata, to leave out a hierarchical namespace's directories.
	pager := s.client.NewListBlobsFlatPager(s.conf.ContainerName, &azblob.ListBlobsFlatOptions{
		Prefix:  &prefix,
		Include: azblob.ListBlobsInclude{Metadata: true},
	})
	var keys []string
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, wrapAzureObjectError(err)
		}
		for _, item := range page.Segment.BlobItems {
			if item.Name != nil && !azureFolder(item.Metadata) {
				keys = append(keys, *item.Name)
			}
		}
	}
	return keys, nil
}

func (s *azureBLOBStorage) DeleteObjects(ctx context.Context, storagePaths []string) error {
	for _, p := range storagePaths {
		// A blob with snapshots is not deleted, as on Storage before: deleting
		// someone's snapshots along with it is not this call's to decide.
		_, err := s.client.DeleteBlob(ctx, s.conf.ContainerName, p, nil)
		if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
			return wrapAzureObjectError(err)
		}
	}
	return nil
}

// PresignedURL signs with the account key unless a token credential is
// set, in which case it signs with a user delegation key as Storage does.
func (s *azureBLOBStorage) PresignedURL(ctx context.Context, storagePath string, expiration time.Duration) (string, error) {
	if s.conf.TokenCredential != nil {
		return s.presignedUrlWithToken(ctx, storagePath, expiration)
	}
	// HTTPS only, as the user delegation SAS, unless the endpoint itself is
	// plain HTTP, as an emulator's.
	protocol := sas.ProtocolHTTPS
	if strings.HasPrefix(s.serviceURL, "http://") {
		protocol = sas.ProtocolHTTPSandHTTP
	}
	qp, err := sas.BlobSignatureValues{
		Protocol:      protocol,
		ExpiryTime:    time.Now().UTC().Add(expiration),
		Permissions:   (&sas.BlobPermissions{Read: true}).String(),
		ContainerName: s.conf.ContainerName,
		BlobName:      storagePath,
	}.SignWithSharedKey(s.cred)
	if err != nil {
		return "", err
	}
	u := s.blobURL(storagePath)
	u.RawQuery = qp.Encode()
	return u.String(), nil
}

func wrapAzureObjectError(err error) error {
	switch {
	case err == nil:
		return nil
	case bloberror.HasCode(err, bloberror.BlobNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, wrapAzureError(err))
	case bloberror.HasCode(err, bloberror.BlobAlreadyExists, bloberror.ConditionNotMet):
		return fmt.Errorf("%w: %w", ErrExists, wrapAzureError(err))
	}
	return wrapAzureError(err)
}
