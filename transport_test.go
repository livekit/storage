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
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
	"github.com/stretchr/testify/require"
)

// A dead HTTP/2 peer, one that keeps the connection open but answers nothing,
// not even pings, fails the request well before the idle timeout, with an
// error the GCS SDK retries. A live one that is merely slow is never cut.
func TestHTTP2Pings(t *testing.T) {
	savedIdle, savedInterval, savedTimeout := idleTimeout, h2PingInterval, h2PingTimeout
	idleTimeout, h2PingInterval, h2PingTimeout = 10*time.Second, 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { idleTimeout, h2PingInterval, h2PingTimeout = savedIdle, savedInterval, savedTimeout })

	t.Run("dead peer", func(t *testing.T) {
		// The server completes the TLS and HTTP/2 handshakes, then goes silent.
		srv, client := h2Server(t, nil, func(_ *http.Server, c *tls.Conn, _ http.Handler) {
			_, _ = c.Write([]byte{0, 0, 0, 0x4, 0, 0, 0, 0, 0}) // an empty SETTINGS frame
			_, _ = io.Copy(io.Discard, c)
		})

		start := time.Now()
		_, err := client.Get(srv.URL)
		require.ErrorContains(t, err, "http2: client connection lost")
		require.Less(t, time.Since(start), 5*time.Second)
		require.True(t, gcs.ShouldRetry(err))
	})

	t.Run("slow peer", func(t *testing.T) {
		srv, client := h2Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(w, &trickle{n: 10, flush: w.(http.Flusher)})
		}), nil)

		resp, err := client.Get(srv.URL)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 2, resp.ProtoMajor)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Len(t, body, 10)
	})
}

// h2Server is a TLS server speaking HTTP/2, and a client trusting it whose
// transport has this package's timeouts. A takeover, if given, owns each
// connection in place of the HTTP/2 server.
func h2Server(t *testing.T, h http.Handler, takeover func(*http.Server, *tls.Conn, http.Handler)) (*httptest.Server, *http.Client) {
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	if takeover != nil {
		srv.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){"h2": takeover}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: transportWithTimeouts(srv.Client().Transport)}
	return srv, client
}

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
