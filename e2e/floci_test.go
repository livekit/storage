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

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"

	"github.com/livekit/storage"
)

// The emulators run with the tests on any machine with Docker, and must in CI.
// Each holds its state in memory and is removed when its test ends.
// Cleanups take a context of their own: the test's is canceled by then.

const flociBucket = "livekit"

func TestFlociS3(t *testing.T) {
	endpoint := floci(t, "floci/floci:2.3.0@sha256:e0b292df4dc9559b7b1bf4f51bdda4bb8e269276236438cd03cae661260ea8a9", "4566")
	conf := &storage.S3Config{
		AccessKey:      "test",
		Secret:         "test",
		Region:         "us-east-1",
		Endpoint:       endpoint,
		ForcePathStyle: true,
		Bucket:         flociBucket,
	}
	admin := s3.New(s3.Options{
		Region:       conf.Region,
		BaseEndpoint: &conf.Endpoint,
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(conf.AccessKey, conf.Secret, ""),
	})
	_, err := admin.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(flociBucket)})
	require.NoError(t, err)

	testProvider(t, endpoint, func(endpoint string) storage.Config {
		c := *conf
		c.Endpoint = endpoint
		return &c
	}, objectsSuite{bulk: true, multipartIfAbsentRaces: true, commit: s3Commit})
}

// flociAzureKey is Azurite's well-known development key; floci-az accepts any.
const flociAzureKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

func TestFlociAzure(t *testing.T) {
	// An IP host: the SDK parses an account from the path only then.
	endpoint := strings.Replace(floci(t, "floci/floci-az:0.15.0@sha256:530635d603223e120adbf9221a97560cffc6991d65fa0c72d0f79d1097974c89", "4577"), "localhost", "127.0.0.1", 1)
	// floci-az gives devstoreaccount1 a hierarchical namespace (ADLS Gen2),
	// where a path's directories are entries of their own, and other accounts
	// a flat one: both are common on Azure.
	for name, account := range map[string]string{"hierarchical namespace": "devstoreaccount1", "flat namespace": "flat"} {
		t.Run(name, func(t *testing.T) {
			conf := &storage.AzureConfig{
				AccountName:   account,
				AccountKey:    flociAzureKey,
				ContainerName: flociBucket,
				Endpoint:      endpoint + "/" + account + "/",
			}
			cred, err := azblob.NewSharedKeyCredential(conf.AccountName, conf.AccountKey)
			require.NoError(t, err)
			admin, err := azblob.NewClientWithSharedKeyCredential(conf.Endpoint, cred, nil)
			require.NoError(t, err)
			_, err = admin.CreateContainer(t.Context(), flociBucket, nil)
			require.NoError(t, err)

			testProvider(t, endpoint, func(endpoint string) storage.Config {
				c := *conf
				c.Endpoint = endpoint + "/" + account + "/"
				return &c
			}, objectsSuite{bulk: true, multipartIgnoresIfAbsent: true, commit: func(r *http.Request) (bool, bool) {
				return r.Method == http.MethodPut && r.URL.Query().Get("comp") == "blocklist", r.Header.Get("If-None-Match") == "*"
			}})
		})
	}
}

func TestFlociGCP(t *testing.T) {
	endpoint := floci(t, "floci/floci-gcp:0.11.0@sha256:eae9683d806ceb643bcb36aee97cdb8cad13c3e2959cd053bf40e481994fde12", "4588")
	conf := &storage.GCPConfig{
		// A service account of its own, to sign URLs.
		CredentialsJSON: gcpServiceAccount(t),
		Bucket:          flociBucket,
		Endpoint:        endpoint,
	}
	admin, err := gcs.NewClient(t.Context(), option.WithEndpoint(endpoint+"/storage/v1/"), option.WithoutAuthentication())
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	require.NoError(t, admin.Bucket(flociBucket).Create(t.Context(), "floci-local", nil))

	testProvider(t, endpoint, func(endpoint string) storage.Config {
		c := *conf
		c.Endpoint = endpoint
		return &c
	}, objectsSuite{bulk: true, commit: func(r *http.Request) (bool, bool) {
		// A resumable upload takes its condition at the request that starts it,
		// the one without an upload_id.
		q := r.URL.Query()
		return r.Method == http.MethodPost && q.Get("uploadType") == "resumable" && !q.Has("upload_id"), q.Get("ifGenerationMatch") == "0"
	}})
}

