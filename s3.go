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
	"context"
	"encoding/base64"
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
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
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
	conf    *S3Config
	awsConf *aws.Config
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

	if conf.AssumeRoleArn != "" {
		stsSvc := sts.NewFromConfig(*awsConf)
		cp = stscreds.NewAssumeRoleProvider(stsSvc, conf.AssumeRoleArn, func(o *stscreds.AssumeRoleOptions) {
			if conf.AssumeRoleExternalId != "" {
				o.ExternalID = aws.String(conf.AssumeRoleExternalId)
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
		conf:    conf,
		awsConf: awsConf,
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

		if conf.ProxyConfig != nil {
			proxyUrl, err := url.Parse(conf.ProxyConfig.Url)
			if err != nil {
				return err
			}
			s3Transport := http.DefaultTransport.(*http.Transport).Clone()
			s3Transport.Proxy = http.ProxyURL(proxyUrl)
			if conf.ProxyConfig.Username != "" && conf.ProxyConfig.Password != "" {
				auth := fmt.Sprintf("%s:%s", conf.ProxyConfig.Username, conf.ProxyConfig.Password)
				basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(auth))
				s3Transport.ProxyConnectHeader = http.Header{}
				s3Transport.ProxyConnectHeader.Add("Proxy-Authorization", basicAuth)
			}
			o.HTTPClient = &http.Client{Transport: s3Transport}
		}

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

func (s *s3Storage) getClient(l logging.Logger) *s3.Client {
	return s3.NewFromConfig(*s.awsConf, func(o *s3.Options) {
		if l != nil {
			o.Logger = l
			o.ClientLogMode = aws.LogRequest | aws.LogResponse | aws.LogRetries
		}

		o.UsePathStyle = s.conf.ForcePathStyle

		if s.conf.Endpoint != "" {
			// needed for compatibility with non-AWS S3-compatible providers (Oracle, etc.)
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
	})
}

func (s *s3Storage) UploadData(data []byte, storagePath, contentType string) (string, int64, error) {
	location, err := s.upload(bytes.NewReader(data), storagePath, contentType)
	if err != nil {
		return "", 0, err
	}
	return location, int64(len(data)), nil
}

func (s *s3Storage) UploadFile(filepath, storagePath, contentType string) (string, int64, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		_ = file.Close()
	}()

	stat, err := file.Stat()
	if err != nil {
		return "", 0, err
	}

	location, err := s.upload(file, storagePath, contentType)
	if err != nil {
		return "", 0, err
	}

	return location, stat.Size(), nil
}

func (s *s3Storage) upload(reader io.Reader, storagePath, contentType string) (string, error) {
	l := NewS3Logger()
	client := s.getClient(l)

	input := &s3.PutObjectInput{
		Body:        reader,
		Bucket:      aws.String(s.conf.Bucket),
		ContentType: aws.String(contentType),
		Key:         aws.String(storagePath),
		Metadata:    s.conf.Metadata,
	}
	if s.conf.Tagging != "" {
		input.Tagging = &s.conf.Tagging
	}
	if s.conf.ContentDisposition != "" {
		input.ContentDisposition = &s.conf.ContentDisposition
	} else {
		contentDisposition := "inline"
		input.ContentDisposition = &contentDisposition
	}

	if _, err := s.uploader(client).Upload(context.Background(), input); err != nil {
		l.WriteLogs()
		return "", wrapS3Error(err)
	}

	return s.location(storagePath), nil
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

func (s *s3Storage) DownloadData(storagePath string) ([]byte, error) {
	w := &manager.WriteAtBuffer{}
	_, err := s.download(w, storagePath)
	if err != nil {
		return nil, err
	}

	return w.Bytes(), nil
}

func (s *s3Storage) DownloadFile(filepath, storagePath string) (int64, error) {
	file, err := os.Create(filepath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	return s.download(file, storagePath)
}

func (s *s3Storage) download(w io.WriterAt, storagePath string) (int64, error) {
	client := s.getClient(nil)

	n, err := manager.NewDownloader(client).Download(
		context.Background(),
		w,
		&s3.GetObjectInput{
			Bucket: aws.String(s.conf.Bucket),
			Key:    aws.String(storagePath),
		},
	)
	return n, wrapS3ObjectError(err)
}

func (s *s3Storage) DeleteObject(storagePath string) error {
	client := s.getClient(nil)

	_, err := client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(s.conf.Bucket),
		Key:    aws.String(storagePath),
	})
	return wrapS3Error(err)
}

const s3UploadPartSize = 16 << 20

func (s *s3Storage) PutObject(ctx context.Context, storagePath string, data []byte, opts PutOptions) error {
	input := s.putInput(storagePath, bytes.NewReader(data), opts)
	if opts.IfAbsent {
		input.IfNoneMatch = aws.String("*")
	}
	_, err := s.getClient(nil).PutObject(ctx, input)
	return wrapS3ObjectError(err)
}

// UploadObject checks IfAbsent before uploading, which spares a refused upload
// its traffic, and again when it completes.
func (s *s3Storage) UploadObject(ctx context.Context, storagePath string, body io.Reader, opts PutOptions) error {
	if opts.IfAbsent {
		exists, err := s.ObjectExists(ctx, storagePath)
		if err != nil {
			return err
		}
		if exists {
			return ErrObjectExists
		}
	}
	input := s.putInput(storagePath, body, opts)
	uploader := s.uploader(s.getClient(nil), func(u *manager.Uploader) {
		u.PartSize = s3UploadPartSize
		if opts.IfAbsent {
			// A body that fits one part goes up as this PutObject; a larger one
			// is completed with the same condition, which the uploader drops.
			input.IfNoneMatch = aws.String("*")
			u.ClientOptions = append(u.ClientOptions, completeIfAbsent)
		}
	})
	_, err := uploader.Upload(ctx, input)
	return wrapS3ObjectError(err)
}

func completeIfAbsent(o *s3.Options) {
	o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
		return stack.Initialize.Add(middleware.InitializeMiddlewareFunc("CompleteIfAbsent",
			func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
				middleware.InitializeOutput, middleware.Metadata, error,
			) {
				if v, ok := in.Parameters.(*s3.CompleteMultipartUploadInput); ok {
					v.IfNoneMatch = aws.String("*")
				}
				return next.HandleInitialize(ctx, in)
			},
		), middleware.Before)
	})
}

