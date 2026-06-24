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
	"errors"
	"fmt"
	"net/http"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"
)

func smithyRespErr(code int) *smithyhttp.ResponseError {
	return &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: code}},
		Err:      errors.New("inner"),
	}
}

func requireStatus(t *testing.T, err error, wantStatus int, wantInner error) {
	t.Helper()
	var sce *ErrorWithStatusCode
	require.ErrorAs(t, err, &sce)
	require.Equal(t, wantStatus, sce.StatusCode)
	require.ErrorIs(t, sce.Err, wantInner)
}

type customError struct{ msg string }

func (c *customError) Error() string { return c.msg }

func TestErrorWithStatusCodeUnwrap(t *testing.T) {
	sentinel := errors.New("sentinel")
	err := &ErrorWithStatusCode{Err: sentinel, StatusCode: 418}

	t.Run("errors.Is finds wrapped sentinel", func(t *testing.T) {
		require.ErrorIs(t, err, sentinel)
	})

	t.Run("errors.Is through fmt.Errorf wrap", func(t *testing.T) {
		wrapped := fmt.Errorf("outer: %w", err)
		require.ErrorIs(t, wrapped, sentinel)
	})

	t.Run("errors.As extracts inner error type", func(t *testing.T) {
		inner := &customError{msg: "boom"}
		wrapped := &ErrorWithStatusCode{Err: inner, StatusCode: 500}
		var ce *customError
		require.ErrorAs(t, wrapped, &ce)
		require.Same(t, inner, ce)
	})

	t.Run("Unwrap returns inner error", func(t *testing.T) {
		require.Same(t, sentinel, errors.Unwrap(err))
	})
}

func TestWrapS3Error(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		require.NoError(t, wrapS3Error(nil))
	})

	t.Run("smithy ResponseError", func(t *testing.T) {
		inner := smithyRespErr(404)
		requireStatus(t, wrapS3Error(inner), 404, inner)
	})

	t.Run("aws ResponseError", func(t *testing.T) {
		inner := &awshttp.ResponseError{ResponseError: smithyRespErr(503), RequestID: "req-1"}
		requireStatus(t, wrapS3Error(inner), 503, inner)
	})

	t.Run("wrapped via fmt.Errorf", func(t *testing.T) {
		inner := smithyRespErr(403)
		wrapped := fmt.Errorf("operation failed: %w", inner)
		requireStatus(t, wrapS3Error(wrapped), 403, inner)
	})

	t.Run("plain error passes through", func(t *testing.T) {
		plain := errors.New("network down")
		got := wrapS3Error(plain)
		require.Same(t, plain, got)
		var sce *ErrorWithStatusCode
		require.False(t, errors.As(got, &sce))
	})
}

func TestWrapGCPError(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		require.NoError(t, wrapGCPError(nil))
	})

	t.Run("ErrBucketNotExist", func(t *testing.T) {
		requireStatus(t, wrapGCPError(storage.ErrBucketNotExist), http.StatusNotFound, storage.ErrBucketNotExist)
	})

	t.Run("ErrObjectNotExist", func(t *testing.T) {
		requireStatus(t, wrapGCPError(storage.ErrObjectNotExist), http.StatusNotFound, storage.ErrObjectNotExist)
	})

	t.Run("wrapped ErrObjectNotExist", func(t *testing.T) {
		wrapped := fmt.Errorf("read object: %w", storage.ErrObjectNotExist)
		requireStatus(t, wrapGCPError(wrapped), http.StatusNotFound, storage.ErrObjectNotExist)
	})

	t.Run("googleapi.Error", func(t *testing.T) {
		inner := &googleapi.Error{Code: 500, Message: "boom"}
		requireStatus(t, wrapGCPError(inner), 500, inner)
	})

	t.Run("wrapped googleapi.Error", func(t *testing.T) {
		inner := &googleapi.Error{Code: 403, Message: "forbidden"}
		wrapped := fmt.Errorf("delete failed: %w", inner)
		requireStatus(t, wrapGCPError(wrapped), 403, inner)
	})

	t.Run("plain error passes through", func(t *testing.T) {
		plain := errors.New("dial timeout")
		got := wrapGCPError(plain)
		require.Same(t, plain, got)
		var sce *ErrorWithStatusCode
		require.False(t, errors.As(got, &sce))
	})
}

func TestWrapAzureError(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		require.NoError(t, wrapAzureError(nil))
	})

	t.Run("azcore ResponseError", func(t *testing.T) {
		inner := &azcore.ResponseError{StatusCode: 409, ErrorCode: "BlobAlreadyExists"}
		requireStatus(t, wrapAzureError(inner), 409, inner)
	})

	t.Run("wrapped azcore ResponseError", func(t *testing.T) {
		inner := &azcore.ResponseError{StatusCode: 404, ErrorCode: "BlobNotFound"}
		wrapped := fmt.Errorf("download: %w", inner)
		requireStatus(t, wrapAzureError(wrapped), 404, inner)
	})

	t.Run("plain error passes through", func(t *testing.T) {
		plain := errors.New("connection reset")
		got := wrapAzureError(plain)
		require.Same(t, plain, got)
		var sce *ErrorWithStatusCode
		require.False(t, errors.As(got, &sce))
	})
}

func TestWrapAliOSSError(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		require.NoError(t, wrapAliOSSError(nil))
	})

	t.Run("ServiceError", func(t *testing.T) {
		inner := &oss.ServiceError{StatusCode: 403, Code: "AccessDenied", Message: "denied"}
		requireStatus(t, wrapAliOSSError(inner), 403, inner)
	})

	t.Run("wrapped ServiceError", func(t *testing.T) {
		inner := &oss.ServiceError{StatusCode: 404, Code: "NoSuchKey"}
		wrapped := fmt.Errorf("get: %w", inner)
		requireStatus(t, wrapAliOSSError(wrapped), 404, inner)
	})


	t.Run("plain error passes through", func(t *testing.T) {
		plain := errors.New("eof")
		got := wrapAliOSSError(plain)
		require.Same(t, plain, got)
		var sce *ErrorWithStatusCode
		require.False(t, errors.As(got, &sce))
	})
}
