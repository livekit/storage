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
	"net"
	"net/http"
	"time"

	"github.com/livekit/storage/objects"
)

// The interfaces live in storage/objects so that code implementing or
// accepting them without creating one doesn't pull in every provider SDK.
type (
	Storage    = objects.Storage
	Bucket     = objects.Bucket
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
func getObject(ctx context.Context, s Bucket, storagePath string) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := s.DownloadObject(ctx, storagePath, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// responseHeaderTimeout bounds the wait for a response's headers on the clients
// this package builds. It catches a dead connection without a deadline on the
// body, so a long transfer is never cut; a call's context bounds the call.
const responseHeaderTimeout = time.Minute

// transportWithTimeouts is t, or Go's default transport, with dial, TLS and
// response header timeouts.
func transportWithTimeouts(rt http.RoundTripper) *http.Transport {
	t, ok := rt.(*http.Transport)
	if !ok {
		t = http.DefaultTransport.(*http.Transport)
	}
	t = t.Clone()
	t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = responseHeaderTimeout
	return t
}
