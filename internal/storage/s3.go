package storage

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/xuhang113/urag/internal/model"
)

const (
	metaPrefix     = "urag-meta/v1/"
	metaCollection = "collection-id"
	metaIdem       = "idempotency-key"
	metaChecksum   = "checksum"
)

// s3Store 把 Store 接到自定义端点上的对象存储。
// 厂商客户端只留在这个未导出类型里。
type s3Store struct {
	client *s3.Client
	bucket string
	cfg    S3Config
}

func newS3Store(cfg S3Config) (Store, error) {
	if err := validateS3Config(cfg); err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(aws.Config{
		Region:           cfg.Region,
		Credentials:      credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		RetryMaxAttempts: 1,
		Retryer: func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = 1
			})
		},
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = cfg.PathStyle
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &s3Store{client: client, bucket: cfg.Bucket, cfg: cfg}, nil
}

func (s *s3Store) Put(ctx context.Context, key model.ObjectKey, r io.Reader, opts PutOptions) (ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return ObjectMeta{}, normalizeCtx(err)
	}
	if err := validateObjectKey(key); err != nil {
		return ObjectMeta{}, err
	}
	if opts.IdempotencyKey != "" {
		meta, ok, err := s.findIdem(ctx, opts.CollectionID, opts.IdempotencyKey)
		if err != nil {
			return ObjectMeta{}, err
		}
		if ok {
			return meta, nil
		}
	}
	path, size, sum, err := spoolFile(ctx, os.TempDir(), r)
	if err != nil {
		return ObjectMeta{}, err
	}
	defer os.Remove(path)
	if opts.ExpectedChecksum != "" && sum != opts.ExpectedChecksum {
		return ObjectMeta{}, ErrChecksumMismatch
	}
	meta := ObjectMeta{
		Key:            key,
		Size:           size,
		Checksum:       sum,
		ContentType:    opts.ContentType,
		CollectionID:   opts.CollectionID,
		IdempotencyKey: opts.IdempotencyKey,
	}
	if size == 0 {
		err = retryCommitIf(ctx, s.cfg.Retry, nil, nil, func() error {
			_, putErr := s.client.PutObject(ctx, &s3.PutObjectInput{
				Bucket:        aws.String(s.bucket),
				Key:           aws.String(string(key)),
				Body:          bytes.NewReader(nil),
				ContentLength: aws.Int64(0),
				ContentType:   nonempty(meta.ContentType),
				Metadata:      userMetadata(meta),
			})
			return putErr
		}, retryableAPI)
		if err != nil {
			return ObjectMeta{}, err
		}
		if err := s.writePointers(ctx, meta); err != nil {
			return ObjectMeta{}, err
		}
		return meta, nil
	}
	uploadID, err := s.createUpload(ctx, string(key), meta)
	if err != nil {
		return ObjectMeta{}, err
	}
	abort := true
	defer func() {
		if abort {
			s.abortUpload(context.WithoutCancel(ctx), string(key), uploadID)
		}
	}()
	completed, err := s.uploadFile(ctx, string(key), uploadID, path, size)
	if err != nil {
		return ObjectMeta{}, err
	}
	err = retryCommitIf(ctx, s.cfg.Retry, nil, nil, func() error {
		return s.finishUpload(ctx, string(key), uploadID, completed)
	}, retryableAPI)
	if err != nil {
		return ObjectMeta{}, err
	}
	abort = false
	if err := s.writePointers(ctx, meta); err != nil {
		return ObjectMeta{}, err
	}
	return meta, nil
}

func (s *s3Store) Get(ctx context.Context, key model.ObjectKey) (io.ReadCloser, ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, ObjectMeta{}, normalizeCtx(err)
	}
	if err := validateObjectKey(key); err != nil {
		return nil, ObjectMeta{}, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(string(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ObjectMeta{}, ErrNotFound
		}
		return nil, ObjectMeta{}, err
	}
	meta, err := objectMeta(string(key), out.ContentLength, out.ContentType, out.Metadata)
	if err != nil {
		out.Body.Close()
		return nil, ObjectMeta{}, err
	}
	return out.Body, meta, nil
}

