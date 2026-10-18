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

package storage_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"hash/crc64"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/storage"
	"github.com/livekit/storage/objects"
)

// These hold what a caller's environment relies on, which the emulators,
// reached with any credentials over plain HTTP, cannot show.

// A custom CA bundle, as for a self-hosted store behind a private CA, can only
// be added to the AWS SDK's own HTTP client.
func TestS3CustomCABundle(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(bundle, selfSignedCert(t), 0o600))
	t.Setenv("AWS_CA_BUNDLE", bundle)

	_, err := storage.New(&storage.S3Config{
		AccessKey: "key",
		Secret:    "secret",
		Region:    "us-east-1",
		Endpoint:  "https://127.0.0.1:1",
		Bucket:    "bucket",
	})
	require.NoError(t, err)
}

// Building a GCS client must not change the process's default transport,
// which every http.Get in the importing service uses.
func TestGCPLeavesDefaultTransport(t *testing.T) {
	orig := http.DefaultTransport
	before := orig.(*http.Transport).Clone()
	// The rest of the process reads it meanwhile: under -race, a write races.
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				_ = http.DefaultTransport
			}
		}
	}()

	_, err := storage.New(&storage.GCPConfig{
		Bucket:      "bucket",
		Endpoint:    "http://127.0.0.1:1",
		ProxyConfig: &storage.ProxyConfig{URL: "http://127.0.0.1:2", Username: "u", Password: "p"},
	})
	require.NoError(t, err)

	require.Same(t, orig, http.DefaultTransport)
	after := orig.(*http.Transport)
	require.Nil(t, after.ProxyConnectHeader)
	require.Equal(t, reflect.ValueOf(before.DialContext).Pointer(), reflect.ValueOf(after.DialContext).Pointer(), "dialer")
	require.Equal(t, reflect.ValueOf(before.Proxy).Pointer(), reflect.ValueOf(after.Proxy).Pointer(), "proxy")
}

// The proxy goes on the transport the client sends with: where it is dropped,
// traffic leaves around the proxy a network requires.
func TestProxyCarriesRequests(t *testing.T) {
	for name, conf := range map[string]func(proxy *storage.ProxyConfig) storage.Config{
		"S3": func(proxy *storage.ProxyConfig) storage.Config {
			return &storage.S3Config{
				AccessKey:      "key",
				Secret:         "secret",
				Region:         "us-east-1",
				Endpoint:       "http://store.invalid",
				ForcePathStyle: true,
				Bucket:         "bucket",
				ProxyConfig:    proxy,
			}
		},
		"GCP": func(proxy *storage.ProxyConfig) storage.Config {
			return &storage.GCPConfig{Bucket: "bucket", Endpoint: "http://store.invalid", ProxyConfig: proxy}
		},
	} {
		t.Run(name, func(t *testing.T) {
			proxied := make(chan *http.Request, 16)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proxied <- r
				http.Error(w, "no", http.StatusForbidden)
			}))
			t.Cleanup(proxy.Close)
			send := func() {
				s, err := storage.New(conf(&storage.ProxyConfig{URL: proxy.URL, Username: "user", Password: "pass"}))
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_, _ = s.ObjectExists(ctx, "path")
			}

			// The credentials reach the proxy for a plain http target too, not
			// only on CONNECT.
			send()
			select {
			case r := <-proxied:
				require.Equal(t, "store.invalid", r.Host)
				require.Equal(t, "Basic dXNlcjpwYXNz", r.Header.Get("Proxy-Authorization"))
			default:
				t.Fatal("the request went around the proxy")
			}

			// NO_PROXY is honored, as it is for the metadata services that hand
			// out instance credentials.
			t.Setenv("NO_PROXY", "store.invalid")
			send()
			select {
			case <-proxied:
				t.Fatal("a NO_PROXY host went through the proxy")
			default:
			}
		})
	}
}

