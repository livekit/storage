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
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
)

func wrapAliOSSError(err error) error {
	if err == nil {
		return nil
	}
	var svcErr *oss.ServiceError
	if errors.As(err, &svcErr) {
		return &ErrorWithStatusCode{
			Err:        err,
			StatusCode: svcErr.StatusCode,
		}
	}
	return err
}

type aliOSSStorage struct {
	conf   *AliOSSConfig
	client *oss.Client
}

func NewAliOSS(conf *AliOSSConfig) (Storage, error) {
	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(conf.AccessKey, conf.Secret)).
		WithEndpoint(conf.Endpoint).
		WithRegion(aliOSSRegion(conf))

	return &aliOSSStorage{
		conf:   conf,
		client: oss.NewClient(cfg),
	}, nil
}

// aliOSSRegion returns the region used for request signing. It prefers an
// explicitly configured region and otherwise derives one from the endpoint,
// which for public OSS endpoints has the form oss-<region>[-internal].aliyuncs.com.
func aliOSSRegion(conf *AliOSSConfig) string {
	if conf.Region != "" {
		return conf.Region
	}

	host := strings.TrimPrefix(conf.Endpoint, "https://")
	host = strings.TrimPrefix(host, "http://")
	if i := strings.IndexAny(host, "/:"); i >= 0 {
		host = host[:i]
	}
	host = strings.TrimSuffix(host, ".aliyuncs.com")
	host = strings.TrimPrefix(host, "oss-")
	host = strings.TrimSuffix(host, "-internal")
	return host
}

func (s *aliOSSStorage) UploadData(data []byte, storagePath, contentType string) (string, int64, error) {
	_, err := s.client.PutObject(context.Background(), &oss.PutObjectRequest{
		Bucket:      oss.Ptr(s.conf.Bucket),
		Key:         oss.Ptr(storagePath),
		Body:        bytes.NewReader(data),
		ContentType: oss.Ptr(contentType),
	})
	if err != nil {
		return "", 0, wrapAliOSSError(err)
	}

	return s.location(storagePath), int64(len(data)), nil
}

func (s *aliOSSStorage) UploadFile(filepath, storagePath, contentType string) (string, int64, error) {
	info, err := os.Stat(filepath)
	if err != nil {
		return "", 0, err
	}

	_, err = s.client.PutObjectFromFile(context.Background(), &oss.PutObjectRequest{
		Bucket:      oss.Ptr(s.conf.Bucket),
		Key:         oss.Ptr(storagePath),
		ContentType: oss.Ptr(contentType),
	}, filepath)
	if err != nil {
		return "", 0, wrapAliOSSError(err)
	}

	return s.location(storagePath), info.Size(), nil
}

func (s *aliOSSStorage) location(storagePath string) string {
	endpoint := strings.TrimPrefix(s.conf.Endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	u := url.URL{Scheme: "https", Host: s.conf.Bucket + "." + endpoint, Path: storagePath}
	return u.String()
}

func (s *aliOSSStorage) ListObjects(prefix string) ([]string, error) {
	var objects []string
	var continuationToken *string
	for {
		lor, err := s.client.ListObjectsV2(context.Background(), &oss.ListObjectsV2Request{
			Bucket:            oss.Ptr(s.conf.Bucket),
			Prefix:            oss.Ptr(prefix),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return nil, wrapAliOSSError(err)
		}

		for _, object := range lor.Contents {
			objects = append(objects, oss.ToString(object.Key))
		}

		if !lor.IsTruncated {
			break
		}
		continuationToken = lor.NextContinuationToken
	}

	return objects, nil
}

func (s *aliOSSStorage) DownloadData(storagePath string) ([]byte, error) {
	result, err := s.client.GetObject(context.Background(), &oss.GetObjectRequest{
		Bucket: oss.Ptr(s.conf.Bucket),
		Key:    oss.Ptr(storagePath),
	})
	if err != nil {
		return nil, wrapAliOSSError(err)
	}
	defer result.Body.Close()

	data, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, wrapAliOSSError(err)
	}
	return data, nil
}

func (s *aliOSSStorage) DownloadFile(filepath, storagePath string) (int64, error) {
	_, err := s.client.GetObjectToFile(context.Background(), &oss.GetObjectRequest{
		Bucket: oss.Ptr(s.conf.Bucket),
		Key:    oss.Ptr(storagePath),
	}, filepath)
	if err != nil {
		return 0, wrapAliOSSError(err)
	}

	info, err := os.Stat(filepath)
	if err != nil {
		return 0, err
	}

	return info.Size(), nil
}

func (s *aliOSSStorage) GeneratePresignedUrl(storagePath string, expiration time.Duration) (string, error) {
	result, err := s.client.Presign(context.Background(), &oss.GetObjectRequest{
		Bucket: oss.Ptr(s.conf.Bucket),
		Key:    oss.Ptr(storagePath),
	}, oss.PresignExpires(expiration))
	if err != nil {
		return "", wrapAliOSSError(err)
	}
	return result.URL, nil
}

func (s *aliOSSStorage) DeleteObject(storagePath string) error {
	_, err := s.client.DeleteObject(context.Background(), &oss.DeleteObjectRequest{
		Bucket: oss.Ptr(s.conf.Bucket),
		Key:    oss.Ptr(storagePath),
	})
	return wrapAliOSSError(err)
}

func (s *aliOSSStorage) DeleteObjects(storagePaths []string) error {
	objects := make([]oss.DeleteObject, 0, len(storagePaths))
	for _, p := range storagePaths {
		objects = append(objects, oss.DeleteObject{Key: oss.Ptr(p)})
	}

	_, err := s.client.DeleteMultipleObjects(context.Background(), &oss.DeleteMultipleObjectsRequest{
		Bucket:  oss.Ptr(s.conf.Bucket),
		Objects: objects,
	})
	return wrapAliOSSError(err)
}