func (s *s3Store) Delete(ctx context.Context, key model.ObjectKey) error {
	if err := ctx.Err(); err != nil {
		return normalizeCtx(err)
	}
	meta, err := s.Stat(ctx, key)
	if err != nil {
		return err
	}
	if err := s.deleteObject(ctx, string(key)); err != nil {
		return err
	}
	if meta.IdempotencyKey != "" {
		_ = s.deleteObject(ctx, idemPointer(meta.CollectionID, meta.IdempotencyKey))
	}
	if meta.Checksum != "" {
		_ = s.deleteChecksum(ctx, meta)
	}
	return nil
}

func (s *s3Store) Stat(ctx context.Context, key model.ObjectKey) (ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return ObjectMeta{}, normalizeCtx(err)
	}
	if err := validateObjectKey(key); err != nil {
		return ObjectMeta{}, err
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(string(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return ObjectMeta{}, ErrNotFound
		}
		return ObjectMeta{}, err
	}
	return objectMeta(string(key), out.ContentLength, out.ContentType, out.Metadata)
}

func (s *s3Store) ExistsByChecksum(ctx context.Context, checksum model.Checksum) (ObjectMeta, bool, error) {
	if err := ctx.Err(); err != nil {
		return ObjectMeta{}, false, normalizeCtx(err)
	}
	if checksum == "" {
		return ObjectMeta{}, false, nil
	}
	meta, ok, err := s.readPointer(ctx, sumPointer(checksum))
	if err != nil || !ok {
		return ObjectMeta{}, false, err
	}
	if _, err := s.Stat(ctx, meta.Key); err != nil {
		if errors.Is(err, ErrNotFound) {
			_ = s.deleteObject(ctx, sumPointer(checksum))
			return ObjectMeta{}, false, nil
		}
		return ObjectMeta{}, false, err
	}
	return meta, true, nil
}

func (s *s3Store) CreateMultipart(ctx context.Context, key model.ObjectKey, opts PutOptions) (MultipartSession, error) {
	if err := ctx.Err(); err != nil {
		return MultipartSession{}, normalizeCtx(err)
	}
	if err := validateObjectKey(key); err != nil {
		return MultipartSession{}, err
	}
	stagingID, err := randomHex(16)
	if err != nil {
		return MultipartSession{}, err
	}
	staging := metaPrefix + "staging/" + stagingID
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(staging),
		ContentType: nonempty(opts.ContentType),
	})
	if err != nil {
		return MultipartSession{}, err
	}
	if out.UploadId == nil || *out.UploadId == "" {
		return MultipartSession{}, errors.New("storage: missing upload id")
	}
	rec := remoteUpload{
		UploadID:       *out.UploadId,
		Key:            string(key),
		StagingKey:     staging,
		CollectionID:   string(opts.CollectionID),
		IdempotencyKey: string(opts.IdempotencyKey),
		ContentType:    opts.ContentType,
		Expected:       string(opts.ExpectedChecksum),
	}
	if err := s.putJSON(ctx, uploadMetaKey(rec.UploadID), rec); err != nil {
		s.abortUpload(context.WithoutCancel(ctx), staging, rec.UploadID)
		return MultipartSession{}, err
	}
	return MultipartSession{
		UploadID:       rec.UploadID,
		Key:            key,
		CollectionID:   opts.CollectionID,
		IdempotencyKey: opts.IdempotencyKey,
	}, nil
}

func (s *s3Store) UploadPart(ctx context.Context, session MultipartSession, partNumber int, r io.Reader) (Part, error) {
	if err := ctx.Err(); err != nil {
		return Part{}, normalizeCtx(err)
	}
	if partNumber < 1 || partNumber > 10000 {
		return Part{}, errors.New("storage: invalid part number")
	}
	up, err := s.readUpload(ctx, session.UploadID)
	if err != nil {
		return Part{}, err
	}
	dir, err := partCacheDir(session.UploadID)
	if err != nil {
		return Part{}, err
	}
	path, size, sum, err := spoolFile(ctx, dir, r)
	if err != nil {
		return Part{}, err
	}
	final := filepath.Join(dir, fmt.Sprintf("%06d.bin", partNumber))
	if err := os.Rename(path, final); err != nil {
		os.Remove(path)
		return Part{}, err
	}
	f, err := os.Open(final)
	if err != nil {
		os.Remove(final)
		return Part{}, err
	}
	out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(up.StagingKey),
		UploadId:      aws.String(up.UploadID),
		PartNumber:    aws.Int32(int32(partNumber)),
		Body:          f,
		ContentLength: aws.Int64(size),
	}, unsignedPayload)
	f.Close()
	if err != nil {
		os.Remove(final)
		return Part{}, err
	}
	part := Part{PartNumber: partNumber, Size: size, Checksum: sum, ETag: deref(out.ETag)}
	if err := writeJSONAtomic(final+".json", part); err != nil {
		return Part{}, err
	}
	if err := s.putJSON(ctx, partMetaKey(session.UploadID, partNumber), part); err != nil {
		return Part{}, err
	}
	return part, nil
}

