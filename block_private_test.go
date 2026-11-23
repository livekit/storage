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
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/utils/netguard"
)

// blockedConfigs are a config per provider whose requests, token or role
// fetches, or proxy go to host, with BlockPrivateAddresses set.
func blockedConfigs(t *testing.T, host string) map[string]Config {
	endpoint := "http://" + host
	oci := ociTestConfig(t, endpoint)
	oci.BlockPrivateAddresses = true
	return map[string]Config{
		"S3": &S3Config{
			AccessKey: "key", Secret: "secret", Region: "us-east-1", Bucket: "bucket",
			Endpoint: endpoint, ForcePathStyle: true, BlockPrivateAddresses: true,
		},
		"S3 assume role": &S3Config{
			AccessKey: "key", Secret: "secret", Region: "us-east-1", Bucket: "bucket",
			AssumeRoleARN: "arn:aws:iam::123456789012:role/r",
			Endpoint:      endpoint, ForcePathStyle: true, BlockPrivateAddresses: true,
		},
		"S3 proxy": &S3Config{
			AccessKey: "key", Secret: "secret", Region: "us-east-1", Bucket: "bucket",
			Endpoint: "http://store.invalid", ForcePathStyle: true,
			ProxyConfig: &ProxyConfig{URL: endpoint}, BlockPrivateAddresses: true,
		},
		"GCP": &GCPConfig{Bucket: "bucket", Endpoint: endpoint, BlockPrivateAddresses: true},
		"GCP proxy": &GCPConfig{
			Bucket: "bucket", Endpoint: "http://store.invalid",
			ProxyConfig: &ProxyConfig{URL: endpoint}, BlockPrivateAddresses: true,
		},
		"GCP token": &GCPConfig{
			CredentialsJSON: testServiceAccount(t, endpoint+"/token"), Bucket: "bucket",
			Endpoint: "https://store.invalid", BlockPrivateAddresses: true,
		},
		"Azure": &AzureConfig{
			AccountName: "account", AccountKey: base64.StdEncoding.EncodeToString([]byte("key")),
			ContainerName: "container", Endpoint: endpoint + "/account/", BlockPrivateAddresses: true,
		},
		"AliOSS": &AliOSSConfig{AccessKey: "key", Secret: "secret", Endpoint: endpoint, Bucket: "bucket", BlockPrivateAddresses: true},
		"OCI":    oci,
	}
}

func TestBlockPrivateAddresses(t *testing.T) {
	var requests atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(local.Close)

	for _, host := range []string{
		strings.TrimPrefix(local.URL, "http://"),
		"169.254.169.254",
		"10.0.0.1",
		"[::1]:1",
	} {
		for name, conf := range blockedConfigs(t, host) {
			t.Run(host+"/"+name, func(t *testing.T) {
				requireBlocked(t, name, conf)
			})
		}
	}
	require.Zero(t, requests.Load(), "a request reached the loopback server")
}

// A redirect is dialed, and checked, like any other destination.
func TestBlockPrivateAddressesRedirect(t *testing.T) {
	var internal atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		internal.Add(1)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	// The redirecting server stands in for a public one.
	public := redirector.Listener.Addr().String()
	blockInternal = func(network, address string, c syscall.RawConn) error {
		if address == public {
			return nil
		}
		return netguard.BlockInternal(network, address, c)
	}
	t.Cleanup(func() { blockInternal = netguard.BlockInternal })

	for name, conf := range blockedConfigs(t, public) {
		if strings.Contains(name, "token") || strings.Contains(name, "assume role") || strings.Contains(name, "proxy") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			requireBlocked(t, name, conf)
		})
	}
	require.Zero(t, internal.Load(), "a redirect reached the internal server")
}

// requireBlocked fails unless a request is refused at once: a refusal that is
// retried runs into the deadline.
func requireBlocked(t *testing.T, name string, conf Config) {
	t.Helper()
	s, err := New(conf)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err = s.ObjectExists(ctx, "path")
		require.NoError(t, ctx.Err(), "the refusal was retried until the deadline")
	}
	if name == "GCP token" {
		// oauth2 formats the transport's error into its own.
		require.ErrorContains(t, err, netguard.ErrBlockedAddress.Error())
		return
	}
	require.ErrorIs(t, err, netguard.ErrBlockedAddress)
}

// Without the setting, the clients connect as before.
func TestBlockPrivateAddressesOff(t *testing.T) {
	var requests atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(local.Close)

	host := strings.TrimPrefix(local.URL, "http://")
	for name, conf := range blockedConfigs(t, host) {
		if strings.Contains(name, "token") {
			continue
		}
		setBlockPrivateAddresses(conf, false)
		t.Run(name, func(t *testing.T) {
			before := requests.Load()
			s, err := New(conf)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = s.ObjectExists(ctx, "path")
			require.Greater(t, requests.Load(), before)
		})
	}
}

// An S3 config without keys gets no credentials from the host's metadata
// endpoints, which the setting refuses like any other internal address.
func TestBlockPrivateAddressesS3Credentials(t *testing.T) {
	var requests atomic.Int32
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(metadata.Close)

	for name, env := range map[string]map[string]string{
		"instance metadata": {
			"AWS_EC2_METADATA_SERVICE_ENDPOINT": metadata.URL,
		},
		"container credentials": {
			"AWS_CONTAINER_CREDENTIALS_FULL_URI": metadata.URL + "/credentials",
			"AWS_EC2_METADATA_DISABLED":          "true",
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, v := range []string{
				"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE",
				"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
				"AWS_EC2_METADATA_DISABLED",
			} {
				t.Setenv(v, "")
			}
			t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
			t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
			for k, v := range env {
				t.Setenv(k, v)
			}
			conf := &S3Config{
				Region: "us-east-1", Bucket: "bucket", Endpoint: "https://store.invalid",
				ForcePathStyle: true, BlockPrivateAddresses: true,
			}

			requests.Store(0)
			requireBlocked(t, "S3 "+name, conf)
			require.Zero(t, requests.Load(), "a credential lookup reached the metadata server")

			// Without the setting, the lookup reaches it, so the case above is not vacuous.
			conf.BlockPrivateAddresses = false
			s, err := New(conf)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = s.ObjectExists(ctx, "path")
			require.NotZero(t, requests.Load(), "the credential lookup never ran")
		})
	}
}

func setBlockPrivateAddresses(conf Config, on bool) {
	switch c := conf.(type) {
	case *S3Config:
		c.BlockPrivateAddresses = on
	case *GCPConfig:
		c.BlockPrivateAddresses = on
	case *AzureConfig:
		c.BlockPrivateAddresses = on
	case *AliOSSConfig:
		c.BlockPrivateAddresses = on
	case *OCIConfig:
		c.BlockPrivateAddresses = on
	}
}

func testServiceAccount(t *testing.T, tokenURI string) string {
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
