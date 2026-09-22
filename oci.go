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

func (s *ociStorage) UploadData(data []byte, storagePath, contentType string) (string, int64, error) {
	size := int64(len(data))
	body := common.NewOCIReadSeekCloser(io.NopCloser(bytes.NewReader(data)))

	location, err := s.upload(body, size, storagePath, contentType)
	if err != nil {
		return "", 0, err
	}
	return location, size, nil
}

func (s *ociStorage) UploadFile(filepath, storagePath, contentType string) (string, int64, error) {
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

	location, err := s.upload(file, stat.Size(), storagePath, contentType)
	if err != nil {
		return "", 0, err
	}
	return location, stat.Size(), nil
}

// upload takes a seekable body: the SDK re-reads it when it retries.
func (s *ociStorage) upload(body io.ReadCloser, size int64, storagePath, contentType string) (string, error) {
	contentDisposition := "inline"
	_, err := s.client.PutObject(context.Background(), objectstorage.PutObjectRequest{
		NamespaceName:      &s.namespace,
		BucketName:         &s.conf.Bucket,
		ObjectName:         &storagePath,
		PutObjectBody:      body,
		ContentLength:      &size,
		ContentType:        &contentType,
		ContentDisposition: &contentDisposition,
	})
	if err != nil {
		return "", wrapOCIError(err)
	}

	return s.location(storagePath), nil
}

func (s *ociStorage) location(storagePath string) string {
	return (&url.URL{
		Scheme: "https",
		Host:   s.host,
		Path:   path.Join("/n", s.namespace, "b", s.conf.Bucket, "o", storagePath),
	}).String()
}

func (s *ociStorage) ListObjects(prefix string) ([]string, error) {
	var objects []string
	var start *string

	for {
		resp, err := s.client.ListObjects(context.Background(), objectstorage.ListObjectsRequest{
			NamespaceName: &s.namespace,
			BucketName:    &s.conf.Bucket,
			Prefix:        &prefix,
			Start:         start,
		})
		if err != nil {
			return nil, wrapOCIError(err)
		}

		for _, obj := range resp.Objects {
			objects = append(objects, *obj.Name)
		}

		if resp.NextStartWith == nil {
			return objects, nil
		}
		start = resp.NextStartWith
	}
}

func (s *ociStorage) DownloadData(storagePath string) ([]byte, error) {
	resp, err := s.download(storagePath)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Content.Close()
	}()

	data, err := io.ReadAll(resp.Content)
	if err != nil {
		return nil, wrapOCIError(err)
	}
	return data, nil
}

func (s *ociStorage) DownloadFile(filepath, storagePath string) (int64, error) {
	file, err := os.Create(filepath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	resp, err := s.download(storagePath)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = resp.Content.Close()
	}()

	if _, err = io.Copy(file, resp.Content); err != nil {
		return 0, wrapOCIError(err)
	}

	return *resp.ContentLength, nil
}

func (s *ociStorage) download(storagePath string) (objectstorage.GetObjectResponse, error) {
	resp, err := s.client.GetObject(context.Background(), objectstorage.GetObjectRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.conf.Bucket,
		ObjectName:    &storagePath,
	})
	return resp, wrapOCIError(err)
}

// GeneratePresignedUrl creates a pre-authenticated request. Unlike an S3
// presigned URL, a PAR is a durable object: it is listable and revocable, it
// costs an API call to mint, and it needs PAR_MANAGE on the bucket. Its access
// URI is returned only here and cannot be read back afterwards.
func (s *ociStorage) GeneratePresignedUrl(storagePath string, expiration time.Duration) (string, error) {
	name := fmt.Sprintf("livekit-storage-%s-%d", storagePath, time.Now().UnixNano())
	expires := common.SDKTime{Time: time.Now().Add(expiration)}

	resp, err := s.client.CreatePreauthenticatedRequest(context.Background(), objectstorage.CreatePreauthenticatedRequestRequest{
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

func (s *ociStorage) DeleteObject(storagePath string) error {
	_, err := s.client.DeleteObject(context.Background(), objectstorage.DeleteObjectRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.conf.Bucket,
		ObjectName:    &storagePath,
	})
	return wrapOCIError(err)
}

// DeleteObjects deletes one at a time — OCI has no bulk equivalent — and stops
// at the first failure, leaving earlier deletions in place, as gcp.go does.
func (s *ociStorage) DeleteObjects(storagePaths []string) error {
	for _, storagePath := range storagePaths {
		if err := s.DeleteObject(storagePath); err != nil {
			return err
		}
	}
	return nil
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