func (s *s3Store) ListParts(ctx context.Context, session MultipartSession) ([]Part, error) {
	if err := ctx.Err(); err != nil {
		return nil, normalizeCtx(err)
	}
	up, err := s.readUpload(ctx, session.UploadID)
	if err != nil {
		return nil, err
	}
	var marker *string
	parts := []Part{}
	for {
		out, err := s.client.ListParts(ctx, &s3.ListPartsInput{
			Bucket:           aws.String(s.bucket),
			Key:              aws.String(up.StagingKey),
			UploadId:         aws.String(up.UploadID),
			PartNumberMarker: marker,
		})
		if err != nil {
			if isNotFound(err) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		for _, item := range out.Parts {
			if item.PartNumber == nil {
				continue
			}
			num := int(*item.PartNumber)
			part := Part{PartNumber: num, Size: derefInt(item.Size), ETag: deref(item.ETag)}
			if extra, ok := s.cachedPart(ctx, session.UploadID, num); ok {
				part.Checksum = extra.Checksum
				if part.ETag == "" {
					part.ETag = extra.ETag
				}
				if part.Size == 0 {
					part.Size = extra.Size
				}
			}
			parts = append(parts, part)
		}
		if out.IsTruncated == nil || !*out.IsTruncated || out.NextPartNumberMarker == nil {
			break
		}
		marker = out.NextPartNumberMarker
	}
	sortParts(parts)
	return parts, nil
}

func (s *s3Store) CompleteMultipart(ctx context.Context, session MultipartSession, parts []Part) (ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return ObjectMeta{}, normalizeCtx(err)
	}
	up, err := s.readUpload(ctx, session.UploadID)
	if err != nil {
		return ObjectMeta{}, err
	}
	if session.Key != "" && string(session.Key) != up.Key {
		return ObjectMeta{}, errors.New("storage: multipart key mismatch")
	}
	if up.IdempotencyKey != "" {
		meta, ok, err := s.findIdem(ctx, model.CollectionID(up.CollectionID), model.IdempotencyKey(up.IdempotencyKey))
		if err != nil {
			return ObjectMeta{}, err
		}
		if ok {
			return meta, nil
		}
	}
	listed, err := s.ListParts(ctx, session)
	if err != nil {
		return ObjectMeta{}, err
	}
	selected, err := matchParts(listed, parts)
	if err != nil {
		return ObjectMeta{}, err
	}
	size, sum, err := s.hashCachedParts(ctx, session.UploadID, selected)
	if err != nil {
		return ObjectMeta{}, err
	}
	if up.Expected != "" && sum != model.Checksum(up.Expected) {
		return ObjectMeta{}, ErrChecksumMismatch
	}
	meta := ObjectMeta{
		Key:            model.ObjectKey(up.Key),
		Size:           size,
		Checksum:       sum,
		ContentType:    up.ContentType,
		CollectionID:   model.CollectionID(up.CollectionID),
		IdempotencyKey: model.IdempotencyKey(up.IdempotencyKey),
	}
	err = retryCommitIf(ctx, s.cfg.Retry, nil, nil, func() error {
		if err := s.finishUpload(ctx, up.StagingKey, up.UploadID, completedParts(selected)); err != nil {
			return err
		}
		return s.copyStaging(ctx, up.StagingKey, meta)
	}, retryableAPI)
	if err != nil {
		return ObjectMeta{}, err
	}
	if err := s.writePointers(ctx, meta); err != nil {
		return ObjectMeta{}, err
	}
	s.removeUpload(context.WithoutCancel(ctx), up)
	return meta, nil
}

