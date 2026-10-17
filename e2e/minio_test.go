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
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/storage"
)

// TestMinIO runs the suites on a real S3 implementation, whose conditional
// writes are atomic where floci's are not, so the IfAbsent races run in full.
// It takes the minio binary from MINIO_BIN or PATH; CI installs it.
func TestMinIO(t *testing.T) {
	endpoint := minio(t)
	conf := &storage.S3Config{
		AccessKey:      "minioadmin",
		Secret:         "minioadmin",
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
	}, objectsSuite{bulk: true, commit: s3Commit})
}

// minio starts a MinIO server on a free port and returns its endpoint once it
// is live. It skips the test without a binary, except in CI.
func minio(t *testing.T) string {
	bin := os.Getenv("MINIO_BIN")
	if bin == "" {
		bin, _ = exec.LookPath("minio")
	}
	if bin == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("minio binary not found: set MINIO_BIN")
		}
		t.Skip("minio binary not found: set MINIO_BIN")
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := "127.0.0.1:" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	require.NoError(t, l.Close())

	cmd := exec.Command(bin, "server", t.TempDir(), "--address", addr, "--console-address", "127.0.0.1:0", "--quiet")
	cmd.Env = append(os.Environ(), "MINIO_ROOT_USER=minioadmin", "MINIO_ROOT_PASSWORD=minioadmin")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	endpoint := "http://" + addr
	require.Eventually(t, func() bool {
		resp, err := http.Get(endpoint + "/minio/health/live")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 30*time.Second, 100*time.Millisecond)
	return endpoint
}
