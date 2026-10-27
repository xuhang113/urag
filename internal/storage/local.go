package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/xuhang113/urag/internal/model"
)

// Local 是目录上的对象存储。
// Put、Get 和 UploadPart 按不超过 CopyBufSize 的缓冲流式复制，不把整个对象读进内存。
// 正文先写入临时文件。校验和与大小一致，并且提交步骤成功之后，Get 和 Stat 才能看见对象。
// 同一集合里的非空幂等键只保留第一次提交的对象；空幂等键按对象键覆盖。
// 对象键含有 ".." 或是绝对路径时拒绝写入，避免逃出目录。
type Local struct {
	root      string
	blobDir   string
	metaDir   string
	uploadDir string
	policy    RetryPolicy
	hook      Hook

	attempts atomic.Int64

	mu      sync.Mutex
	objects map[model.ObjectKey]ObjectMeta
	blobs   map[model.ObjectKey]string
	byIdem  map[string]model.ObjectKey
	bySum   map[model.Checksum][]model.ObjectKey
}

// NewLocal 打开 dir 上的本地存储，并创建需要的目录。
// policy 控制可重试错误的退避。MaxAttempts 含第一次，小于 1 时按 3 次。BaseDelay 为 0 时不休眠。
// hook 观察每一次提交，可以为空；为空时照常提交。
// dir 为空、目录无法创建，或已有元数据读不出来时返回错误。
func NewLocal(dir string, policy RetryPolicy, hook Hook) (Store, error) {
	if dir == "" {
		return nil, errors.New("storage: empty dir")
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err == nil && !info.IsDir() {
		return nil, errors.New("storage: dir is not a directory")
	}
	loc := &Local{
		root:      root,
		blobDir:   filepath.Join(root, "blobs"),
		metaDir:   filepath.Join(root, "meta"),
		uploadDir: filepath.Join(root, "uploads"),
		policy:    policy,
		hook:      hook,
		objects:   map[model.ObjectKey]ObjectMeta{},
		blobs:     map[model.ObjectKey]string{},
		byIdem:    map[string]model.ObjectKey{},
		bySum:     map[model.Checksum][]model.ObjectKey{},
	}
	for _, sub := range []string{loc.blobDir, loc.metaDir, loc.uploadDir} {
		if err := os.MkdirAll(sub, 0o700); err != nil {
			return nil, err
		}
	}
	if err := loc.load(); err != nil {
		return nil, err
	}
	return loc, nil
}

// CommitAttempts 返回已经进入提交步骤的次数，含失败的尝试。
// 校验和不一致、上下文取消、上下文截止和幂等命中都不进入提交，因此不增加。
func (s *Local) CommitAttempts() int {
	return int(s.attempts.Load())
}

func (s *Local) markAttempt() {
	s.attempts.Add(1)
}

func (s *Local) load() error {
	entries, err := os.ReadDir(s.metaDir)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var rec diskMeta
		if err := readJSON(filepath.Join(s.metaDir, entry.Name()), &rec); err != nil {
			return err
		}
		if err := validateKey(rec.Key); err != nil || !safeBase(rec.Blob) {
			return errors.New("storage: corrupt metadata")
		}
		s.indexLocked(rec.object(), rec.Blob)
	}
	return nil
}

// Put 流式写入对象。失败时对象保持不可见，临时文件会被删掉。
// 同一集合的非空幂等键再次调用时直接返回已有元数据，不再读取正文。
func (s *Local) Put(ctx context.Context, key model.ObjectKey, r io.Reader, opts PutOptions) (ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return ObjectMeta{}, normalizeCtx(err)
	}
	if err := validateKey(key); err != nil {
		return ObjectMeta{}, err
	}
	if meta, ok := s.existingIdem(opts.CollectionID, opts.IdempotencyKey); ok {
		return meta, nil
	}
	path, size, sum, err := spoolFile(ctx, s.blobDir, r)
	if err != nil {
		return ObjectMeta{}, err
	}
	kept := false
	defer func() {
		if !kept {
			os.Remove(path)
		}
	}()
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
	published, referenced, err := s.finish(ctx, meta, filepath.Base(path))
	if err != nil {
		return ObjectMeta{}, err
	}
	kept = referenced
	return published, nil
}

