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

type Storage interface {
	UploadData(data []byte, storagePath, contentType string) (location string, size int64, err error)
	UploadFile(filepath, storagePath, contentType string) (location string, size int64, err error)

	DownloadData(storagePath string) (data []byte, err error)
	DownloadFile(filepath, storagePath string) (size int64, err error)

	GeneratePresignedUrl(storagePath string) (url string, err error)

	Delete(storagePath string) error
}