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
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
)

func wrapS3Error(err error) error {
	if err == nil {
		return nil
	}
	var sc interface{ HTTPStatusCode() int }
	if errors.As(err, &sc) {
		return &ErrorWithStatusCode{
			Err:        err,
			StatusCode: sc.HTTPStatusCode(),
		}
	}
	return err
}

const defaultBucketLocation = "us-east-1"

type s3Storage struct {
	conf   *S3Config
	client *s3.Client
}

func NewS3(conf *S3Config) (Storage, error) {
	var cp aws.CredentialsProvider

	if conf.AccessKey != "" && conf.Secret != "" {
		cp = credentials.StaticCredentialsProvider{
			Value: aws.Credentials{
				AccessKeyID:     conf.AccessKey,
				SecretAccessKey: conf.Secret,
				SessionToken:    conf.SessionToken,
			},
		}
	}

	awsConf, err := getConf(conf, cp)
	if err != nil {
		return nil, err
	}

	if conf.AssumeRoleARN != "" {
		stsSvc := sts.NewFromConfig(*awsConf)
		cp = stscreds.NewAssumeRoleProvider(stsSvc, conf.AssumeRoleARN, func(o *stscreds.AssumeRoleOptions) {
			if conf.AssumeRoleExternalID != "" {
				o.ExternalID = aws.String(conf.AssumeRoleExternalID)
			}
		})
		awsConf, err = getConf(conf, cp)
		if err != nil {
			return nil, err
		}
	}

	if conf.Region == "" && conf.Endpoint == "" {
		if err = updateRegion(awsConf, conf.Bucket); err != nil {
			return nil, err
		}
	}

	return &s3Storage{
		conf:   conf,
		client: newS3Client(awsConf, conf),
	}, nil
}

func getConf(conf *S3Config, cp aws.CredentialsProvider) (*aws.Config, error) {
	opts := func(o *config.LoadOptions) error {
		if conf.Region != "" {
			o.Region = conf.Region
		} else {
			o.Region = defaultBucketLocation
		}

		o.Credentials = cp
		o.Retryer = func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = conf.MaxRetries
				o.MaxBackoff = conf.MaxRetryDelay
				o.Retryables = append(o.Retryables, retry.RetryableHTTPStatusCode{
					Codes: map[int]struct{}{
						http.StatusConflict:        {},
						http.StatusTooManyRequests: {},
					},
				})
			})
		}

		// The SDK's own client, which keeps its pool defaults and is the only
		// kind a custom CA bundle (AWS_CA_BUNDLE) can be added to.
		setProxy, err := proxySetter(conf.ProxyConfig)
		if err != nil {
			return err
		}
		// "Legacy" is the SDK's default when AWS_DEFAULTS_MODE is unset, so
		// this pins what main ran with. Every other mode only swaps in its own
		// connect and TLS timeouts (1.1s to 30s), and does it by replacing the
		// dialer, which drops the idle timeout setTimeouts puts on it. This
		// package sets those timeouts itself, and retries with its own retryer,
		// which no mode changes.
		o.DefaultsModeOptions.Mode = aws.DefaultsModeLegacy
		// The SDK's read timeout is off: setTimeouts bounds reads and writes.
		o.HTTPClient = awshttp.NewBuildableClient().
			WithDialerOptions(setDialTimeouts).
			WithTransportOptions(setTimeouts, setProxy).
			WithReadTimeout(0)

		return nil
	}

	awsConf, err := config.LoadDefaultConfig(context.Background(), opts)
	if err != nil {
		return nil, err
	}

	if conf.Endpoint != "" {
		awsConf.BaseEndpoint = &conf.Endpoint
	}

	return &awsConf, nil
}

func updateRegion(awsConf *aws.Config, bucket string) error {
	req := &s3.GetBucketLocationInput{
		Bucket: &bucket,
	}

	resp, err := s3.NewFromConfig(*awsConf).GetBucketLocation(context.Background(), req)
	if err != nil {
		return wrapS3Error(err)
	}

	if resp.LocationConstraint != "" {
		awsConf.Region = string(resp.LocationConstraint)
	}

	return nil
}