// Get 打开已提交对象。对象不存在时返回 ErrNotFound。调用方必须关闭 ReadCloser。
func (s *Local) Get(ctx context.Context, key model.ObjectKey) (io.ReadCloser, ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, ObjectMeta{}, normalizeCtx(err)
	}
	rec, err := s.readMeta(key)
	if err != nil {
		return nil, ObjectMeta{}, err
	}
	f, err := os.Open(s.blobPath(rec.Blob))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ObjectMeta{}, ErrNotFound
		}
		return nil, ObjectMeta{}, err
	}
	return f, rec.object(), nil
}

// Delete 删除已提交对象，并清掉它的幂等键和校验和记录。
// 对象不存在时返回 ErrNotFound。
func (s *Local) Delete(ctx context.Context, key model.ObjectKey) error {
	if err := ctx.Err(); err != nil {
		return normalizeCtx(err)
	}
	if err := validateKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.readMetaUnlocked(key)
	if err != nil {
		return err
	}
	path, err := s.metaPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	if rec.Blob != "" {
		os.Remove(s.blobPath(rec.Blob))
	}
	s.unindexLocked(rec.object())
	return nil
}

// Stat 返回已提交对象的元数据。对象不存在时返回 ErrNotFound。
func (s *Local) Stat(ctx context.Context, key model.ObjectKey) (ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return ObjectMeta{}, normalizeCtx(err)
	}
	rec, err := s.readMeta(key)
	if err != nil {
		return ObjectMeta{}, err
	}
	return rec.object(), nil
}

// ExistsByChecksum 只在已提交对象里查找。没有时返回 false 和空错误。
func (s *Local) ExistsByChecksum(ctx context.Context, checksum model.Checksum) (ObjectMeta, bool, error) {
	if err := ctx.Err(); err != nil {
		return ObjectMeta{}, false, normalizeCtx(err)
	}
	if checksum == "" {
		return ObjectMeta{}, false, nil
	}
	s.mu.Lock()
	keys := append([]model.ObjectKey(nil), s.bySum[checksum]...)
	s.mu.Unlock()
	for _, key := range keys {
		meta, err := s.Stat(ctx, key)
		if err == nil && meta.Checksum == checksum {
			return meta, true, nil
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return ObjectMeta{}, false, err
		}
	}
	return ObjectMeta{}, false, nil
}

// CreateMultipart 开始一次分片上传。提交前对象不可见。
func (s *Local) CreateMultipart(ctx context.Context, key model.ObjectKey, opts PutOptions) (MultipartSession, error) {
	if err := ctx.Err(); err != nil {
		return MultipartSession{}, normalizeCtx(err)
	}
	if err := validateKey(key); err != nil {
		return MultipartSession{}, err
	}
	id, err := randomHex(16)
	if err != nil {
		return MultipartSession{}, err
	}
	dir, err := s.uploadPath(id)
	if err != nil {
		return MultipartSession{}, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "parts"), 0o700); err != nil {
		return MultipartSession{}, err
	}
	rec := uploadMeta{
		UploadID:       id,
		Key:            key,
		CollectionID:   opts.CollectionID,
		IdempotencyKey: opts.IdempotencyKey,
		ContentType:    opts.ContentType,
		Expected:       opts.ExpectedChecksum,
	}
	if err := writeJSONAtomic(filepath.Join(dir, "meta.json"), rec); err != nil {
		os.RemoveAll(dir)
		return MultipartSession{}, err
	}
	return MultipartSession{
		UploadID:       id,
		Key:            key,
		CollectionID:   opts.CollectionID,
		IdempotencyKey: opts.IdempotencyKey,
	}, nil
}

// UploadPart 流式写入一个分片。分片号从 1 开始。成功后 ListParts 能看见它。
func (s *Local) UploadPart(ctx context.Context, session MultipartSession, partNumber int, r io.Reader) (Part, error) {
	if err := ctx.Err(); err != nil {
		return Part{}, normalizeCtx(err)
	}
	if partNumber < 1 || partNumber > 10000 {
		return Part{}, errors.New("storage: invalid part number")
	}
	dir, _, err := s.loadUpload(session)
	if err != nil {
		return Part{}, err
	}
	partDir := filepath.Join(dir, "parts")
	path, size, sum, err := spoolFile(ctx, partDir, r)
	if err != nil {
		return Part{}, err
	}
	final := filepath.Join(partDir, fmt.Sprintf("%06d.bin", partNumber))
	if err := os.Rename(path, final); err != nil {
		os.Remove(path)
		return Part{}, err
	}
	part := Part{PartNumber: partNumber, Size: size, Checksum: sum, ETag: string(sum)}
	if err := writeJSONAtomic(filepath.Join(partDir, fmt.Sprintf("%06d.json", partNumber)), part); err != nil {
		os.Remove(final)
		return Part{}, err
	}
	return part, nil
}

