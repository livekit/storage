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

	"github.com/livekit/storage/objects"
)

// The interface lives in storage/objects so that code implementing or
// accepting it without creating one doesn't pull in every provider SDK.
type (
	Storage    = objects.Storage
	PutOptions = objects.PutOptions
)

var (
	ErrObjectNotFound = objects.ErrNotFound
	ErrObjectExists   = objects.ErrExists
)

func New(conf Config) (Storage, error) {
	return newStorage(conf)
}

// getObject is GetObject for a backend whose reads are streams.
func getObject(ctx context.Context, s Storage, storagePath string) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := s.DownloadObject(ctx, storagePath, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
