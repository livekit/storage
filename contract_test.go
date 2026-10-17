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
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/storage"
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
			proxied := make(chan string, 16)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proxied <- r.Host
				http.Error(w, "no", http.StatusForbidden)
			}))
			t.Cleanup(proxy.Close)
			s, err := storage.New(conf(&storage.ProxyConfig{URL: proxy.URL}))
			require.NoError(t, err)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = s.ObjectExists(ctx, "path")
			select {
			case host := <-proxied:
				require.Equal(t, "store.invalid", host)
			default:
				t.Fatal("the request went around the proxy")
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
	_, _, err = s.UploadFile(ctx, file, "file", "text/plain")
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
			} else {
				require.ErrorContains(t, err, tc.code)
			}
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
