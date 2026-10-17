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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const s3UploadPartSize = 16 << 20

type s3Objects struct {
	s *s3Storage
}

func NewS3Objects(conf *S3Config) (ObjectStorage, error) {
	s, err := NewS3(conf)
	if err != nil {
		return nil, err
	}
	return &s3Objects{s: s.(*s3Storage)}, nil
}

func (o *s3Objects) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	input := o.putInput(storagePath, bytes.NewReader(data), opts)
	if opts.IfAbsent {
		input.IfNoneMatch = aws.String("*")
	}
	_, err := o.s.getClient(nil).PutObject(ctx, input)
	return wrapS3ObjectError(err)
}

func (o *s3Objects) UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	if opts.IfAbsent {
		exists, err := o.ObjectExists(ctx, storagePath)
		if err != nil {
			return err
		}
		if exists {
			return ErrObjectExists
		}
	}
	uploader := manager.NewUploader(o.s.getClient(nil), func(u *manager.Uploader) {
		u.PartSize = s3UploadPartSize
	})
	_, err := uploader.Upload(ctx, o.putInput(storagePath, body, opts))
	return wrapS3ObjectError(err)
}

func (o *s3Objects) putInput(storagePath string, body io.Reader, opts PutOptions) *s3.PutObjectInput {
	input := &s3.PutObjectInput{
		Bucket: aws.String(o.s.conf.Bucket),
		Key:    aws.String(storagePath),
		Body:   body,
	}
	if opts.ContentType != "" {
		input.ContentType = aws.String(opts.ContentType)
	}
	return input
}

func (o *s3Objects) GetObject(ctx context.Context, storagePath string) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := o.DownloadObject(ctx, storagePath, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (o *s3Objects) GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("storage: invalid range offset %d length %d", offset, length)
	}
	out, err := o.s.getClient(nil).GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(o.s.conf.Bucket),
		Key:    aws.String(storagePath),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)),
	})
	if err != nil {
		return nil, wrapS3ObjectError(err)
	}
	defer out.Body.Close()
	return io.ReadAll(io.LimitReader(out.Body, length))
}

func (o *s3Objects) DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error) {
	out, err := o.s.getClient(nil).GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(o.s.conf.Bucket),
		Key:    aws.String(storagePath),
	})
	if err != nil {
		return 0, wrapS3ObjectError(err)
	}
	defer out.Body.Close()
	return io.Copy(w, out.Body)
}

func (o *s3Objects) ObjectExists(ctx context.Context, storagePath string) (bool, error) {
	_, err := o.s.getClient(nil).HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(o.s.conf.Bucket),
		Key:    aws.String(storagePath),
	})
	err = wrapS3ObjectError(err)
	if errors.Is(err, ErrObjectNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (o *s3Objects) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	paginator := s3.NewListObjectsV2Paginator(o.s.getClient(nil), &s3.ListObjectsV2Input{
		Bucket: aws.String(o.s.conf.Bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, wrapS3Error(err)
		}
		for _, obj := range page.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
	}
	return keys, nil
}

// DeleteObjects fails when any key fails, even within a batch S3 accepted.
func (o *s3Objects) DeleteObjects(ctx context.Context, storagePaths []string) error {
	client := o.s.getClient(nil)
	for i := 0; i < len(storagePaths); i += 1000 {
		batch := storagePaths[i:min(i+1000, len(storagePaths))]
		objects := make([]types.ObjectIdentifier, 0, len(batch))
		for _, p := range batch {
			objects = append(objects, types.ObjectIdentifier{Key: aws.String(p)})
		}
		input := &s3.DeleteObjectsInput{
			Bucket: aws.String(o.s.conf.Bucket),
			Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		}
		if o.s.conf.Endpoint != "" {
			input.ChecksumAlgorithm = types.ChecksumAlgorithmSha256
		}
		out, err := client.DeleteObjects(ctx, input)
		if err != nil {
			return wrapS3Error(err)
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("storage: %d of %d deletes failed, first %s: %s %s",
				len(out.Errors), len(batch), aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message))
		}
	}
	return nil
}

func (o *s3Objects) GeneratePresignedUrl(ctx context.Context, storagePath string, expiration time.Duration) (string, error) {
	res, err := s3.NewPresignClient(o.s.getClient(nil)).PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(o.s.conf.Bucket),
		Key:    aws.String(storagePath),
	}, s3.WithPresignExpires(expiration))
	if err != nil {
		return "", wrapS3Error(err)
	}
	return res.URL, nil
}

// wrapS3ObjectError is wrapS3Error with ErrObjectNotFound and ErrObjectExists
// for the codes S3-compatible stores answer them with.
func wrapS3ObjectError(err error) error {
	if err == nil {
		return nil
	}
	var noSuchKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
		return fmt.Errorf("%w: %w", ErrObjectNotFound, wrapS3Error(err))
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return fmt.Errorf("%w: %w", ErrObjectNotFound, wrapS3Error(err))
		case "PreconditionFailed":
			return fmt.Errorf("%w: %w", ErrObjectExists, wrapS3Error(err))
		}
	}
	return wrapS3Error(err)
}