func (s *s3Storage) putInput(storagePath string, body io.Reader, opts PutOptions) *s3.PutObjectInput {
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.conf.Bucket),
		Key:    aws.String(storagePath),
		Body:   body,
	}
	if opts.ContentType != "" {
		input.ContentType = aws.String(opts.ContentType)
	}
	return input
}

func (s *s3Storage) GetObject(ctx context.Context, storagePath string) ([]byte, error) {
	return getObject(ctx, s, storagePath)
}

func (s *s3Storage) GetObjectRange(ctx context.Context, storagePath string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("storage: invalid range offset %d length %d", offset, length)
	}
	out, err := s.getClient(nil).GetObject(ctx, &s3.GetObjectInput{
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
	out, err := s.getClient(nil).GetObject(ctx, &s3.GetObjectInput{
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
	_, err := s.getClient(nil).HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.conf.Bucket),
		Key:    aws.String(storagePath),
	})
	err = wrapS3ObjectError(err)
	if errors.Is(err, ErrObjectNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *s3Storage) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	paginator := s3.NewListObjectsV2Paginator(s.getClient(nil), &s3.ListObjectsV2Input{
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
	client := s.getClient(nil)
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
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("storage: %d of %d deletes failed, first %s: %s %s",
				len(out.Errors), len(batch), aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message))
		}
	}
	return nil
}

func (s *s3Storage) GeneratePresignedUrl(ctx context.Context, storagePath string, expiration time.Duration) (string, error) {
	res, err := s3.NewPresignClient(s.getClient(nil)).PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.conf.Bucket),
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