func (s *s3Store) AbortMultipart(ctx context.Context, session MultipartSession) error {
	if err := ctx.Err(); err != nil {
		return normalizeCtx(err)
	}
	up, err := s.readUpload(ctx, session.UploadID)
	if err != nil {
		return err
	}
	s.removeUpload(context.WithoutCancel(ctx), up)
	return nil
}

func (s *s3Store) createUpload(ctx context.Context, key string, meta ObjectMeta) (string, error) {
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: nonempty(meta.ContentType),
		Metadata:    userMetadata(meta),
	})
	if err != nil {
		return "", err
	}
	if out.UploadId == nil || *out.UploadId == "" {
		return "", errors.New("storage: missing upload id")
	}
	return *out.UploadId, nil
}

func (s *s3Store) uploadFile(ctx context.Context, key, uploadID, path string, size int64) ([]s3types.CompletedPart, error) {
	if size == 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	chunk := size
	if s.cfg.PartSize > 0 && s.cfg.PartSize < size {
		chunk = s.cfg.PartSize
	}
	var completed []s3types.CompletedPart
	var partNumber int32
	for offset := int64(0); offset < size; {
		partNumber++
		n := chunk
		if offset+n > size {
			n = size - offset
		}
		section := io.NewSectionReader(f, offset, n)
		out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(key),
			UploadId:      aws.String(uploadID),
			PartNumber:    aws.Int32(partNumber),
			Body:          section,
			ContentLength: aws.Int64(n),
		}, unsignedPayload)
		if err != nil {
			return nil, err
		}
		completed = append(completed, s3types.CompletedPart{
			ETag:       aws.String(deref(out.ETag)),
			PartNumber: aws.Int32(partNumber),
		})
		offset += n
	}
	return completed, nil
}

func (s *s3Store) finishUpload(ctx context.Context, key, uploadID string, parts []s3types.CompletedPart) error {
	if len(parts) == 0 {
		_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(s.bucket),
			Key:      aws.String(key),
			UploadId: aws.String(uploadID),
		})
		if err != nil && !isNotFound(err) {
			return err
		}
		_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(nil),
			ContentLength: aws.Int64(0),
		})
		return err
	}
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil && isNotFound(err) {
		_, statErr := s.client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		})
		if statErr == nil {
			return nil
		}
	}
	return err
}

func (s *s3Store) copyStaging(ctx context.Context, staging string, meta ObjectMeta) error {
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            aws.String(s.bucket),
		Key:               aws.String(string(meta.Key)),
		CopySource:        aws.String(copySource(s.bucket, staging)),
		MetadataDirective: s3types.MetadataDirectiveReplace,
		ContentType:       nonempty(meta.ContentType),
		Metadata:          userMetadata(meta),
	})
	return err
}

func (s *s3Store) abortUpload(ctx context.Context, key, uploadID string) {
	_, _ = s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
}

func (s *s3Store) removeUpload(ctx context.Context, up remoteUpload) {
	s.abortUpload(ctx, up.StagingKey, up.UploadID)
	_ = s.deleteObject(ctx, up.StagingKey)
	_ = s.deletePrefix(ctx, uploadPrefix(up.UploadID))
	if dir, err := partCacheDir(up.UploadID); err == nil {
		os.RemoveAll(dir)
	}
}

func (s *s3Store) hashCachedParts(ctx context.Context, uploadID string, parts []Part) (int64, model.Checksum, error) {
	dir, err := partCacheDir(uploadID)
	if err != nil {
		return 0, "", err
	}
	full := sha256New()
	buf := make([]byte, CopyBufSize)
	var total int64
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return 0, "", normalizeCtx(err)
		}
		path := filepath.Join(dir, fmt.Sprintf("%06d.bin", part.PartNumber))
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return 0, "", ErrNotFound
			}
			return 0, "", err
		}
		partHash := sha256New()
		n, copyErr := copyInto(ctx, full, partHash, f, buf)
		f.Close()
		if copyErr != nil {
			return 0, "", copyErr
		}
		if part.Checksum != "" && partHash.sum() != part.Checksum {
			return 0, "", ErrChecksumMismatch
		}
		total += n
	}
	return total, full.sum(), nil
}