// Only an http:// endpoint, an emulator's, drops credentials: a private one,
// such as Private Service Connect, is still sent a token.
func TestGCPEndpointKeepsCredentials(t *testing.T) {
	for endpoint, authenticated := range map[string]bool{
		"https://127.0.0.1:1": true,
		"http://127.0.0.1:1":  false,
	} {
		t.Run(endpoint, func(t *testing.T) {
			var tokens atomic.Int32
			tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tokens.Add(1)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"access_token":"token","token_type":"Bearer","expires_in":3600}`)
			}))
			t.Cleanup(tokenServer.Close)
			s, err := storage.New(&storage.GCPConfig{
				CredentialsJSON: serviceAccount(t, tokenServer.URL),
				Bucket:          "bucket",
				Endpoint:        endpoint,
			})
			require.NoError(t, err)

			// The token is fetched before the request is sent, which then fails;
			// GCS retries a refused connection until the context ends.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = s.ObjectExists(ctx, "path")
			require.Equal(t, authenticated, tokens.Load() > 0)
		})
	}
}

// Local objects get os.Create's mode, 0666 less the umask: egress records to
// a mounted volume a host user reads.
func TestLocalFileMode(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.New(&storage.LocalConfig{StorageDir: dir})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, s.PutObject(ctx, "put", []byte("x"), storage.PutOptions{IfAbsent: true}))
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	_, _, err = s.UploadFile(ctx, "file", file, storage.PutOptions{ContentType: "text/plain"})
	require.NoError(t, err)
	for _, name := range []string{"put", "file"} {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o666)&^umask(t), info.Mode().Perm(), name)
	}
}

// An S3 bulk delete answers 200 and lists the keys it could not delete. A key
// already gone is deleted; any other is still there and must fail the call.
func TestS3DeleteObjectsKeyErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		code    string
		deleted bool
	}{
		"already gone":  {code: "NoSuchKey", deleted: true},
		"access denied": {code: "AccessDenied"},
		"internal":      {code: "InternalError"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `<DeleteResult><Error><Key>b</Key><Code>%s</Code><Message>m</Message></Error></DeleteResult>`, tc.code)
			}))
			t.Cleanup(srv.Close)
			s, err := storage.New(&storage.S3Config{
				AccessKey:      "key",
				Secret:         "secret",
				Region:         "us-east-1",
				Endpoint:       srv.URL,
				ForcePathStyle: true,
				Bucket:         "bucket",
			})
			require.NoError(t, err)

			err = s.DeleteObjects(context.Background(), []string{"a", "b"})
			if tc.deleted {
				require.NoError(t, err)
				return
			}
			// The caller learns which keys are left, and only those.
			var failed *storage.DeleteError
			require.ErrorAs(t, err, &failed)
			require.Len(t, failed.Failed, 1)
			require.ErrorContains(t, failed.Failed["b"], tc.code)
		})
	}
}

// Local keys drop a leading "/", in listings too, as egress's paths have one;
// a StorageDir that is a symlink lists what it points to.
func TestLocalKeys(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(dir, link))
	ctx := context.Background()

	for _, storageDir := range []string{dir, link} {
		s, err := storage.New(&storage.LocalConfig{StorageDir: storageDir})
		require.NoError(t, err)
		require.NoError(t, s.PutObject(ctx, "/rec/a.mp4", []byte("x"), storage.PutOptions{}))
		for _, prefix := range []string{"", "/", "rec/", "/rec/", "/rec"} {
			keys, err := s.ListObjects(ctx, prefix)
			require.NoError(t, err, prefix)
			require.Equal(t, []string{"rec/a.mp4"}, keys, prefix)
		}
		require.NoError(t, s.DeleteObjects(ctx, []string{"/rec/a.mp4"}))
	}
}

// A delete that removes an empty directory while a listing walks it does not
// hide the other keys: an empty listing reads as no objects.
func TestLocalListingDuringDeletes(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.New(&storage.LocalConfig{StorageDir: dir})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, s.PutObject(ctx, "keep", []byte("x"), storage.PutOptions{}))

	done := make(chan struct{})
	churned := make(chan struct{})
	go func() {
		defer close(churned)
		for {
			select {
			case <-done:
				return
			default:
			}
			// What a delete's cleanup of emptied directories does.
			_ = os.MkdirAll(filepath.Join(dir, "d", "e"), 0o755)
			_ = os.RemoveAll(filepath.Join(dir, "d"))
		}
	}()
	for range 5000 {
		keys, err := s.ListObjects(ctx, "")
		require.NoError(t, err)
		require.Contains(t, keys, "keep")
	}
	close(done)
	<-churned
}

// umask is the process's, read the way os.Create applies it.
func umask(t *testing.T) os.FileMode {
	f, err := os.Create(filepath.Join(t.TempDir(), "probe"))
	require.NoError(t, err)
	defer f.Close()
	info, err := f.Stat()
	require.NoError(t, err)
	return 0o666 &^ info.Mode().Perm()
}

func serviceAccount(t *testing.T, tokenURI string) string {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	b, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "livekit@test.iam.gserviceaccount.com",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":    tokenURI,
	})
	require.NoError(t, err)
	return string(b)
}

// A multipart upload that fails when its caller's context ends is aborted
// anyway: its parts would stay billed.
func TestS3AbortsAfterDeadline(t *testing.T) {
	aborted := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			fmt.Fprint(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>k</Key><UploadId>up</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && q.Has("partNumber"):
			_, _ = io.Copy(io.Discard, r.Body)
			time.Sleep(time.Second)
		case r.Method == http.MethodDelete && q.Get("uploadId") == "up":
			aborted <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	s, err := storage.New(&storage.S3Config{AccessKey: "key", Secret: "secret", Region: "us-east-1", Endpoint: srv.URL, ForcePathStyle: true, Bucket: "bucket"})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.Error(t, s.UploadObject(ctx, "k", bytes.NewReader(make([]byte, 40<<20)), storage.PutOptions{}))
	select {
	case <-aborted:
	case <-time.After(10 * time.Second):
		t.Fatal("the upload was not aborted")
	}
}

// The OSS SDK never retries, so this package does, for calls it can send
// again: a 5xx or 429 is retried, a 403 is not.
func TestAliOSSRetries(t *testing.T) {
	var requests atomic.Int32
	failNext := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if code := failNext.Swap(0); code != 0 {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(int(code))
			fmt.Fprintf(w, `<Error><Code>Failed</Code><Message>m</Message><RequestId>r</RequestId></Error>`)
			return
		}
		if r.Method == http.MethodGet {
			fmt.Fprint(w, "hello")
		}
	}))
	t.Cleanup(srv.Close)
	s, err := storage.New(&storage.AliOSSConfig{AccessKey: "key", Secret: "secret", Endpoint: srv.URL, Bucket: "bucket"})
	require.NoError(t, err)
	ctx := context.Background()

	for _, code := range []int32{http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		requests.Store(0)
		failNext.Store(code)
		require.NoError(t, s.PutObject(ctx, "k", []byte("hello"), storage.PutOptions{}))
		require.EqualValues(t, 2, requests.Load(), "put after %d", code)

		requests.Store(0)
		failNext.Store(code)
		got, err := s.GetObject(ctx, "k")
		require.NoError(t, err)
		require.Equal(t, []byte("hello"), got)
		require.EqualValues(t, 2, requests.Load(), "get after %d", code)
	}

	requests.Store(0)
	failNext.Store(http.StatusForbidden)
	err = s.PutObject(ctx, "k", []byte("hello"), storage.PutOptions{})
	require.EqualValues(t, 1, requests.Load(), "a 403 is not retried")
	// The status is found with the SDK-free package's type.
	var status *objects.ErrorWithStatusCode
	require.ErrorAs(t, err, &status)
	require.Equal(t, http.StatusForbidden, status.StatusCode)
}

// AliOSS's DownloadFile keeps the contract: it makes the destination's
// directory, a body cut short or a CRC64 that does not match leaves the
// destination as it was and no temporary file behind.
func TestAliOSSDownloadFile(t *testing.T) {
	var mode atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := []byte("hello")
		crc := crc64.Checksum(body, crc64.MakeTable(crc64.ECMA))
		switch mode.Load() {
		case "short":
			w.Header().Set("Content-Length", "10")
			_, _ = w.Write(body)
			// Ends the response five bytes short.
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		case "bad crc":
			crc++
		}
		w.Header().Set("X-Oss-Hash-Crc64ecma", strconv.FormatUint(crc, 10))
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	s, err := storage.New(&storage.AliOSSConfig{AccessKey: "key", Secret: "secret", Endpoint: srv.URL, Bucket: "bucket"})
	require.NoError(t, err)
	ctx := context.Background()

	dir := t.TempDir()
	dst := filepath.Join(dir, "new", "dir", "file")
	mode.Store("ok")
	n, err := s.DownloadFile(ctx, "k", dst)
	require.NoError(t, err)
	require.EqualValues(t, 5, n)
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, []byte("hello"), got)

	for _, m := range []string{"short", "bad crc"} {
		mode.Store(m)
		require.NoError(t, os.WriteFile(dst, []byte("kept"), 0o600))
		_, err = s.DownloadFile(ctx, "k", dst)
		require.Error(t, err, m)
		got, err := os.ReadFile(dst)
		require.NoError(t, err)
		require.Equal(t, []byte("kept"), got, m)
		entries, err := os.ReadDir(filepath.Dir(dst))
		require.NoError(t, err)
		require.Len(t, entries, 1, "%s left a temporary file", m)
	}
}

func selfSignedCert(t *testing.T) []byte {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
