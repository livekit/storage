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
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
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

func (s *azureBLOBStorage) ListObjects(prefix string) ([]string, error) {
	var objects []string

	pager := s.client.NewListBlobsFlatPager(s.conf.ContainerName, &azblob.ListBlobsFlatOptions{
		Prefix: &prefix,
	})
	for pager.More() {
		page, err := pager.NextPage(context.Background())
		if err != nil {
			return nil, wrapAzureError(err)
		}
		for _, item := range page.Segment.BlobItems {
			if item.Name != nil {
				objects = append(objects, *item.Name)
			}
		}
	}

	return objects, nil
}

func (s *azureBLOBStorage) DownloadData(storagePath string) ([]byte, error) {
	ctx := context.Background()
	blobClient := s.client.ServiceClient().NewContainerClient(s.conf.ContainerName).NewBlobClient(storagePath)

	props, err := blobClient.GetProperties(ctx, nil)
	if err != nil {
		return nil, wrapAzureError(err)
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
		return nil, wrapAzureError(err)
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
		return 0, wrapAzureError(err)
	}

	stat, err := file.Stat()
	if err != nil {
		return 0, err
	}
	return stat.Size(), nil
}

func (s *azureBLOBStorage) GeneratePresignedUrl(storagePath string, expiration time.Duration) (string, error) {
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

func (s *azureBLOBStorage) DeleteObjects(storagePaths []string) error {
	for _, p := range storagePaths {
		if err := s.DeleteObject(p); err != nil {
			return err
		}
	}
	return nil
}
