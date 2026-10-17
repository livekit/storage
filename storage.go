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
	"sync/atomic"

	"bytes"
	"context"
	"encoding/base64"
	"golang.org/x/sync/errgroup"
	"net"
	"net/http"
	"net/url"
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
	ErrNotFound = objects.ErrNotFound
	ErrExists   = objects.ErrExists
)

func New(conf Config) (Storage, error) {
	return newStorage(conf)
}

// presentBeforeUpload reports whether IfAbsent can refuse an upload before it is
// sent, because the object is there. A check that fails, such as a 403 for
// credentials that may write but not read, leaves the refusal to the condition
// the store checks when the upload completes.
func presentBeforeUpload(ctx context.Context, b Bucket, storagePath string) bool {
	exists, err := b.ObjectExists(ctx, storagePath)
	return err == nil && exists
}

// getObject is GetObject for a backend whose reads are streams.
func getObject(ctx context.Context, s Bucket, storagePath string) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := s.DownloadObject(ctx, storagePath, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// The clients this package builds cut a connection that stalls, never a slow
// but live transfer: a call's context bounds the call.
const (
	dialTimeout         = 30 * time.Second
	tlsHandshakeTimeout = 10 * time.Second
)

// deleteConcurrency is how many deletes a DeleteObjects sends at once where a
// store takes one object per request (GCS, Azure, OCI): sequential, a large
// delete is one round trip per object.
const deleteConcurrency = 16

// deleteEach deletes every path with del, deleteConcurrency at a time, and
// returns the first error. A failure stops new deletes but lets those in
// flight finish, rather than cancel them: an OCI call canceled mid-flight
// races inside the SDK. A retry is safe, as del treats a missing path as
// deleted.
func deleteEach(ctx context.Context, paths []string, del func(ctx context.Context, path string) error) error {
	var g errgroup.Group
	g.SetLimit(deleteConcurrency)
	var failed atomic.Bool
	for _, p := range paths {
		if failed.Load() {
			break
		}
		g.Go(func() error {
			err := del(ctx, p)
			if err != nil {
				failed.Store(true)
			}
			return err
		})
	}
	return g.Wait()
}

// idleTimeout is how long a connection may move no bytes either way. A store
// can take minutes to answer a large commit.
var idleTimeout = 5 * time.Minute

func setDialTimeouts(d *net.Dialer) {
	d.Timeout = dialTimeout
	d.KeepAlive = 30 * time.Second
}

// setTimeouts sets t's TLS timeout and has its connections time out when idle.
func setTimeouts(t *http.Transport) {
	t.TLSHandshakeTimeout = tlsHandshakeTimeout
	setIdleTimeout(t)
}

// setIdleTimeout has t's connections fail once idle for idleTimeout. A dialer t
// already has, such as one through a VPN, is kept.
func setIdleTimeout(t *http.Transport) {
	dial := t.DialContext
	if dial == nil {
		d := &net.Dialer{}
		setDialTimeouts(d)
		dial = d.DialContext
	}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return idleConn{conn}, nil
	}
}

// idleConn fails once no bytes move either way for idleTimeout. Each read or
// write moves both deadlines: the transport's read for the response is already
// waiting while a long body is still being written.
type idleConn struct{ net.Conn }

func (c idleConn) Read(p []byte) (int, error) {
	if err := c.SetDeadline(time.Now().Add(idleTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c idleConn) Write(p []byte) (int, error) {
	if err := c.SetDeadline(time.Now().Add(idleTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

// transportWithTimeouts is a clone of t, or of Go's default transport, with
// every timeout set.
func transportWithTimeouts(rt http.RoundTripper) *http.Transport {
	// A wrapped transport, such as a tracing one, cannot be cloned, so the
	// client gets a plain one with Go's defaults instead.
	t, ok := rt.(*http.Transport)
	if ok {
		t = t.Clone()
	} else {
		t = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	}
	setTimeouts(t)
	return t
}

// proxySetter routes a transport through p, or leaves it as it is when p is
// nil. A bad URL fails here, so the setter itself can be a transport option.
func proxySetter(p *ProxyConfig) (func(*http.Transport), error) {
	if p == nil {
		return func(*http.Transport) {}, nil
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		return nil, err
	}
	return func(t *http.Transport) {
		t.Proxy = http.ProxyURL(u)
		if p.Username != "" && p.Password != "" {
			auth := base64.StdEncoding.EncodeToString([]byte(p.Username + ":" + p.Password))
			t.ProxyConnectHeader = http.Header{"Proxy-Authorization": {"Basic " + auth}}
		}
	}, nil
}
