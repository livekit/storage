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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
	"github.com/stretchr/testify/require"
)

func TestNewConfigProvider(t *testing.T) {
	// The workload identity branch is not covered: it reaches for a Kubernetes
	// service account token and the proxymux endpoint, so off-cluster it can
	// only fail, and asserting on that pins an SDK error string.

	t.Run("api key credentials", func(t *testing.T) {
		conf := &OCIConfig{
			TenancyOCID: "ocid1.tenancy.oc1..tenancy",
			UserOCID:    "ocid1.user.oc1..user",
			Fingerprint: "aa:bb:cc",
			PrivateKey:  "-----BEGIN PRIVATE KEY-----\nnot-a-real-key\n-----END PRIVATE KEY-----",
			Region:      "us-ashburn-1",
		}

		provider, err := newConfigProvider(conf)
		require.NoError(t, err)
		require.NotNil(t, provider)

		tenancy, err := provider.TenancyOCID()
		require.NoError(t, err)
		require.Equal(t, conf.TenancyOCID, tenancy)

		user, err := provider.UserOCID()
		require.NoError(t, err)
		require.Equal(t, conf.UserOCID, user)

		fingerprint, err := provider.KeyFingerprint()
		require.NoError(t, err)
		require.Equal(t, conf.Fingerprint, fingerprint)

		region, err := provider.Region()
		require.NoError(t, err)
		require.Equal(t, conf.Region, region)
	})

	t.Run("falls back to the default provider", func(t *testing.T) {
		// Only that a provider comes back — what it resolves to depends on the
		// ambient OCI_CLI_* env and ~/.oci/config.
		provider, err := newConfigProvider(&OCIConfig{Bucket: "mybucket"})
		require.NoError(t, err)
		require.NotNil(t, provider)
	})
}

func TestOCILocationHost(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	// A set Namespace keeps NewOCI off the network.
	s, err := NewOCI(&OCIConfig{
		TenancyOCID: "ocid1.tenancy.oc1..tenancy",
		UserOCID:    "ocid1.user.oc1..user",
		Fingerprint: "aa:bb:cc",
		PrivateKey:  string(pemKey),
		Region:      "us-chicago-1",
		Namespace:   "ns",
		Bucket:      "bucket",
	})
	require.NoError(t, err)

	u, err := url.Parse(s.(*ociStorage).location("a/b.mp4"))
	require.NoError(t, err)
	require.Equal(t, "objectstorage.us-chicago-1.oraclecloud.com", u.Host)
}

// ociTestConfig is API key credentials for endpoint. A set Namespace keeps
// NewOCI off the network.
func ociTestConfig(t *testing.T, endpoint string) *OCIConfig {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return &OCIConfig{
		TenancyOCID: "ocid1.tenancy.oc1..tenancy",
		UserOCID:    "ocid1.user.oc1..user",
		Fingerprint: "aa:bb:cc",
		PrivateKey:  string(pemKey),
		Region:      "us-chicago-1",
		Namespace:   "ns",
		Bucket:      "bucket",
		Endpoint:    endpoint,
	}
}

// A request whose connection the server closes before answering is sent
// again, where the SDK's own predicate leaves it failed.
func TestOCIRetriesClosedConnection(t *testing.T) {
	t.Run("upload", func(t *testing.T) {
		// Without the header the server writes nothing before it closes, so
		// the client reads a bare EOF.
		t.Setenv(common.UsingExpectHeaderEnvVar, "false")

		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if requests.Add(1) > 1 {
				return
			}
			if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = c.Close()
			}
		}))
		t.Cleanup(srv.Close)

		s, err := NewOCI(ociTestConfig(t, srv.URL))
		require.NoError(t, err)
		require.NoError(t, s.PutObject(context.Background(), "k", []byte("hello"), PutOptions{}))
		require.EqualValues(t, 2, requests.Load())
	})

	put := func(err error) common.OCIOperationResponse {
		return common.OCIOperationResponse{
			Error: &url.Error{Op: "Put", URL: "https://objectstorage.us-chicago-1.oraclecloud.com/n/ns/b/bucket/o/k", Err: err},
		}
	}

	t.Run("errors of a closed connection", func(t *testing.T) {
		for _, err := range []error{
			io.EOF,
			io.ErrUnexpectedEOF,
			errors.New("http: server closed idle connection"),
		} {
			r := put(err)
			require.False(t, common.DefaultShouldRetryOperation(r), "the SDK's predicate, which this package adds to: %v", err)
			require.True(t, ociShouldRetry(r), "%v", err)
		}
	})

	t.Run("unchanged otherwise", func(t *testing.T) {
		require.True(t, ociShouldRetry(put(syscall.ECONNRESET)))
		require.False(t, ociShouldRetry(put(errors.New("x509: certificate signed by unknown authority"))))
		require.False(t, ociShouldRetry(common.OCIOperationResponse{Error: &ociServiceError{status: http.StatusNotFound}}))
	})
}

// flakyTokenProvider fails its token-backed calls with failures, in turn, then
// serves them as the API key provider it wraps.
type flakyTokenProvider struct {
	common.ConfigurationProvider
	failures []error
	fetches  int
	// gate, when set, holds every fetch until it is closed.
	gate chan struct{}
}

func flakyProvider(t *testing.T, failures ...error) *flakyTokenProvider {
	keys, err := newConfigProvider(ociTestConfig(t, ""))
	require.NoError(t, err)
	return &flakyTokenProvider{ConfigurationProvider: keys, failures: failures}
}