// flociTenancy is floci-oci's tenancy, which is also the root compartment.
const flociTenancy = "ocid1.tenancy.oc1..flocilocaltenancy0000000000000000000000000000000000000000"

func TestFlociOCI(t *testing.T) {
	endpoint := floci(t, "floci/floci-oci:0.5.0@sha256:3cb5af867fb2a4e8b60f8cf6fd0acf4873108f01d481c0aac23c01ca00f935ae", "4599")
	// floci-oci reads the tenancy and user from the request signature without
	// verifying it, so any key signs.
	conf := &storage.OCIConfig{
		TenancyOCID: flociTenancy,
		UserOCID:    "ocid1.user.oc1..livekit",
		Fingerprint: "00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00",
		PrivateKey:  rsaKeyPEM(t),
		Region:      "us-ashburn-1",
		Endpoint:    endpoint,
		Bucket:      flociBucket,
	}
	provider := common.NewRawConfigurationProvider(conf.TenancyOCID, conf.UserOCID, conf.Region, conf.Fingerprint, conf.PrivateKey, nil)
	admin, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(provider)
	require.NoError(t, err)
	admin.Host = endpoint
	ns, err := admin.GetNamespace(t.Context(), objectstorage.GetNamespaceRequest{})
	require.NoError(t, err)
	_, err = admin.CreateBucket(t.Context(), objectstorage.CreateBucketRequest{
		NamespaceName: ns.Value,
		CreateBucketDetails: objectstorage.CreateBucketDetails{
			Name:          common.String(flociBucket),
			CompartmentId: common.String(flociTenancy),
		},
	})
	require.NoError(t, err)

	testProvider(t, endpoint, func(endpoint string) storage.Config {
		c := *conf
		c.Endpoint = endpoint
		// The namespace lookup is a read, so a write-only deployment names it.
		c.Namespace = *ns.Value
		return &c
	}, objectsSuite{bulk: true, singlePartIfAbsentRaces: true, multipartIgnoresIfAbsent: true, commit: func(r *http.Request) (bool, bool) {
		return r.Method == http.MethodPost && r.URL.Query().Has("uploadId"), r.Header.Get("If-None-Match") == "*"
	}})
}

// testProvider runs every suite against the store at endpoint. at gives the
// provider's config for an endpoint, so the suites can reach the store through
// proxies that refuse some requests.
func testProvider(t *testing.T, endpoint string, at func(endpoint string) storage.Config, suite objectsSuite) {
	s, err := storage.New(at(endpoint))
	require.NoError(t, err)
	writeOnly, err := storage.New(at(refuse(t, endpoint, isRead)))
	require.NoError(t, err)
	noDeletes, err := storage.New(at(refuse(t, endpoint, isDelete)))
	require.NoError(t, err)
	t.Run("Storage", func(t *testing.T) { testStorage(t, s) })
	t.Run("Bucket", func(t *testing.T) { testObjectsWith(t, s, suite) })
	t.Run("write only", func(t *testing.T) { testWriteOnly(t, s, writeOnly, suite) })
	t.Run("deletes refused", func(t *testing.T) { testDeletesRefused(t, s, noDeletes) })
	if suite.commit != nil {
		t.Run("multipart IfAbsent on the wire", func(t *testing.T) {
			testConditionalCommit(t, s, endpoint, at, suite.commit)
		})
	}
}

func s3Commit(r *http.Request) (bool, bool) {
	return r.Method == http.MethodPost && r.URL.Query().Has("uploadId"), r.Header.Get("If-None-Match") == "*"
}