// ListParts 返回已经完成的分片，按分片号升序。会话不存在时返回 ErrNotFound。
func (s *Local) ListParts(ctx context.Context, session MultipartSession) ([]Part, error) {
	if err := ctx.Err(); err != nil {
		return nil, normalizeCtx(err)
	}
	dir, _, err := s.loadUpload(session)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "parts"))
	if err != nil {
		if os.IsNotExist(err) {
			return []Part{}, nil
		}
		return nil, err
	}
	parts := make([]Part, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var part Part
		path := filepath.Join(dir, "parts", entry.Name())
		if err := readJSON(path, &part); err != nil {
			return nil, err
		}
		bin := filepath.Join(dir, "parts", fmt.Sprintf("%06d.bin", part.PartNumber))
		if _, err := os.Stat(bin); err != nil {
			continue
		}
		parts = append(parts, part)
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	return parts, nil
}

// CompleteMultipart 按分片号拼回原文。校验和或大小不一致时返回 ErrChecksumMismatch，不重试。
// 失败时对象不可见，已完成分片仍然可以列出。成功后对象可见。
func (s *Local) CompleteMultipart(ctx context.Context, session MultipartSession, parts []Part) (ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return ObjectMeta{}, normalizeCtx(err)
	}
	dir, up, err := s.loadUpload(session)
	if err != nil {
		return ObjectMeta{}, err
	}
	if session.Key != "" && session.Key != up.Key {
		return ObjectMeta{}, errors.New("storage: multipart key mismatch")
	}
	if meta, ok := s.existingIdem(up.CollectionID, up.IdempotencyKey); ok {
		return meta, nil
	}
	stored, err := s.ListParts(ctx, session)
	if err != nil {
		return ObjectMeta{}, err
	}
	selected, err := matchParts(stored, parts)
	if err != nil {
		return ObjectMeta{}, err
	}
	path, size, sum, err := s.concatParts(ctx, dir, selected)
	if err != nil {
		return ObjectMeta{}, err
	}
	kept := false
	defer func() {
		if !kept {
			os.Remove(path)
		}
	}()
	if up.Expected != "" && sum != up.Expected {
		return ObjectMeta{}, ErrChecksumMismatch
	}
	var want int64
	for _, part := range selected {
		want += part.Size
	}
	if size != want {
		return ObjectMeta{}, ErrChecksumMismatch
	}
	meta := ObjectMeta{
		Key:            up.Key,
		Size:           size,
		Checksum:       sum,
		ContentType:    up.ContentType,
		CollectionID:   up.CollectionID,
		IdempotencyKey: up.IdempotencyKey,
	}
	published, referenced, err := s.finish(ctx, meta, filepath.Base(path))
	if err != nil {
		return ObjectMeta{}, err
	}
	kept = referenced
	if referenced {
		os.RemoveAll(dir)
	}
	return published, nil
}

// AbortMultipart 丢弃分片会话。会话不存在时返回 ErrNotFound。之后对象仍然不可见。
func (s *Local) AbortMultipart(ctx context.Context, session MultipartSession) error {
	if err := ctx.Err(); err != nil {
		return normalizeCtx(err)
	}
	dir, _, err := s.loadUpload(session)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return nil
}

func (s *Local) finish(ctx context.Context, meta ObjectMeta, blob string) (ObjectMeta, bool, error) {
	var published ObjectMeta
	referenced := false
	err := retryCommit(ctx, s.policy, s.hook, s.markAttempt, func() error {
		if existing, ok := s.existingIdem(meta.CollectionID, meta.IdempotencyKey); ok && existing.Key != "" {
			published = existing
			referenced = false
			return nil
		}
		got, ref, pubErr := s.publish(meta, blob)
		if pubErr != nil {
			return pubErr
		}
		published = got
		referenced = ref
		return nil
	})
	if err != nil {
		return ObjectMeta{}, false, err
	}
	return published, referenced, nil
}