func (p *flakyTokenProvider) fetch() error {
	if p.gate != nil {
		<-p.gate
	}
	p.fetches++
	if len(p.failures) == 0 {
		return nil
	}
	err := p.failures[0]
	p.failures = p.failures[1:]
	return err
}

func (p *flakyTokenProvider) TenancyOCID() (string, error) {
	if err := p.fetch(); err != nil {
		return "", err
	}
	return p.ConfigurationProvider.TenancyOCID()
}

func (p *flakyTokenProvider) KeyID() (string, error) {
	if err := p.fetch(); err != nil {
		return "", err
	}
	return p.ConfigurationProvider.KeyID()
}

func (p *flakyTokenProvider) PrivateRSAKey() (*rsa.PrivateKey, error) {
	if err := p.fetch(); err != nil {
		return nil, err
	}
	return p.ConfigurationProvider.PrivateRSAKey()
}

func (p *flakyTokenProvider) GetClaim(string) (interface{}, error) {
	return nil, p.fetch()
}

func (p *flakyTokenProvider) Refreshable() bool { return true }

// The SDK's token fetch errors, which carry the response's status only as text.
var (
	errOCIThrottled = errors.New("failed to renew security token: failed to get security token: failed to get a Workload Identity token. Status: 429 Too Many Requests, Message: . Please contact OKE team")
	errOCIForbidden = errors.New("failed to renew security token: failed to get security token: please ensure the cluster type is enhanced: Status: 403 Forbidden, Message: ")
)

// noOCITokenBackoff has a token fetch retried at once.
func noOCITokenBackoff(t *testing.T) {
	backoff := ociTokenBackoff
	ociTokenBackoff = 0
	t.Cleanup(func() { ociTokenBackoff = backoff })
}

// A workload identity token fetch that is throttled is made again, so a client
// is built where the SDK alone fails on the first refusal.
func TestOCIRetriesTokenFetch(t *testing.T) {
	noOCITokenBackoff(t)

	t.Run("a client is built over throttled fetches", func(t *testing.T) {
		_, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(flakyProvider(t, errOCIThrottled))
		require.Error(t, err, "the SDK's one fetch, which this package repeats")

		p := flakyProvider(t, errOCIThrottled, errOCIThrottled)
		client, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(ociTokenProvider{p})
		require.NoError(t, err)
		require.Empty(t, p.failures)
		require.True(t, client.IsRefreshableAuthType())
	})

	t.Run("gives up after its attempts", func(t *testing.T) {
		p := flakyProvider(t, slices.Repeat([]error{errOCIThrottled}, ociTokenAttempts+1)...)
		_, err := ociTokenProvider{p}.KeyID()
		require.ErrorIs(t, err, errOCIThrottled)
		require.Equal(t, ociTokenAttempts, p.fetches)
	})

	t.Run("a cluster without workload identity fails at once", func(t *testing.T) {
		p := flakyProvider(t, errOCIForbidden, errOCIForbidden)
		_, err := ociTokenProvider{p}.GetClaim("res_tenant")
		require.ErrorIs(t, err, errOCIForbidden)
		require.Equal(t, 1, p.fetches)
	})
}

// With workload identity the first call builds the client, not NewOCI: a
// build that fails is that call's failure, and the next call builds again.
func TestOCIBuildsClientOnFirstCall(t *testing.T) {
	noOCITokenBackoff(t)

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		requests.Add(1)
	}))
	t.Cleanup(srv.Close)

	// The store NewOCI makes with workload identity, over provider.
	lazy := func(provider *flakyTokenProvider) *ociStorage {
		conf := ociTestConfig(t, srv.URL)
		return &ociStorage{conf: conf, provider: ociTokenProvider{provider}, namespace: conf.Namespace}
	}
	ctx := context.Background()

	t.Run("a failed build is tried again", func(t *testing.T) {
		requests.Store(0)
		s := lazy(flakyProvider(t, slices.Repeat([]error{errOCIThrottled}, ociTokenAttempts)...))

		err := s.PutObject(ctx, "k", []byte("hello"), PutOptions{})
		require.ErrorContains(t, err, "429 Too Many Requests")
		require.Zero(t, requests.Load())

		require.NoError(t, s.PutObject(ctx, "k", []byte("hello"), PutOptions{}))
		require.EqualValues(t, 1, requests.Load())
	})

	t.Run("a call is released when its context ends", func(t *testing.T) {
		requests.Store(0)
		provider := flakyProvider(t)
		provider.gate = make(chan struct{})
		s := lazy(provider)

		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		require.ErrorIs(t, s.PutObject(short, "k", []byte("hello"), PutOptions{}), context.DeadlineExceeded)

		// The build the first call began goes on, and serves the next.
		close(provider.gate)
		require.NoError(t, s.PutObject(ctx, "k", []byte("hello"), PutOptions{}))
		require.EqualValues(t, 1, requests.Load())
	})
}

// A namespace the config leaves out is looked up by the first call, so NewOCI
// makes no request.
func TestOCILooksUpNamespaceOnFirstCall(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	requests := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		path := strings.TrimSuffix(r.URL.Path, "/")
		mu.Lock()
		seen = append(seen, r.Method+" "+path)
		mu.Unlock()
		if path == "/n" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `"ns"`)
		}
	}))
	t.Cleanup(srv.Close)

	conf := ociTestConfig(t, srv.URL)
	conf.Namespace = ""
	s, err := NewOCI(conf)
	require.NoError(t, err)
	require.Empty(t, requests())

	require.NoError(t, s.PutObject(context.Background(), "k", []byte("hello"), PutOptions{}))
	require.Equal(t, []string{"GET /n", "PUT /n/ns/b/bucket/o/k"}, requests())
}