func (s *s3Store) findIdem(ctx context.Context, col model.CollectionID, idem model.IdempotencyKey) (ObjectMeta, bool, error) {
	if idem == "" {
		return ObjectMeta{}, false, nil
	}
	meta, ok, err := s.readPointer(ctx, idemPointer(col, idem))
	if err != nil || !ok {
		return ObjectMeta{}, false, err
	}
	if _, err := s.Stat(ctx, meta.Key); err != nil {
		if errors.Is(err, ErrNotFound) {
			_ = s.deleteObject(ctx, idemPointer(col, idem))
			return ObjectMeta{}, false, nil
		}
		return ObjectMeta{}, false, err
	}
	return meta, true, nil
}

func (s *s3Store) writePointers(ctx context.Context, meta ObjectMeta) error {
	if meta.Checksum != "" {
		if err := s.putJSON(ctx, sumPointer(meta.Checksum), meta); err != nil {
			return err
		}
	}
	if meta.IdempotencyKey != "" {
		if err := s.putJSON(ctx, idemPointer(meta.CollectionID, meta.IdempotencyKey), meta); err != nil {
			return err
		}
	}
	return nil
}

func (s *s3Store) deleteChecksum(ctx context.Context, meta ObjectMeta) error {
	pointed, ok, err := s.readPointer(ctx, sumPointer(meta.Checksum))
	if err != nil || !ok {
		return err
	}
	if pointed.Key != meta.Key {
		return nil
	}
	return s.deleteObject(ctx, sumPointer(meta.Checksum))
}

func (s *s3Store) readUpload(ctx context.Context, uploadID string) (remoteUpload, error) {
	if uploadID == "" || strings.ContainsRune(uploadID, 0) {
		return remoteUpload{}, ErrNotFound
	}
	var rec remoteUpload
	if err := s.getJSON(ctx, uploadMetaKey(uploadID), &rec); err != nil {
		return remoteUpload{}, err
	}
	if rec.UploadID != uploadID || rec.StagingKey == "" || rec.Key == "" {
		return remoteUpload{}, ErrNotFound
	}
	return rec, nil
}

func (s *s3Store) cachedPart(ctx context.Context, uploadID string, partNumber int) (Part, bool) {
	dir, err := partCacheDir(uploadID)
	if err == nil {
		var part Part
		path := filepath.Join(dir, fmt.Sprintf("%06d.bin.json", partNumber))
		if err := readJSON(path, &part); err == nil {
			return part, true
		}
	}
	var part Part
	if err := s.getJSON(ctx, partMetaKey(uploadID, partNumber), &part); err != nil {
		return Part{}, false
	}
	return part, true
}

func (s *s3Store) putJSON(ctx context.Context, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String("application/json"),
	})
	return err
}

func (s *s3Store) getJSON(ctx context.Context, key string, dest any) error {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	defer out.Body.Close()
	return json.NewDecoder(io.LimitReader(out.Body, 1<<20)).Decode(dest)
}

func (s *s3Store) deleteObject(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func (s *s3Store) deletePrefix(ctx context.Context, prefix string) error {
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return err
		}
		for _, obj := range out.Contents {
			if obj.Key == nil {
				continue
			}
			if err := s.deleteObject(ctx, *obj.Key); err != nil {
				return err
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated || out.NextContinuationToken == nil {
			return nil
		}
		token = out.NextContinuationToken
	}
}

type remoteUpload struct {
	UploadID       string `json:"upload_id"`
	Key            string `json:"key"`
	StagingKey     string `json:"staging_key"`
	CollectionID   string `json:"collection_id"`
	IdempotencyKey string `json:"idempotency_key"`
	ContentType    string `json:"content_type"`
	Expected       string `json:"expected"`
}

func validateObjectKey(key model.ObjectKey) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if strings.HasPrefix(string(key), metaPrefix) || strings.HasPrefix(string(key), "urag-meta/") {
		return errInvalidKey
	}
	return nil
}

