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
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/Azure/azure-storage-blob-go/azblob"
)

type azureBLOBStorage struct {
	conf      *AzureConfig
	container string
}

func NewAzure(conf *AzureConfig) (Storage, error) {
	return &azureBLOBStorage{
		conf:      conf,
		container: fmt.Sprintf("https://%s.blob.core.windows.net/%s", conf.AccountName, conf.ContainerName),
	}, nil
}

func (s *azureBLOBStorage) UploadData(data []byte, storagePath, contentType string) (string, int64, error) {
	// TODO implement me
	panic("implement me")
}

func (s *azureBLOBStorage) UploadFile(filepath, storagePath, contentType string) (string, int64, error) {
	return s.upload(filepath, storagePath, contentType)
}

func (s *azureBLOBStorage) upload(filepath, storagePath, contentType string) (string, int64, error) {
	credential, err := azblob.NewSharedKeyCredential(
		s.conf.AccountName,
		s.conf.AccountKey,
	)
	if err != nil {
		return "", 0, err
	}

	azUrl, err := url.Parse(s.container)
	if err != nil {
		return "", 0, err
	}

	pipeline := azblob.NewPipeline(credential, azblob.PipelineOptions{
		Retry: azblob.RetryOptions{
			Policy:        azblob.RetryPolicyExponential,
			MaxTries:      5,
			RetryDelay:    time.Millisecond * 100,
			MaxRetryDelay: time.Second * 5,
		},
	})
	containerURL := azblob.NewContainerURL(*azUrl, pipeline)
	blobURL := containerURL.NewBlockBlobURL(storagePath)

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

	// upload blocks in parallel for optimal performance
	// it calls PutBlock/PutBlockList for files larger than 256 MBs and PutBlob for smaller files
	_, err = azblob.UploadFileToBlockBlob(context.Background(), file, blobURL, azblob.UploadToBlockBlobOptions{
		BlobHTTPHeaders: azblob.BlobHTTPHeaders{ContentType: contentType},
		BlockSize:       4 * 1024 * 1024,
		Parallelism:     16,
	})
	if err != nil {
		return "", 0, err
	}

	return fmt.Sprintf("%s/%s", s.container, storagePath), stat.Size(), nil
}

func (s *azureBLOBStorage) DownloadData(storagePath string) ([]byte, error) {
	// TODO implement me
	panic("implement me")
}

func (s *azureBLOBStorage) DownloadFile(filepath, storagePath string) (int64, error) {
	err := s.download(filepath, storagePath)
	if err != nil {
		return 0, err
	}

	stat, err := os.Stat(filepath)
	if err != nil {
		return 0, err
	}

	return stat.Size(), nil
}

func (s *azureBLOBStorage) download(filepath, storagePath string) error {
	credential, err := azblob.NewSharedKeyCredential(
		s.conf.AccountName,
		s.conf.AccountKey,
	)
	if err != nil {
		return err
	}

	pipeline := azblob.NewPipeline(credential, azblob.PipelineOptions{
		Retry: azblob.RetryOptions{
			Policy:        azblob.RetryPolicyExponential,
			MaxTries:      5,
			MaxRetryDelay: time.Second * 5,
		},
	})
	sUrl := fmt.Sprintf("https://%s.blob.core.windows.net/%s", s.conf.AccountName, s.conf.ContainerName)
	azUrl, err := url.Parse(sUrl)
	if err != nil {
		return err
	}

	containerURL := azblob.NewContainerURL(*azUrl, pipeline)
	blobURL := containerURL.NewBlobURL(storagePath)

	file, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer file.Close()

	return azblob.DownloadBlobToFile(context.Background(), blobURL, 0, 0, file, azblob.DownloadFromBlobOptions{
		BlockSize:   4 * 1024 * 1024,
		Parallelism: 16,
		RetryReaderOptionsPerBlock: azblob.RetryReaderOptions{
			MaxRetryRequests: 3,
		},
	})
}

func (s *azureBLOBStorage) GeneratePresignedUrl(storagePath string) (string, error) {
	// TODO implement me
	panic("implement me")
}

func (s *azureBLOBStorage) Delete(storagePath string) error {
	credential, err := azblob.NewSharedKeyCredential(
		s.conf.AccountName,
		s.conf.AccountKey,
	)
	if err != nil {
		return err
	}

	pipeline := azblob.NewPipeline(credential, azblob.PipelineOptions{
		Retry: azblob.RetryOptions{
			Policy:        azblob.RetryPolicyExponential,
			MaxTries:      5,
			MaxRetryDelay: time.Second * 5,
		},
	})
	sUrl := fmt.Sprintf("https://%s.blob.core.windows.net/%s", s.conf.AccountName, s.conf.ContainerName)
	azUrl, err := url.Parse(sUrl)
	if err != nil {
		return err
	}

	containerURL := azblob.NewContainerURL(*azUrl, pipeline)
	blobURL := containerURL.NewBlobURL(storagePath)
	_, err = blobURL.Delete(context.Background(), azblob.DeleteSnapshotsOptionNone, azblob.BlobAccessConditions{})
	return err
}