// testConditionalCommit uploads a multipart object IfAbsent through a proxy
// that sees every request: the one that makes the object must carry the
// condition, whether or not the store enforces it.
func testConditionalCommit(t *testing.T, s storage.Storage, endpoint string, at func(string) storage.Config, commit func(*http.Request) (bool, bool)) {
	ctx := t.Context()
	var commits, conditional atomic.Int32
	watched, err := storage.New(at(refuse(t, endpoint, func(r *http.Request) bool {
		if isCommit, isConditional := commit(r); isCommit {
			commits.Add(1)
			if isConditional {
				conditional.Add(1)
			}
		}
		return false
	})))
	require.NoError(t, err)

	path := fmt.Sprintf("conditional-commit-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = s.DeleteObjects(context.Background(), []string{path}) })
	body := bytes.Repeat([]byte("0123456789abcdef"), (17<<20)/16)
	require.NoError(t, watched.UploadObject(ctx, path, bytes.NewReader(body), storage.PutOptions{IfAbsent: true}))
	require.Positive(t, commits.Load(), "no commit request seen")
	require.Equal(t, commits.Load(), conditional.Load(), "a commit went out without the condition")
}

// testDeletesRefused deletes where the store refuses it: the object is still
// there, so the call must fail, missing keys alongside it or not.
func testDeletesRefused(t *testing.T, s, noDeletes storage.Storage) {
	ctx := t.Context()
	path := fmt.Sprintf("deletes-refused-%d", time.Now().UnixNano())
	require.NoError(t, s.PutObject(ctx, path, []byte("kept"), storage.PutOptions{}))
	t.Cleanup(func() { _ = s.DeleteObjects(context.Background(), []string{path}) })

	require.Error(t, noDeletes.DeleteObjects(ctx, []string{path + "-missing", path}))
	exists, err := s.ObjectExists(ctx, path)
	require.NoError(t, err)
	require.True(t, exists)
}

// testWriteOnly writes IfAbsent with credentials that may write but not read:
// any check before uploading fails, and the store's condition alone refuses
// the second upload.
func testWriteOnly(t *testing.T, s, writeOnly storage.Storage, suite objectsSuite) {
	ctx := t.Context()
	prefix := fmt.Sprintf("write-only-%d/", time.Now().UnixNano())
	bodies := map[string][]byte{"single part": []byte("written without reads")}
	if !suite.multipartIgnoresIfAbsent {
		bodies["multipart"] = bytes.Repeat([]byte("0123456789abcdef"), (17<<20)/16)
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			path := prefix + name
			t.Cleanup(func() { _ = s.DeleteObjects(context.Background(), []string{path}) })
			require.NoError(t, writeOnly.UploadObject(ctx, path, bytes.NewReader(body), storage.PutOptions{IfAbsent: true}))
			err := writeOnly.UploadObject(ctx, path, bytes.NewReader(body), storage.PutOptions{IfAbsent: true})
			require.ErrorIs(t, err, storage.ErrExists)
			_, err = writeOnly.GetObject(ctx, path)
			require.Error(t, err, "reads must be denied")
			got, err := s.GetObject(ctx, path)
			require.NoError(t, err)
			require.Equal(t, body, got)
		})
	}
}

func isRead(r *http.Request) bool {
	return r.Method == http.MethodGet || r.Method == http.MethodHead
}

// isDelete is a single delete, or an S3 or AliOSS bulk one (POST ?delete).
func isDelete(r *http.Request) bool {
	return r.Method == http.MethodDelete || (r.Method == http.MethodPost && r.URL.Query().Has("delete"))
}

// refuse is a proxy to endpoint that answers the requests matched with 403,
// like a bucket policy that withholds that permission. It sees every request.
func refuse(t *testing.T, endpoint string, matched func(*http.Request) bool) string {
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if matched(r) {
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// floci starts image, repo:tag@digest, and returns its http:// endpoint once
// it answers. A tag pushed again fails the test rather than change what it
// runs. It skips the test where Docker is unavailable, except in CI.
func floci(t *testing.T, image, port string) string {
	ref, digest, _ := strings.Cut(image, "@")
	repo, tag, _ := strings.Cut(ref, ":")
	pool, err := dockertest.NewPool("")
	if err == nil {
		err = pool.Client.Ping()
	}
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("docker: %v", err)
		}
		t.Skipf("docker unavailable: %v", err)
	}

	res, err := pool.RunWithOptions(&dockertest.RunOptions{Repository: repo, Tag: tag}, func(hc *docker.HostConfig) {
		hc.AutoRemove = true
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Purge(res) })
	img, err := pool.Client.InspectImage(ref)
	require.NoError(t, err)
	require.Contains(t, img.RepoDigests, repo+"@"+digest, "%s is not the image pinned", ref)

	endpoint := "http://" + res.GetHostPort(port+"/tcp")
	require.NoError(t, pool.Retry(func() error {
		resp, err := pool.Client.HTTPClient.Get(endpoint)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}))
	return endpoint
}

func rsaKeyPEM(t *testing.T) string {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func gcpServiceAccount(t *testing.T) string {
	b, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   "floci-local",
		"client_email": "livekit@floci-local.iam.gserviceaccount.com",
		"private_key":  rsaKeyPEM(t),
		"token_uri":    "https://oauth2.googleapis.com/token",
	})
	require.NoError(t, err)
	return string(b)
}