func (s *Local) publish(meta ObjectMeta, blob string) (ObjectMeta, bool, error) {
	if !safeBase(blob) {
		return ObjectMeta{}, false, errInvalidKey
	}
	blobPath := s.blobPath(blob)
	if err := ensureWithin(s.root, blobPath); err != nil {
		return ObjectMeta{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if meta.IdempotencyKey != "" {
		if existing, ok := s.existingIdemLocked(meta.CollectionID, meta.IdempotencyKey); ok {
			return existing, false, nil
		}
	}
	path, err := s.metaPath(meta.Key)
	if err != nil {
		return ObjectMeta{}, false, err
	}
	var oldBlob string
	if _, ok := s.objects[meta.Key]; ok {
		oldBlob = s.blobs[meta.Key]
	}
	rec := diskMeta{
		Key:            meta.Key,
		Size:           meta.Size,
		Checksum:       meta.Checksum,
		ContentType:    meta.ContentType,
		CollectionID:   meta.CollectionID,
		IdempotencyKey: meta.IdempotencyKey,
		Blob:           blob,
	}
	if err := writeJSONAtomic(path, rec); err != nil {
		return ObjectMeta{}, false, err
	}
	s.indexLocked(meta, blob)
	if oldBlob != "" && oldBlob != blob {
		os.Remove(s.blobPath(oldBlob))
	}
	return meta, true, nil
}

func (s *Local) existingIdem(col model.CollectionID, idem model.IdempotencyKey) (ObjectMeta, bool) {
	if idem == "" {
		return ObjectMeta{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.existingIdemLocked(col, idem)
}

func (s *Local) existingIdemLocked(col model.CollectionID, idem model.IdempotencyKey) (ObjectMeta, bool) {
	key, ok := s.byIdem[idemIndex(col, idem)]
	if !ok {
		return ObjectMeta{}, false
	}
	meta, ok := s.objects[key]
	return meta, ok
}

func (s *Local) indexLocked(meta ObjectMeta, blob string) {
	if old, ok := s.objects[meta.Key]; ok {
		s.unindexLocked(old)
	}
	s.objects[meta.Key] = meta
	s.blobs[meta.Key] = blob
	if meta.IdempotencyKey != "" {
		s.byIdem[idemIndex(meta.CollectionID, meta.IdempotencyKey)] = meta.Key
	}
	if meta.Checksum != "" {
		s.bySum[meta.Checksum] = append(s.bySum[meta.Checksum], meta.Key)
	}
}

func (s *Local) unindexLocked(meta ObjectMeta) {
	delete(s.objects, meta.Key)
	delete(s.blobs, meta.Key)
	if meta.IdempotencyKey != "" {
		id := idemIndex(meta.CollectionID, meta.IdempotencyKey)
		if s.byIdem[id] == meta.Key {
			delete(s.byIdem, id)
		}
	}
	keys := s.bySum[meta.Checksum]
	next := make([]model.ObjectKey, 0, len(keys))
	for _, key := range keys {
		if key != meta.Key {
			next = append(next, key)
		}
	}
	if len(next) == 0 {
		delete(s.bySum, meta.Checksum)
	} else {
		s.bySum[meta.Checksum] = next
	}
}

func (s *Local) readMeta(key model.ObjectKey) (diskMeta, error) {
	if err := validateKey(key); err != nil {
		return diskMeta{}, err
	}
	path, err := s.metaPath(key)
	if err != nil {
		return diskMeta{}, err
	}
	var rec diskMeta
	if err := readJSON(path, &rec); err != nil {
		return diskMeta{}, err
	}
	if rec.Key != key || !safeBase(rec.Blob) {
		return diskMeta{}, ErrNotFound
	}
	return rec, nil
}

func (s *Local) readMetaUnlocked(key model.ObjectKey) (diskMeta, error) {
	return s.readMeta(key)
}

func (s *Local) metaPath(key model.ObjectKey) (string, error) {
	path := filepath.Join(s.metaDir, metaFileName(key))
	if err := ensureWithin(s.root, path); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Local) blobPath(name string) string {
	return filepath.Join(s.blobDir, name)
}

func (s *Local) uploadPath(id string) (string, error) {
	if !validUploadID(id) {
		return "", ErrNotFound
	}
	path := filepath.Join(s.uploadDir, id)
	if err := ensureWithin(s.root, path); err != nil {
		return "", ErrNotFound
	}
	return path, nil
}

func (s *Local) loadUpload(session MultipartSession) (string, uploadMeta, error) {
	dir, err := s.uploadPath(session.UploadID)
	if err != nil {
		return "", uploadMeta{}, err
	}
	var rec uploadMeta
	if err := readJSON(filepath.Join(dir, "meta.json"), &rec); err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", uploadMeta{}, ErrNotFound
		}
		return "", uploadMeta{}, err
	}
	if rec.UploadID != session.UploadID {
		return "", uploadMeta{}, ErrNotFound
	}
	return dir, rec, nil
}

func (s *Local) concatParts(ctx context.Context, uploadDir string, parts []Part) (string, int64, model.Checksum, error) {
	f, err := os.CreateTemp(s.blobDir, ".tmp-*")
	if err != nil {
		return "", 0, "", err
	}
	name := f.Name()
	if err := ensureWithin(s.root, name); err != nil {
		f.Close()
		os.Remove(name)
		return "", 0, "", err
	}
	remove := true
	defer func() {
		if remove {
			f.Close()
			os.Remove(name)
		}
	}()
	buf := make([]byte, CopyBufSize)
	hash := sha256New()
	var total int64
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return "", 0, "", normalizeCtx(err)
		}
		bin := filepath.Join(uploadDir, "parts", fmt.Sprintf("%06d.bin", part.PartNumber))
		if err := ensureWithin(s.root, bin); err != nil {
			return "", 0, "", err
		}
		in, err := os.Open(bin)
		if err != nil {
			if os.IsNotExist(err) {
				return "", 0, "", ErrNotFound
			}
			return "", 0, "", err
		}
		partHash := sha256New()
		n, sumErr := copyInto(ctx, f, io.MultiWriter(hash, partHash), in, buf)
		in.Close()
		if sumErr != nil {
			return "", 0, "", sumErr
		}
		if part.Checksum != "" && partHash.sum() != part.Checksum {
			return "", 0, "", ErrChecksumMismatch
		}
		total += n
	}
	if err := f.Sync(); err != nil {
		return "", 0, "", err
	}
	if err := f.Close(); err != nil {
		return "", 0, "", err
	}
	remove = false
	return name, total, hash.sum(), nil
}