// newS3Client is the one client a Storage sends with, so calls share its
// connections.
func newS3Client(awsConf *aws.Config, conf *S3Config) *s3.Client {
	return s3.NewFromConfig(*awsConf, func(o *s3.Options) {
		o.UsePathStyle = conf.ForcePathStyle

		if conf.Endpoint != "" {
			// needed for compatibility with non-AWS S3-compatible providers (Oracle, etc.)
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
	})
}

func (s *s3Storage) UploadFile(ctx context.Context, filepath, storagePath, contentType string) (string, int64, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	if err = s.upload(ctx, file, storagePath, contentType); err != nil {
		return "", 0, err
	}
	return s.location(storagePath), stat.Size(), nil
}

func (s *s3Storage) upload(ctx context.Context, body io.Reader, storagePath, contentType string) error {
	// The SDK's request log is kept, and written only when the upload fails.
	l := news3Logger()
	logTo := func(u *manager.Uploader) {
		u.ClientOptions = append(u.ClientOptions, func(o *s3.Options) {
			o.Logger = l
			o.ClientLogMode = aws.LogRequest | aws.LogResponse | aws.LogRetries
		})
	}
	if _, err := s.uploader(s.client, logTo).Upload(ctx, s.putInput(storagePath, body, PutOptions{ContentType: contentType})); err != nil {
		l.WriteLogs()
		return wrapS3Error(err)
	}
	return nil
}

func (s *s3Storage) uploader(client *s3.Client, opts ...func(*manager.Uploader)) *manager.Uploader {
	// Uploader forces a CRC32 ChecksumAlgorithm on multipart uploads,
	// which makes UploadPart use a trailing checksum encoded as Content-Encoding:
	// aws-chunked. Non-AWS S3-compatible providers (e.g. OCI) don't support
	// this, so clear the algorithm before any checksum middleware sees it.
	return manager.NewUploader(client, append(opts, func(u *manager.Uploader) {
		if s.conf.Endpoint != "" {
			u.ClientOptions = append(u.ClientOptions, func(o *s3.Options) {
				o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
					return stack.Initialize.Add(middleware.InitializeMiddlewareFunc(
						"ClearMultipartChecksum",
						func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
							middleware.InitializeOutput, middleware.Metadata, error,
						) {
							switch v := in.Parameters.(type) {
							case *s3.CreateMultipartUploadInput:
								v.ChecksumAlgorithm = ""
							case *s3.UploadPartInput:
								v.ChecksumAlgorithm = ""
							}
							return next.HandleInitialize(ctx, in)
						},
					), middleware.Before)
				})
			})
		}
	})...)
}

func (s *s3Storage) location(storagePath string) string {
	endpoint := "s3.amazonaws.com"
	if s.conf.Endpoint != "" {
		endpoint = s.conf.Endpoint
	}
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")

	loc := url.URL{Scheme: "https"}
	if s.conf.ForcePathStyle {
		loc.Host = endpoint
		loc.Path = path.Join(s.conf.Bucket, storagePath)
	} else {
		loc.Host = s.conf.Bucket + "." + endpoint
		loc.Path = storagePath
	}
	return loc.String()
}

func (s *s3Storage) DownloadFile(ctx context.Context, filepath, storagePath string) (int64, error) {
	file, err := os.Create(filepath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	return s.downloadRanges(ctx, file, storagePath)
}

// downloadRanges fetches the object in parallel ranges, each retried on its own.
func (s *s3Storage) downloadRanges(ctx context.Context, w io.WriterAt, storagePath string) (int64, error) {
	n, err := manager.NewDownloader(s.client).Download(ctx, w, &s3.GetObjectInput{
		Bucket: aws.String(s.conf.Bucket),
		Key:    aws.String(storagePath),
	})
	return n, wrapS3ObjectError(err)
}

const s3UploadPartSize = 16 << 20

func (s *s3Storage) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	input := s.putInput(storagePath, bytes.NewReader(data), opts)
	if opts.IfAbsent {
		input.IfNoneMatch = aws.String("*")
	}
	_, err := s.client.PutObject(ctx, input)
	return wrapS3ObjectError(err)
}

// UploadObject checks IfAbsent before uploading, which spares a refused upload
// its traffic, and in the request that completes it: the uploader copies the
// input's IfNoneMatch into PutObject or CompleteMultipartUpload.
func (s *s3Storage) UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	if opts.IfAbsent && presentBeforeUpload(ctx, s, storagePath) {
		return ErrExists
	}
	input := s.putInput(storagePath, body, opts)
	uploader := s.uploader(s.client, func(u *manager.Uploader) {
		u.PartSize = s3UploadPartSize
	})
	if opts.IfAbsent {
		input.IfNoneMatch = aws.String("*")
	}
	_, err := uploader.Upload(ctx, input)
	return wrapS3ObjectError(err)
}

