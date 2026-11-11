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
	"strconv"
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
			require.True(t, ociAlsoClosed(common.DefaultShouldRetryOperation)(r), "%v", err)
		}
	})

	t.Run("unchanged otherwise", func(t *testing.T) {
		retry := ociAlsoClosed(common.DefaultShouldRetryOperation)
		require.True(t, retry(put(syscall.ECONNRESET)))
		require.False(t, retry(put(errors.New("x509: certificate signed by unknown authority"))))
		require.False(t, retry(common.OCIOperationResponse{Error: &ociServiceError{status: http.StatusNotFound}}))
	})
}

// flakyTokenProvider fails its token-backed calls with failures, in turn, then
// serves them as the API key provider it wraps.
type flakyTokenProvider struct {
	common.ConfigurationProvider
	failures []error
	fetches  atomic.Int32
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
	p.fetches.Add(1)
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
		require.EqualValues(t, ociTokenAttempts, p.fetches.Load())
	})

	t.Run("a cluster without workload identity fails at once", func(t *testing.T) {
		p := flakyProvider(t, errOCIForbidden, errOCIForbidden)
		_, err := ociTokenProvider{p}.GetClaim("res_tenant")
		require.ErrorIs(t, err, errOCIForbidden)
		require.EqualValues(t, 1, p.fetches.Load())
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

	t.Run("a delete checks once for all its paths", func(t *testing.T) {
		provider := flakyProvider(t, slices.Repeat([]error{errOCIThrottled}, 10*ociTokenAttempts)...)
		s := lazy(provider)

		paths := make([]string, 40)
		for i := range paths {
			paths[i] = strconv.Itoa(i)
		}
		var failed *DeleteError
		require.ErrorAs(t, s.DeleteObjects(ctx, paths), &failed)
		require.Len(t, failed.Failed, len(paths))
		require.EqualValues(t, ociTokenAttempts, provider.fetches.Load())
	})

	t.Run("calls that leave start no build of their own", func(t *testing.T) {
		provider := flakyProvider(t, slices.Repeat([]error{errOCIThrottled}, 10*ociTokenAttempts)...)
		provider.gate = make(chan struct{})
		s := lazy(provider)

		errs := make([]error, 5)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Go(func() {
				short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
				errs[i] = s.PutObject(short, "k", []byte("hello"), PutOptions{})
			})
		}
		wg.Wait()
		for _, err := range errs {
			require.ErrorIs(t, err, context.DeadlineExceeded)
		}

		// The one build they shared fails after its attempts, and none follows.
		close(provider.gate)
		require.Eventually(t, func() bool { return provider.fetches.Load() == ociTokenAttempts }, 5*time.Second, 5*time.Millisecond)
		require.Never(t, func() bool { return provider.fetches.Load() > ociTokenAttempts }, 200*time.Millisecond, 10*time.Millisecond)
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

// hangUp reads the request, then closes its connection with no answer.
func hangUp(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = c.Close()
	}
}

// The policy the SDK is configured with stays in effect: retries switched off
// stay off, and a global policy keeps its attempts, with a closed connection
// among what it retries.
func TestOCIKeepsConfiguredRetries(t *testing.T) {
	t.Setenv(common.UsingExpectHeaderEnvVar, "false")

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		hangUp(w, r)
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()

	t.Run("retries switched off", func(t *testing.T) {
		t.Setenv("OCI_SDK_DEFAULT_RETRY_ENABLED", "false")
		requests.Store(0)

		s, err := NewOCI(ociTestConfig(t, srv.URL))
		require.NoError(t, err)
		require.ErrorIs(t, s.PutObject(ctx, "k", []byte("hello"), PutOptions{}), io.EOF)
		require.EqualValues(t, 1, requests.Load())
	})

	t.Run("a global policy", func(t *testing.T) {
		twice := common.NewRetryPolicy(2, common.DefaultShouldRetryOperation, func(common.OCIOperationResponse) time.Duration { return 0 })
		common.GlobalRetry = &twice
		t.Cleanup(func() { common.GlobalRetry = nil })
		requests.Store(0)

		s, err := NewOCI(ociTestConfig(t, srv.URL))
		require.NoError(t, err)
		require.ErrorIs(t, s.PutObject(ctx, "k", []byte("hello"), PutOptions{}), io.EOF)
		require.EqualValues(t, 2, requests.Load())
	})
}

// A pre-authenticated request that got no answer may have been created, so
// it is not sent again; one the server refused is.
func TestOCIPresignedURLRetriesOnlyAnswered(t *testing.T) {
	var requests atomic.Int32
	var answer atomic.Value // func(http.ResponseWriter, *http.Request, int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer.Load().(func(http.ResponseWriter, *http.Request, int32))(w, r, requests.Add(1))
	}))
	t.Cleanup(srv.Close)

	s, err := NewOCI(ociTestConfig(t, srv.URL))
	require.NoError(t, err)
	ctx := context.Background()

	t.Run("no answer", func(t *testing.T) {
		requests.Store(0)
		answer.Store(func(w http.ResponseWriter, r *http.Request, _ int32) { hangUp(w, r) })

		_, err := s.PresignedURL(ctx, "k", time.Hour)
		require.ErrorIs(t, err, io.EOF)
		require.EqualValues(t, 1, requests.Load())
	})

	t.Run("refused, then created", func(t *testing.T) {
		requests.Store(0)
		answer.Store(func(w http.ResponseWriter, r *http.Request, n int32) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"code":"ServiceUnavailable","message":"busy"}`)
				return
			}
			_, _ = io.WriteString(w, `{"accessUri":"/p/token/n/ns/b/bucket/o/k"}`)
		})

		u, err := s.PresignedURL(ctx, "k", time.Hour)
		require.NoError(t, err)
		require.Equal(t, srv.URL+"/p/token/n/ns/b/bucket/o/k", u)
		require.EqualValues(t, 2, requests.Load())
	})
}