type diskMeta struct {
	Key            model.ObjectKey      `json:"key"`
	Size           int64                `json:"size"`
	Checksum       model.Checksum       `json:"checksum"`
	ContentType    string               `json:"content_type"`
	CollectionID   model.CollectionID   `json:"collection_id"`
	IdempotencyKey model.IdempotencyKey `json:"idempotency_key"`
	Blob           string               `json:"blob"`
}

func (d diskMeta) object() ObjectMeta {
	return ObjectMeta{
		Key:            d.Key,
		Size:           d.Size,
		Checksum:       d.Checksum,
		ContentType:    d.ContentType,
		CollectionID:   d.CollectionID,
		IdempotencyKey: d.IdempotencyKey,
	}
}

type uploadMeta struct {
	UploadID       string               `json:"upload_id"`
	Key            model.ObjectKey      `json:"key"`
	CollectionID   model.CollectionID   `json:"collection_id"`
	IdempotencyKey model.IdempotencyKey `json:"idempotency_key"`
	ContentType    string               `json:"content_type"`
	Expected       model.Checksum       `json:"expected"`
}

func validUploadID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func matchParts(stored, requested []Part) ([]Part, error) {
	if len(requested) == 0 {
		return nil, errors.New("storage: no parts")
	}
	byNum := make(map[int]Part, len(stored))
	for _, part := range stored {
		byNum[part.PartNumber] = part
	}
	selected := append([]Part(nil), requested...)
	sort.Slice(selected, func(i, j int) bool { return selected[i].PartNumber < selected[j].PartNumber })
	seen := make(map[int]struct{}, len(selected))
	out := make([]Part, 0, len(selected))
	for i, part := range selected {
		if _, ok := seen[part.PartNumber]; ok {
			return nil, errors.New("storage: duplicate part")
		}
		seen[part.PartNumber] = struct{}{}
		if part.PartNumber != i+1 {
			return nil, errors.New("storage: parts are not contiguous")
		}
		got, ok := byNum[part.PartNumber]
		if !ok {
			return nil, ErrNotFound
		}
		if part.Size != got.Size || (part.Checksum != "" && part.Checksum != got.Checksum) || (part.ETag != "" && part.ETag != got.ETag) {
			return nil, ErrChecksumMismatch
		}
		out = append(out, got)
	}
	return out, nil
}