// putInput is every write's request: the content type, and the metadata,
// tagging and content disposition (inline by default) the config sets.
func (s *s3Storage) putInput(storagePath string, body io.Reader, opts PutOptions) *s3.PutObjectInput {
	input := &s3.PutObjectInput{
		Bucket:             aws.String(s.conf.Bucket),
		Key:                aws.String(storagePath),
		Body:               body,
		Metadata:           s.conf.Metadata,
		ContentDisposition: aws.String(cmp.Or(s.conf.ContentDisposition, "inline")),
	}
	if opts.ContentType != "" {
		input.ContentType = aws.String(opts.ContentType)
	}
	if s.conf.Tagging != "" {
		input.Tagging = aws.String(s.conf.Tagging)
	}
	return input
}

func (s *s3Storage) GetObject(ctx context.Context, storagePath string) ([]byte, error) {
	w := manager.NewWriteAtBuffer(nil)
	if _, err := s.downloadRanges(ctx, w, storagePath); err != nil {
		return nil, err
	}
	return w.Bytes(), nil
}

func (s *s3Storage) GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("storage: invalid range offset %d length %d", offset, length)
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.conf.Bucket),
		Key:    aws.String(storagePath),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)),
	})
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidRange" {
		return []byte{}, nil
	}
	if err != nil {
		return nil, wrapS3ObjectError(err)
	}
	defer out.Body.Close()
	// A store that ignores Range answers with the whole object.
	if out.ContentRange == nil {
		return nil, fmt.Errorf("storage: %s answered without Content-Range", storagePath)
	}
	return io.ReadAll(io.LimitReader(out.Body, length))
}

func (s *s3Storage) DownloadObject(ctx context.Context, storagePath string, w io.Writer) (int64, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.conf.Bucket),
		Key:    aws.String(storagePath),
	})
	if err != nil {
		return 0, wrapS3ObjectError(err)
	}
	defer out.Body.Close()
	return io.Copy(w, out.Body)
}

func (s *s3Storage) ObjectExists(ctx context.Context, storagePath string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.conf.Bucket),
		Key:    aws.String(storagePath),
	})
	err = wrapS3ObjectError(err)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *s3Storage) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.conf.Bucket),
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
func (s *s3Storage) DeleteObjects(ctx context.Context, storagePaths []string) error {
	client := s.client
	for i := 0; i < len(storagePaths); i += 1000 {
		batch := storagePaths[i:min(i+1000, len(storagePaths))]
		objects := make([]types.ObjectIdentifier, 0, len(batch))
		for _, p := range batch {
			objects = append(objects, types.ObjectIdentifier{Key: aws.String(p)})
		}
		input := &s3.DeleteObjectsInput{
			Bucket: aws.String(s.conf.Bucket),
			Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		}
		if s.conf.Endpoint != "" {
			input.ChecksumAlgorithm = types.ChecksumAlgorithmSha256
		}
		out, err := client.DeleteObjects(ctx, input)
		if err != nil {
			return wrapS3Error(err)
		}
		// Any key the store reports that is not already gone is still there,
		// so the call fails rather than leave it behind unseen.
		var errs []error
		for _, e := range out.Errors {
			if aws.ToString(e.Code) != "NoSuchKey" {
				errs = append(errs, fmt.Errorf("storage: delete %s: %s %s", aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message)))
			}
		}
		if err := errors.Join(errs...); err != nil {
			return err
		}
	}
	return nil
}

func (s *s3Storage) PresignedURL(ctx context.Context, storagePath string, expiration time.Duration) (string, error) {
	res, err := s3.NewPresignClient(s.client).PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.conf.Bucket),
		Key:    aws.String(storagePath),
	}, s3.WithPresignExpires(expiration))
	if err != nil {
		return "", wrapS3Error(err)
	}
	return res.URL, nil
}

// wrapS3ObjectError is wrapS3Error with ErrNotFound and ErrExists
// for the codes S3-compatible stores answer them with.
func wrapS3ObjectError(err error) error {
	if err == nil {
		return nil
	}
	var noSuchKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
		return fmt.Errorf("%w: %w", ErrNotFound, wrapS3Error(err))
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return fmt.Errorf("%w: %w", ErrNotFound, wrapS3Error(err))
		case "PreconditionFailed":
			return fmt.Errorf("%w: %w", ErrExists, wrapS3Error(err))
		}
	}
	return wrapS3Error(err)
}
