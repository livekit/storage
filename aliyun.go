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
	"fmt"
	"os"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

type aliOSSStorage struct {
	conf *AliOSSConfig
}

func NewAliOSS(conf *AliOSSConfig) (Storage, error) {
	return &aliOSSStorage{
		conf: conf,
	}, nil
}

func (s *aliOSSStorage) UploadData(data []byte, storagePath, _ string) (location string, size int64, err error) {
	bucket, err := s.getBucket()
	if err != nil {
		return "", 0, err
	}

	reader := bytes.NewBuffer(data)
	if err = bucket.PutObject(storagePath, reader); err != nil {
		return "", 0, err
	}

	return fmt.Sprintf("https://%s.%s/%s", s.conf.Bucket, s.conf.Endpoint, storagePath), int64(len(data)), nil
}

func (s *aliOSSStorage) UploadFile(filepath, storagePath, _ string) (location string, size int64, err error) {
	stat, err := os.Stat(filepath)
	if err != nil {
		return "", 0, err
	}

	bucket, err := s.getBucket()
	if err != nil {
		return "", 0, err
	}

	if err = bucket.PutObjectFromFile(storagePath, filepath); err != nil {
		return "", 0, err
	}

	return fmt.Sprintf("https://%s.%s/%s", s.conf.Bucket, s.conf.Endpoint, storagePath), stat.Size(), nil
}

func (s *aliOSSStorage) getBucket() (*oss.Bucket, error) {
	client, err := oss.New(s.conf.Endpoint, s.conf.AccessKey, s.conf.Secret)
	if err != nil {
		return nil, err
	}

	return client.Bucket(s.conf.Bucket)
}

func (s *aliOSSStorage) DownloadData(storagePath string) (data []byte, err error) {
	// TODO implement me
	panic("implement me")
}

func (s *aliOSSStorage) DownloadFile(filepath, storagePath string) (size int64, err error) {
	// TODO implement me
	panic("implement me")
}

func (s *aliOSSStorage) GeneratePresignedUrl(storagePath string) (url string, err error) {
	// TODO implement me
	panic("implement me")
}

func (s *aliOSSStorage) Delete(storagePath string) error {
	// TODO implement me
	panic("implement me")
}
