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
	"testing"

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
