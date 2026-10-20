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
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/stretchr/testify/require"
)

// A connection is cut when no bytes move for idleTimeout, and never while a
// slow transfer is still moving them.
func TestIdleTimeout(t *testing.T) {
	saved := idleTimeout
	idleTimeout = time.Second
	t.Cleanup(func() { idleTimeout = saved })
	client := &http.Client{Transport: transportWithTimeouts(nil)}

	t.Run("slow upload", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
		}))
		t.Cleanup(srv.Close)
		// The second request reuses the first's connection.
		for range 2 {
			resp, err := client.Post(srv.URL, "text/plain", &trickle{n: 10})
			require.NoError(t, err)
			resp.Body.Close()
		}
	})

	t.Run("slow download", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(w, &trickle{n: 10, flush: w.(http.Flusher)})
		}))
		t.Cleanup(srv.Close)
		resp, err := client.Get(srv.URL)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Len(t, body, 10)
	})

	t.Run("stalled", func(t *testing.T) {
		start := time.Now()
		_, err := client.Get(stalled(t))
		require.Error(t, err)
		require.Less(t, time.Since(start), 5*time.Second)
	})

	// Other defaults modes have the SDK replace the dialer the timeout is on.
	t.Run("S3 in standard defaults mode", func(t *testing.T) {
		t.Setenv("AWS_DEFAULTS_MODE", "standard")
		s, err := NewS3(&S3Config{AccessKey: "key", Secret: "secret", Region: "us-east-1", Endpoint: stalled(t), ForcePathStyle: true, Bucket: "bucket"})
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err = s.ObjectExists(ctx, "path")
		require.Error(t, err)
		require.NoError(t, ctx.Err(), "the context, not the idle timeout, ended the call")
	})
}

// A deadline set from above, as TLS sets one to send its close alert, holds:
// reads and writes do not push it out to the idle timeout.
func TestIdleConnKeepsDeadlines(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	c := &idleConn{Conn: client, timeout: time.Hour}

	require.NoError(t, c.SetDeadline(time.Now().Add(100*time.Millisecond)))
	start := time.Now()
	_, err := c.Write([]byte("x")) // nobody reads the other end
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	_, err = c.Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	require.Less(t, time.Since(start), 5*time.Second)
}

// A connection that goes silent without closing held every request HTTP/2
// shares it with until the idle timeout, which each new request restarted.
// The S3 client pings a silent connection and closes it, and the next
// request dials a new one.
func TestSilentHTTP2ConnectionIsDropped(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.EnableHTTP2 = true
	l := &freezable{Listener: srv.Listener, frozen: make(chan struct{})}
	srv.Listener = l
	srv.StartTLS()
	t.Cleanup(func() { close(l.frozen); srv.Close() })

	s, err := NewS3(&S3Config{AccessKey: "key", Secret: "secret", Region: "us-east-1", Endpoint: srv.URL, ForcePathStyle: true, Bucket: "bucket"})
	require.NoError(t, err)
	tr := s.(*s3Storage).client.Options().HTTPClient.(*awshttp.BuildableClient).GetTransport()
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	client := &http.Client{Transport: tr}
	get := func() error {
		resp, err := client.Get(srv.URL)
		if err != nil {
			return err
		}
		resp.Body.Close()
		require.Equal(t, 2, resp.ProtoMajor)
		return nil
	}

	require.NoError(t, get())
	l.freeze()
	start := time.Now()
	for range 3 {
		_ = get()
	}
	require.Less(t, time.Since(start), 2*healthCheck+pingTimeout, "requests held past the health check")
	// Once the silent connection is closed, a request dials a new one.
	require.NoError(t, get())
}

// freezable is a listener whose accepted connections stop reading once
// frozen, as a peer gone silent without closing.
type freezable struct {
	net.Listener
	frozen chan struct{}
	mu     sync.Mutex
	conns  []*freezableConn
}

type freezableConn struct {
	net.Conn
	frozen atomic.Pointer[chan struct{}]
}

func (c *freezableConn) Read(p []byte) (int, error) {
	if f := c.frozen.Load(); f != nil {
		<-*f
	}
	return c.Conn.Read(p)
}

func (l *freezable) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	fc := &freezableConn{Conn: c}
	l.mu.Lock()
	l.conns = append(l.conns, fc)
	l.mu.Unlock()
	return fc, nil
}

// freeze stops every connection accepted so far.
func (l *freezable) freeze() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		c.frozen.Store(&l.frozen)
	}
}

// stalled is a server that accepts requests and never answers them.
func stalled(t *testing.T) string {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-done
	}))
	t.Cleanup(func() { close(done); srv.Close() })
	return srv.URL
}

// trickle yields n bytes, one every 100ms, a tenth of the idle timeout.
type trickle struct {
	n     int
	flush http.Flusher
}

func (r *trickle) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	if r.flush != nil {
		r.flush.Flush()
	}
	time.Sleep(100 * time.Millisecond)
	r.n--
	p[0] = 'x'
	return 1, nil
}