func userMetadata(meta ObjectMeta) map[string]string {
	raw := map[string]string{}
	if meta.CollectionID != "" {
		raw[metaCollection] = hex.EncodeToString([]byte(meta.CollectionID))
	}
	if meta.IdempotencyKey != "" {
		raw[metaIdem] = hex.EncodeToString([]byte(meta.IdempotencyKey))
	}
	if meta.Checksum != "" {
		raw[metaChecksum] = string(meta.Checksum)
	}
	if len(raw) == 0 {
		return nil
	}
	return raw
}

func unsignedPayload(o *s3.Options) {
	o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
}

func objectMeta(key string, size *int64, contentType *string, raw map[string]string) (ObjectMeta, error) {
	if raw == nil {
		raw = map[string]string{}
	}
	col, err := hex.DecodeString(raw[metaCollection])
	if err != nil {
		return ObjectMeta{}, err
	}
	idem, err := hex.DecodeString(raw[metaIdem])
	if err != nil {
		return ObjectMeta{}, err
	}
	return ObjectMeta{
		Key:            model.ObjectKey(key),
		Size:           derefInt(size),
		Checksum:       model.Checksum(raw[metaChecksum]),
		ContentType:    deref(contentType),
		CollectionID:   model.CollectionID(col),
		IdempotencyKey: model.IdempotencyKey(idem),
	}, nil
}

func idemPointer(col model.CollectionID, idem model.IdempotencyKey) string {
	return metaPrefix + "idempotency/" + hex.EncodeToString([]byte(col)) + "/" + hex.EncodeToString([]byte(idem))
}

func sumPointer(sum model.Checksum) string {
	return metaPrefix + "checksum/" + string(sum)
}

func uploadMetaKey(uploadID string) string {
	return uploadPrefix(uploadID) + "meta"
}

func uploadPrefix(uploadID string) string {
	return metaPrefix + "uploads/" + hex.EncodeToString([]byte(uploadID)) + "/"
}

func partMetaKey(uploadID string, partNumber int) string {
	return uploadPrefix(uploadID) + fmt.Sprintf("parts/%06d", partNumber)
}

func partCacheDir(uploadID string) (string, error) {
	if uploadID == "" || strings.ContainsRune(uploadID, 0) {
		return "", ErrNotFound
	}
	root := filepath.Join(os.TempDir(), "urag-storage-parts")
	dir := filepath.Join(root, hex.EncodeToString([]byte(uploadID)))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := ensureWithin(root, dir); err != nil {
		return "", err
	}
	return dir, nil
}

func completedParts(parts []Part) []s3types.CompletedPart {
	out := make([]s3types.CompletedPart, 0, len(parts))
	for _, part := range parts {
		etag := part.ETag
		out = append(out, s3types.CompletedPart{
			ETag:       aws.String(etag),
			PartNumber: aws.Int32(int32(part.PartNumber)),
		})
	}
	return out
}

func sortParts(parts []Part) {
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j].PartNumber < parts[j-1].PartNumber; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
}

func copySource(bucket, key string) string {
	escaped := strings.ReplaceAll(url.PathEscape(key), "%2F", "/")
	return bucket + "/" + escaped
}

func nonempty(v string) *string {
	if v == "" {
		return nil
	}
	return aws.String(v)
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func derefInt(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func retryableAPI(err error) bool {
	if shouldRetry(err) {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "InternalError", "ServiceUnavailable", "SlowDown", "RequestTimeout", "RequestTimeTooSkewed":
			return true
		}
	}
	return false
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var noKey *s3types.NoSuchKey
	var noUpload *s3types.NoSuchUpload
	var missing *s3types.NotFound
	if errors.As(err, &noKey) || errors.As(err, &noUpload) || errors.As(err, &missing) {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NotFound", "NoSuchKey", "NoSuchUpload":
			return true
		}
	}
	var resp *smithyhttp.ResponseError
	if errors.As(err, &resp) && resp.HTTPStatusCode() == 404 {
		return true
	}
	return false
}

func (s *s3Store) readPointer(ctx context.Context, key string) (ObjectMeta, bool, error) {
	var meta ObjectMeta
	if err := s.getJSON(ctx, key, &meta); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ObjectMeta{}, false, nil
		}
		return ObjectMeta{}, false, err
	}
	return meta, true, nil
}
