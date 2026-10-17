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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"strings"
	"testing"

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

const flociBucket = "livekit"

func TestFlociS3(t *testing.T) {
	endpoint := floci(t, "floci/floci", "2.3.0", "4566")
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

	testProvider(t, conf)
}

// flociAzureKey is Azurite's well-known development key; floci-az accepts any.
const flociAzureKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

func TestFlociAzure(t *testing.T) {
	// An IP host: the SDK parses an account from the path only then.
	endpoint := strings.Replace(floci(t, "floci/floci-az", "0.15.0", "4577"), "localhost", "127.0.0.1", 1)
	conf := &storage.AzureConfig{
		AccountName:   "devstoreaccount1",
		AccountKey:    flociAzureKey,
		ContainerName: flociBucket,
		Endpoint:      endpoint + "/devstoreaccount1/",
	}
	cred, err := azblob.NewSharedKeyCredential(conf.AccountName, conf.AccountKey)
	require.NoError(t, err)
	admin, err := azblob.NewClientWithSharedKeyCredential(conf.Endpoint, cred, nil)
	require.NoError(t, err)
	_, err = admin.CreateContainer(t.Context(), flociBucket, nil)
	require.NoError(t, err)

	testProvider(t, conf)
}

func TestFlociGCP(t *testing.T) {
	endpoint := floci(t, "floci/floci-gcp", "0.11.0", "4588")
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

	testProvider(t, conf)
}

// flociTenancy is floci-oci's tenancy, which is also the root compartment.
const flociTenancy = "ocid1.tenancy.oc1..flocilocaltenancy0000000000000000000000000000000000000000"

func TestFlociOCI(t *testing.T) {
	endpoint := floci(t, "floci/floci-oci", "0.5.0", "4599")
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

	testProvider(t, conf)
}

func testProvider(t *testing.T, conf storage.Config) {
	s, err := storage.New(conf)
	require.NoError(t, err)
	t.Run("Storage", func(t *testing.T) { testStorage(t, s) })
	t.Run("Bucket", func(t *testing.T) { testObjects(t, s) })
}

// floci starts repo:tag and returns its http:// endpoint once it answers. It
// skips the test where Docker is unavailable, except in CI.
func floci(t *testing.T, repo, tag, port string) string {
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
