// Copyright 2024 LiveKit, Inc.
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
	"fmt"

	"github.com/livekit/storage/config"
)

// The config structs live in the storage/config package so that consumers which
// only embed them in their own config (and never create a Storage) don't pull
// in every provider SDK. These aliases keep the existing API.
type (
	Config       = config.Config
	AliOSSConfig = config.AliOSSConfig
	AzureConfig  = config.AzureConfig
	GCPConfig    = config.GCPConfig
	LocalConfig  = config.LocalConfig
	S3Config     = config.S3Config
	ProxyConfig  = config.ProxyConfig
)

func newStorage(conf Config) (Storage, error) {
	switch c := conf.(type) {
	case *AliOSSConfig:
		return NewAliOSS(c)
	case *AzureConfig:
		return NewAzure(c)
	case *GCPConfig:
		return NewGCP(c)
	case *LocalConfig:
		return NewLocal(c)
	case *S3Config:
		return NewS3(c)
	default:
		return nil, fmt.Errorf("unsupported storage config type %T", conf)
	}
}
