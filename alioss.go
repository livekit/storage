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
	"errors"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

func wrapAliOSSError(err error) error {
	if err == nil {
		return nil
	}
	var svcErr oss.ServiceError
	if errors.As(err, &svcErr) {
		return &ErrorWithStatusCode{
			Err:        err,
			StatusCode: svcErr.StatusCode,
		}
	}
	var unexpectedErr oss.UnexpectedStatusCodeError
	if errors.As(err, &unexpectedErr) {
		return &ErrorWithStatusCode{
			Err:        err,
			StatusCode: unexpectedErr.Got(),
		}
	}
	return err
}

type aliOSSStorage struct {
	conf   *AliOSSConfig
	bucket *oss.Bucket
}

func NewAliOSS(conf *AliOSSConfig) (Storage, error) {
	client, err := oss.New(conf.Endpoint, conf.AccessKey, conf.Secret)
	if err != nil {
		return nil, err
	}

	bucket, err := client.Bucket(conf.Bucket)
	if err != nil {
		return nil, err
	}

	return &aliOSSStorage{
		conf:   conf,
		bucket: bucket,
	}, nil
}

func (s *aliOSSStorage) UploadData(data []byte, storagePath, contentType string) (string, int64, error) {
	reader := bytes.NewBuffer(data)
	if err := s.bucket.PutObject(storagePath, reader, oss.ContentType(contentType)); err != nil {
		return "", 0, wrapAliOSSError(err)
	}

	return s.location(storagePath), int64(len(data)), nil
}

func (s *aliOSSStorage) UploadFile(filepath, storagePath, contentType string) (string, int64, error) {
	info, err := os.Stat(filepath)
	if err != nil {
		return "", 0, err
	}

	if err = s.bucket.PutObjectFromFile(storagePath, filepath, oss.ContentType(contentType)); err != nil {
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
	marker := oss.Marker("")
	for {
		lor, err := s.bucket.ListObjects(oss.Prefix(prefix), marker)
		if err != nil {
			return nil, wrapAliOSSError(err)
		}

		for _, object := range lor.Objects {
			objects = append(objects, object.Key)
		}

		if !lor.IsTruncated {
			break
		}
		marker = oss.Marker(lor.NextMarker)
	}

	return objects, nil
}

func (s *aliOSSStorage) DownloadData(storagePath string) ([]byte, error) {
	reader, err := s.bucket.GetObject(storagePath)
	if err != nil {
		return nil, wrapAliOSSError(err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, wrapAliOSSError(err)
	}
	return data, nil
}

func (s *aliOSSStorage) DownloadFile(filepath, storagePath string) (int64, error) {
	if err := s.bucket.GetObjectToFile(storagePath, filepath); err != nil {
		return 0, wrapAliOSSError(err)
	}

	info, err := os.Stat(filepath)
	if err != nil {
		return 0, err
	}

	return info.Size(), nil
}

func (s *aliOSSStorage) GeneratePresignedUrl(storagePath string, expiration time.Duration) (string, error) {
	u, err := s.bucket.SignURL(storagePath, oss.HTTPGet, int64(expiration.Seconds()))
	if err != nil {
		return "", wrapAliOSSError(err)
	}
	return u, nil
}

func (s *aliOSSStorage) DeleteObject(storagePath string) error {
	return wrapAliOSSError(s.bucket.DeleteObject(storagePath))
}

func (s *aliOSSStorage) DeleteObjects(storagePaths []string) error {
	_, err := s.bucket.DeleteObjects(storagePaths)
	return wrapAliOSSError(err)
}
