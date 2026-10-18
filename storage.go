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

// Package storage is object storage on S3 and S3-compatible stores, GCS,
// Azure Blob, OCI Object Storage, AliOSS and a local directory, behind the
// interfaces in storage/objects. New builds a Storage from a config.
package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http/httpproxy"
	"golang.org/x/sync/errgroup"

	"github.com/livekit/storage/objects"
)

// The interfaces live in storage/objects so that code implementing or
// accepting them without creating one doesn't pull in every provider SDK.
type (
	Storage             = objects.Storage
	Bucket              = objects.Bucket
	PutOptions          = objects.PutOptions
	ErrorWithStatusCode = objects.ErrorWithStatusCode
	DeleteError         = objects.DeleteError
)

var (
	ErrNotFound = objects.ErrNotFound
	ErrExists   = objects.ErrExists
)

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

// createFile opens name for writing, with its parents, in the mode os.Create
// gives, 0666 less the umask, so other users, such as a host reading a
// container's volume, can read it. A delete can remove the parent once empty
// before the file is in it, so it is made again.
func createFile(name string, flag int) (*os.File, error) {
	for {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|flag, 0o666)
		if !errors.Is(err, fs.ErrNotExist) {
			return f, err
		}
	}
}

// createTemp is a new file in dir named prefix and a random suffix.
func createTemp(dir, prefix string) (*os.File, error) {
	for {
		f, err := createFile(filepath.Join(dir, prefix+strconv.FormatUint(rand.Uint64(), 36)), os.O_EXCL)
		if !errors.Is(err, fs.ErrExist) {
			return f, err
		}
	}
}

// writeFile copies body into f, syncs and closes it.
func writeFile(f *os.File, body io.Reader) (int64, error) {
	n, err := io.Copy(f, body)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return n, err
}

// downloadFile fetches an object into a file beside name, renamed to name once
// complete, so a missing object or a failed download leaves name as it was.
func downloadFile(name string, fetch func(f *os.File) (int64, error)) (int64, error) {
	tmp, err := createTemp(filepath.Dir(name), "."+filepath.Base(name)+".download-")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())

	n, err := fetch(tmp)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, err
	}
	return n, os.Rename(tmp.Name(), name)
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

// deleteEach deletes every path with del, deleteConcurrency at a time, where a
// store takes one path per request. Once the context ends, the paths left are
// failed without a request: an OCI call canceled mid-flight races in its SDK.
func deleteEach(ctx context.Context, paths []string, del func(ctx context.Context, path string) error) error {
	var mu sync.Mutex
	failed := map[string]error{}
	var g errgroup.Group
	g.SetLimit(deleteConcurrency)
	for _, p := range paths {
		g.Go(func() error {
			err := ctx.Err()
			if err == nil {
				err = del(ctx, p)
			}
			if err != nil {
				mu.Lock()
				failed[p] = err
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	return deleteError(failed)
}

// deleteError is a *DeleteError for the paths in failed, or nil for none.
func deleteError(failed map[string]error) error {
	if len(failed) == 0 {
		return nil
	}
	return &DeleteError{Failed: failed}
}

// idleTimeout is how long a connection may move no bytes either way. A store
// can take minutes to answer a large commit.
var idleTimeout = 5 * time.Minute

func setDialTimeouts(d *net.Dialer) {
	d.Timeout = dialTimeout
	d.KeepAlive = 30 * time.Second
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
	timeout := idleTimeout
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &idleConn{Conn: conn, timeout: timeout}, nil
	}
}

// idleConn fails once no bytes move either way for timeout. Each read or write
// moves both deadlines: the transport's read for the response is already
// waiting while a long body is still being written. A deadline set from above,
// such as TLS's on close or net/http's to interrupt a read, is never extended.
type idleConn struct {
	net.Conn
	timeout time.Duration
	// readBy and writeBy are the deadlines set from above, in Unix
	// nanoseconds, 0 for none.
	readBy, writeBy atomic.Int64
}

func (c *idleConn) Read(p []byte) (int, error) {
	if err := c.extend(); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	if err := c.extend(); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

func (c *idleConn) extend() error {
	idle := time.Now().Add(c.timeout)
	if err := c.Conn.SetReadDeadline(earliest(idle, c.readBy.Load())); err != nil {
		return err
	}
	return c.Conn.SetWriteDeadline(earliest(idle, c.writeBy.Load()))
}

func (c *idleConn) SetDeadline(t time.Time) error {
	c.readBy.Store(unixNano(t))
	c.writeBy.Store(unixNano(t))
	return c.Conn.SetDeadline(t)
}

func (c *idleConn) SetReadDeadline(t time.Time) error {
	c.readBy.Store(unixNano(t))
	return c.Conn.SetReadDeadline(t)
}

func (c *idleConn) SetWriteDeadline(t time.Time) error {
	c.writeBy.Store(unixNano(t))
	return c.Conn.SetWriteDeadline(t)
}

// earliest is idle, or the deadline set from above when that comes first.
func earliest(idle time.Time, by int64) time.Time {
	if by != 0 && by < idle.UnixNano() {
		return time.Unix(0, by)
	}
	return idle
}

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// maxIdleConnsPerHost keeps a connection for each request a call can have in
// flight, deletes and upload blocks: Go's default keeps two and redials the rest.
const maxIdleConnsPerHost = 32

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
	t.MaxIdleConnsPerHost = max(t.MaxIdleConnsPerHost, maxIdleConnsPerHost)
	t.TLSHandshakeTimeout = tlsHandshakeTimeout
	setIdleTimeout(t)
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
	// Credentials in the URL go to the proxy for http targets as well as on
	// CONNECT for https ones.
	if p.Username != "" && p.Password != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	// NO_PROXY is honored, and the cloud metadata services, which hand out
	// instance credentials, are never proxied: a proxy cannot reach them.
	proxy := (&httpproxy.Config{
		HTTPProxy:  u.String(),
		HTTPSProxy: u.String(),
		NoProxy:    strings.Join([]string{os.Getenv("NO_PROXY"), os.Getenv("no_proxy"), metadataHosts}, ","),
	}).ProxyFunc()
	return func(t *http.Transport) {
		t.Proxy = func(r *http.Request) (*url.URL, error) { return proxy(r.URL) }
	}, nil
}

// metadataHosts are the instance metadata and container credential endpoints
// of EC2, ECS, EKS Pod Identity and GCE.
const metadataHosts = "169.254.169.254,169.254.170.2,169.254.170.23,fd00:ec2::254,fd00:ec2::23,metadata.google.internal"
