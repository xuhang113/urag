package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xuhang113/urag/internal/model"
)

func TestLocalStreamPutGet(t *testing.T) {
	dir := t.TempDir()
	st := openLocal(t, dir, RetryPolicy{}, nil)
	body := bytes.Repeat([]byte("stream-body-"), CopyBufSize)
	if len(body) <= CopyBufSize {
		t.Fatalf("fixture length %d is not greater than CopyBufSize", len(body))
	}
	key := model.ObjectKey("docs/a.txt")
	opts := PutOptions{
		ContentType:    "text/plain",
		CollectionID:   "col-a",
		IdempotencyKey: "idem-a",
	}
	src := &boundedReader{data: body}
	meta, err := st.Put(context.Background(), key, src, opts)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if src.reads == 0 {
		t.Fatal("Put did not read the body")
	}
	wantSum := checksumOf(body)
	if meta.Key != key || meta.Size != int64(len(body)) || meta.Checksum != wantSum {
		t.Fatalf("meta = %+v, want key %s size %d checksum %s", meta, key, len(body), wantSum)
	}
	if meta.ContentType != opts.ContentType || meta.CollectionID != opts.CollectionID || meta.IdempotencyKey != opts.IdempotencyKey {
		t.Fatalf("meta fields = %+v", meta)
	}
	if attempts(t, st) != 1 {
		t.Fatalf("CommitAttempts = %d, want 1", attempts(t, st))
	}
	rc, got, err := st.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	if got != meta {
		t.Fatalf("Get meta = %+v, Put meta = %+v", got, meta)
	}
	round, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(round, body) {
		t.Fatalf("round trip length %d, want %d", len(round), len(body))
	}
	stated, err := st.Stat(context.Background(), key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if stated != meta {
		t.Fatalf("Stat = %+v, want %+v", stated, meta)
	}
	found, ok, err := st.ExistsByChecksum(context.Background(), wantSum)
	if err != nil || !ok || found != meta {
		t.Fatalf("ExistsByChecksum = %+v, %v, %v", found, ok, err)
	}
	if n := countPayload(t, dir, body); n != 1 {
		t.Fatalf("payload copies = %d, want 1", n)
	}

	emptyKey := model.ObjectKey("docs/empty.txt")
	emptyMeta, err := st.Put(context.Background(), emptyKey, &boundedReader{}, PutOptions{CollectionID: "col-a"})
	if err != nil {
		t.Fatalf("Put empty: %v", err)
	}
	if emptyMeta.Size != 0 || emptyMeta.Checksum != checksumOf(nil) {
		t.Fatalf("empty meta = %+v", emptyMeta)
	}
	emptyRC, _, err := st.Get(context.Background(), emptyKey)
	if err != nil {
		t.Fatalf("Get empty: %v", err)
	}
	defer emptyRC.Close()
	emptyBody, err := io.ReadAll(emptyRC)
	if err != nil {
		t.Fatalf("read empty: %v", err)
	}
	if len(emptyBody) != 0 {
		t.Fatalf("empty body = %q", emptyBody)
	}
}

func TestChecksumMismatchNotRetried(t *testing.T) {
	dir := t.TempDir()
	hook := &scriptedHook{errs: []error{ErrRetryable, ErrRetryable, ErrRetryable}}
	st := openLocal(t, dir, RetryPolicy{MaxAttempts: 5, BaseDelay: 0}, hook)
	body := bytes.Repeat([]byte("bad-sum-"), CopyBufSize)
	key := model.ObjectKey("docs/bad.txt")
	src := &boundedReader{data: body}
	_, err := st.Put(context.Background(), key, src, PutOptions{
		CollectionID:     "col-a",
		IdempotencyKey:   "idem-bad",
		ExpectedChecksum: model.Checksum(strings.Repeat("ab", 32)),
	})
	if err != ErrChecksumMismatch {
		t.Fatalf("Put err = %v, want ErrChecksumMismatch", err)
	}
	if attempts(t, st) != 0 {
		t.Fatalf("CommitAttempts = %d, want 0", attempts(t, st))
	}
	if len(hook.attempts) != 0 {
		t.Fatalf("OnCommit attempts = %v, want none", hook.attempts)
	}
	if src.reads == 0 {
		t.Fatal("mismatch path did not read the body")
	}
	_, _, err = st.Get(context.Background(), key)
	if err != ErrNotFound {
		t.Fatalf("Get err = %v, want ErrNotFound", err)
	}
	_, err = st.Stat(context.Background(), key)
	if err != ErrNotFound {
		t.Fatalf("Stat err = %v, want ErrNotFound", err)
	}
	if _, ok, err := st.ExistsByChecksum(context.Background(), checksumOf(body)); err != nil || ok {
		t.Fatalf("ExistsByChecksum after mismatch = %v, %v", ok, err)
	}
	if n := countPayload(t, dir, body); n != 0 {
		t.Fatalf("payload copies = %d, want 0", n)
	}
	if leftover := regularFiles(t, dir); len(leftover) != 0 {
		t.Fatalf("leftover files after mismatch: %v", leftover)
	}
}

func TestIdempotentPut(t *testing.T) {
	dir := t.TempDir()
	st := openLocal(t, dir, RetryPolicy{}, nil)
	body1 := bytes.Repeat([]byte("first-body-"), CopyBufSize)
	body2 := bytes.Repeat([]byte("second-body"), CopyBufSize)
	if bytes.Equal(body1, body2) {
		t.Fatal("fixture bodies are identical")
	}
	key := model.ObjectKey("docs/once.txt")
	opts := PutOptions{
		ContentType:    "text/plain",
		CollectionID:   "col-a",
		IdempotencyKey: "same-key",
	}
	meta1, err := st.Put(context.Background(), key, &boundedReader{data: body1}, opts)
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	afterFirst := attempts(t, st)
	second := &boundedReader{data: body2, fail: errors.New("second body must not be read")}
	opts2 := opts
	opts2.ContentType = "application/octet-stream"
	meta2, err := st.Put(context.Background(), key, second, opts2)
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if second.reads != 0 {
		t.Fatalf("second Put read the body %d times", second.reads)
	}
	if meta2 != meta1 {
		t.Fatalf("second meta = %+v, first = %+v", meta2, meta1)
	}
	if attempts(t, st) != afterFirst {
		t.Fatalf("CommitAttempts = %d, want %d", attempts(t, st), afterFirst)
	}
	got := readObject(t, st, key)
	if !bytes.Equal(got, body1) {
		t.Fatal("idempotent Put replaced the first object")
	}
	if n := countPayload(t, dir, body1); n != 1 {
		t.Fatalf("first payload copies = %d, want 1", n)
	}
	if n := countPayload(t, dir, body2); n != 0 {
		t.Fatalf("second payload copies = %d, want 0", n)
	}
	if _, ok, err := st.ExistsByChecksum(context.Background(), checksumOf(body2)); err != nil || ok {
		t.Fatalf("second checksum visible = %v, %v", ok, err)
	}
	reopened := openLocal(t, dir, RetryPolicy{}, nil)
	again := &boundedReader{data: body2, fail: errors.New("reopened store must not read the second body")}
	meta3, err := reopened.Put(context.Background(), key, again, opts)
	if err != nil {
		t.Fatalf("reopened Put: %v", err)
	}
	if again.reads != 0 || meta3 != meta1 {
		t.Fatalf("reopened meta = %+v, reads = %d", meta3, again.reads)
	}

	body3 := bytes.Repeat([]byte("third-body-"), CopyBufSize)
	overKey := model.ObjectKey("docs/overwrite.txt")
	overOpts := PutOptions{CollectionID: "col-a"}
	if _, err := st.Put(context.Background(), overKey, &boundedReader{data: body3}, overOpts); err != nil {
		t.Fatalf("empty idempotency first Put: %v", err)
	}
	if _, err := st.Put(context.Background(), overKey, &boundedReader{data: body2}, overOpts); err != nil {
		t.Fatalf("empty idempotency second Put: %v", err)
	}
	if !bytes.Equal(readObject(t, st, overKey), body2) {
		t.Fatal("empty idempotency key did not overwrite")
	}
	if _, ok, err := st.ExistsByChecksum(context.Background(), checksumOf(body3)); err != nil || ok {
		t.Fatalf("overwritten checksum still visible = %v, %v", ok, err)
	}
}

func TestMultipartResume(t *testing.T) {
	dir := t.TempDir()
	st := openLocal(t, dir, RetryPolicy{MaxAttempts: 3}, nil)
	payload := bytes.Repeat([]byte("Mp"), CopyBufSize+40)
	if len(payload) <= CopyBufSize {
		t.Fatalf("payload length %d", len(payload))
	}
	p1 := payload[:CopyBufSize+1]
	p2 := payload[CopyBufSize+1 : CopyBufSize+1+80]
	p3 := payload[CopyBufSize+1+80:]
	if len(p1) == 0 || len(p2) == 0 || len(p3) == 0 {
		t.Fatal("empty part")
	}
	key := model.ObjectKey("docs/multi.bin")
	opts := PutOptions{ContentType: "application/octet-stream", CollectionID: "col-m", IdempotencyKey: "multi-1"}

	bad, err := st.CreateMultipart(context.Background(), key, PutOptions{
		CollectionID:     "col-m",
		IdempotencyKey:   "multi-bad",
		ExpectedChecksum: model.Checksum(strings.Repeat("00", 32)),
	})
	if err != nil {
		t.Fatalf("CreateMultipart bad: %v", err)
	}
	if _, err := st.UploadPart(context.Background(), bad, 1, &boundedReader{data: p1}); err != nil {
		t.Fatalf("UploadPart bad: %v", err)
	}
	before := attempts(t, st)
	if _, err := st.CompleteMultipart(context.Background(), bad, []Part{{PartNumber: 1, Size: int64(len(p1)), Checksum: checksumOf(p1)}}); err != ErrChecksumMismatch {
		t.Fatalf("bad complete err = %v", err)
	}
	if attempts(t, st) != before {
		t.Fatalf("checksum mismatch changed CommitAttempts from %d to %d", before, attempts(t, st))
	}
	if _, _, err := st.Get(context.Background(), key); err != ErrNotFound {
		t.Fatalf("Get after bad complete = %v", err)
	}
	still, err := st.ListParts(context.Background(), bad)
	if err != nil {
		t.Fatalf("ListParts after bad complete: %v", err)
	}
	if len(still) != 1 || still[0].PartNumber != 1 {
		t.Fatalf("parts after bad complete = %+v", still)
	}
	if err := st.AbortMultipart(context.Background(), bad); err != nil {
		t.Fatalf("AbortMultipart: %v", err)
	}
	if _, err := st.ListParts(context.Background(), bad); err != ErrNotFound {
		t.Fatalf("ListParts after abort = %v", err)
	}
	if err := st.AbortMultipart(context.Background(), bad); err != ErrNotFound {
		t.Fatalf("second abort = %v", err)
	}

	session, err := st.CreateMultipart(context.Background(), key, opts)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if _, err := st.UploadPart(context.Background(), session, 0, &boundedReader{data: p1}); err == nil {
		t.Fatal("part 0 was accepted")
	}
	part1, err := st.UploadPart(context.Background(), session, 1, &boundedReader{data: p1})
	if err != nil {
		t.Fatalf("UploadPart 1: %v", err)
	}
	part3, err := st.UploadPart(context.Background(), session, 3, &boundedReader{data: p3})
	if err != nil {
		t.Fatalf("UploadPart 3: %v", err)
	}
	if part1.PartNumber != 1 || part1.Size != int64(len(p1)) || part1.Checksum != checksumOf(p1) || part1.ETag == "" {
		t.Fatalf("part1 = %+v", part1)
	}
	if part3.PartNumber != 3 || part3.Size != int64(len(p3)) || part3.Checksum != checksumOf(p3) {
		t.Fatalf("part3 = %+v", part3)
	}
	listed, err := st.ListParts(context.Background(), session)
	if err != nil {
		t.Fatalf("ListParts: %v", err)
	}
	if !samePartNumbers(listed, []int{1, 3}) {
		t.Fatalf("ListParts = %+v, want part numbers 1 and 3", listed)
	}
	if _, _, err := st.Get(context.Background(), key); err != ErrNotFound {
		t.Fatalf("Get before complete = %v", err)
	}
	listedAgain, err := st.ListParts(context.Background(), session)
	if err != nil {
		t.Fatalf("ListParts again: %v", err)
	}
	if !samePartNumbers(listedAgain, []int{1, 3}) {
		t.Fatalf("ListParts again = %+v", listedAgain)
	}
	reopened := openLocal(t, dir, RetryPolicy{}, nil)
	listedReopen, err := reopened.ListParts(context.Background(), session)
	if err != nil {
		t.Fatalf("ListParts after reopen: %v", err)
	}
	if !samePartNumbers(listedReopen, []int{1, 3}) {
		t.Fatalf("ListParts after reopen = %+v", listedReopen)
	}
	if _, ok, err := st.ExistsByChecksum(context.Background(), checksumOf(payload)); err != nil || ok {
		t.Fatalf("checksum visible before complete = %v, %v", ok, err)
	}
	part2, err := st.UploadPart(context.Background(), session, 2, &boundedReader{data: p2})
	if err != nil {
		t.Fatalf("UploadPart 2: %v", err)
	}
	if part2.Checksum != checksumOf(p2) || part2.Size != int64(len(p2)) {
		t.Fatalf("part2 = %+v", part2)
	}
	meta, err := st.CompleteMultipart(context.Background(), session, []Part{part3, part1, part2})
	if err != nil {
		t.Fatalf("CompleteMultipart: %v", err)
	}
	if meta.Key != key || meta.Size != int64(len(payload)) || meta.Checksum != checksumOf(payload) {
		t.Fatalf("complete meta = %+v", meta)
	}
	if meta.CollectionID != opts.CollectionID || meta.IdempotencyKey != opts.IdempotencyKey || meta.ContentType != opts.ContentType {
		t.Fatalf("complete meta fields = %+v", meta)
	}
	if !bytes.Equal(readObject(t, st, key), payload) {
		t.Fatal("completed object bytes mismatch")
	}
	found, ok, err := st.ExistsByChecksum(context.Background(), meta.Checksum)
	if err != nil || !ok || found.Key != key {
		t.Fatalf("ExistsByChecksum after complete = %+v, %v, %v", found, ok, err)
	}
	if _, err := st.ListParts(context.Background(), session); err != ErrNotFound {
		t.Fatalf("ListParts after complete = %v", err)
	}
	if n := countPayload(t, dir, payload); n != 1 {
		t.Fatalf("completed payload copies = %d, want 1", n)
	}
}

func TestPutCancel(t *testing.T) {
	dir := t.TempDir()
	hook := &scriptedHook{errs: []error{ErrRetryable}}
	st := openLocal(t, dir, RetryPolicy{MaxAttempts: 4, BaseDelay: time.Hour}, hook)
	body := bytes.Repeat([]byte("cancel-me-"), CopyBufSize)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &boundedReader{data: body, cancelOnRead: 2, cancel: cancel}
	_, err := st.Put(ctx, model.ObjectKey("docs/cancel.txt"), src, PutOptions{
		CollectionID:   "col-a",
		IdempotencyKey: "idem-cancel",
	})
	if err != context.Canceled {
		t.Fatalf("Put err = %v, want context.Canceled", err)
	}
	if attempts(t, st) != 0 {
		t.Fatalf("CommitAttempts = %d, want 0", attempts(t, st))
	}
	if len(hook.attempts) != 0 {
		t.Fatalf("canceled Put called OnCommit: %v", hook.attempts)
	}
	if _, _, err := st.Get(context.Background(), "docs/cancel.txt"); err != ErrNotFound {
		t.Fatalf("Get err = %v, want ErrNotFound", err)
	}
	if _, err := st.Stat(context.Background(), "docs/cancel.txt"); err != ErrNotFound {
		t.Fatalf("Stat err = %v, want ErrNotFound", err)
	}
	if leftover := regularFiles(t, dir); len(leftover) != 0 {
		t.Fatalf("leftover files after cancel: %v", leftover)
	}

	deadline, stop := context.WithTimeout(context.Background(), time.Nanosecond)
	defer stop()
	time.Sleep(time.Millisecond)
	_, err = st.Put(deadline, model.ObjectKey("docs/deadline.txt"), &boundedReader{data: body}, PutOptions{CollectionID: "col-a"})
	if err != context.DeadlineExceeded {
		t.Fatalf("deadline Put err = %v", err)
	}
	if attempts(t, st) != 0 {
		t.Fatalf("deadline CommitAttempts = %d", attempts(t, st))
	}
	if _, _, err := st.Get(context.Background(), "docs/deadline.txt"); err != ErrNotFound {
		t.Fatalf("Get after deadline = %v", err)
	}
}

func TestExportedAPIHasNoVendorTypes(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(info os.FileInfo) bool {
		name := info.Name()
		return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	pkg := pkgs["storage"]
	if pkg == nil {
		t.Fatal("package storage not parsed")
	}
	for filename, astFile := range pkg.Files {
		imports := mapImportPaths(astFile)
		for _, decl := range astFile.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !exportedFunc(d) {
					continue
				}
				checkVendorTypes(t, filename, d.Name.Name, d.Type, imports)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if !s.Name.IsExported() {
							continue
						}
						checkVendorName(t, filename, s.Name.Name, s.Name.Name)
						checkVendorTypes(t, filename, s.Name.Name, s.Type, imports)
					case *ast.ValueSpec:
						for _, name := range s.Names {
							if !name.IsExported() {
								continue
							}
							checkVendorName(t, filename, name.Name, name.Name)
							if s.Type != nil {
								checkVendorTypes(t, filename, name.Name, s.Type, imports)
							}
						}
					}
				}
			}
		}
	}
}

func TestCommitRetryDoesNotReread(t *testing.T) {
	dir := t.TempDir()
	body := bytes.Repeat([]byte("r"), CopyBufSize+10)
	src := &boundedReader{data: body}
	hook := &scriptedHook{
		errs:   []error{ErrRetryable, fmt.Errorf("wrapped: %w", ErrRetryable), nil},
		reader: src,
	}
	st := openLocal(t, dir, RetryPolicy{MaxAttempts: 0, BaseDelay: 0, MaxDelay: 0}, hook)
	start := time.Now()
	meta, err := st.Put(context.Background(), "docs/retry.txt", src, PutOptions{
		ContentType:  "text/plain",
		CollectionID: "col-r",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("zero BaseDelay slept %s", elapsed)
	}
	if attempts(t, st) != 3 {
		t.Fatalf("CommitAttempts = %d, want 3", attempts(t, st))
	}
	if !sameInts(hook.attempts, []int{1, 2, 3}) {
		t.Fatalf("OnCommit attempts = %v", hook.attempts)
	}
	if len(hook.readCounts) != 3 {
		t.Fatalf("read snapshots = %v", hook.readCounts)
	}
	for i, n := range hook.readCounts {
		if n != hook.readCounts[0] || n == 0 {
			t.Fatalf("body reread during commit: snapshot %d = %v", i, hook.readCounts)
		}
	}
	if src.reads > 4 {
		t.Fatalf("body reads = %d, want one streaming pass", src.reads)
	}
	if meta.Checksum != checksumOf(body) || meta.Size != int64(len(body)) {
		t.Fatalf("meta = %+v", meta)
	}
	if !bytes.Equal(readObject(t, st, "docs/retry.txt"), body) {
		t.Fatal("retried commit stored the wrong body")
	}

	onceHook := &scriptedHook{errs: []error{ErrRetryable}}
	once := openLocal(t, t.TempDir(), RetryPolicy{MaxAttempts: 1, BaseDelay: 0}, onceHook)
	_, err = once.Put(context.Background(), "docs/once.txt", &boundedReader{data: body}, PutOptions{CollectionID: "col-r"})
	if err != ErrRetryable {
		t.Fatalf("single attempt err = %v", err)
	}
	if attempts(t, once) != 1 || len(onceHook.attempts) != 1 {
		t.Fatalf("attempts = %d, hook = %v", attempts(t, once), onceHook.attempts)
	}
	if _, _, err := once.Get(context.Background(), "docs/once.txt"); err != ErrNotFound {
		t.Fatalf("Get after failed commit = %v", err)
	}

	plain := errors.New("not retryable")
	plainHook := &scriptedHook{errs: []error{plain, ErrRetryable}}
	plainStore := openLocal(t, t.TempDir(), RetryPolicy{MaxAttempts: 4, BaseDelay: 0}, plainHook)
	_, err = plainStore.Put(context.Background(), "docs/plain.txt", &boundedReader{data: body}, PutOptions{CollectionID: "col-r"})
	if err != plain {
		t.Fatalf("non-retryable err = %v", err)
	}
	if attempts(t, plainStore) != 1 || len(plainHook.attempts) != 1 {
		t.Fatalf("non-retryable attempts = %d, hook = %v", attempts(t, plainStore), plainHook.attempts)
	}
	if _, _, err := plainStore.Get(context.Background(), "docs/plain.txt"); err != ErrNotFound {
		t.Fatalf("Get after non-retryable = %v", err)
	}
}

func TestDeadlineAndNonRetryableStop(t *testing.T) {
	body := bytes.Repeat([]byte("stop-body-"), CopyBufSize)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := &scriptedHook{errs: []error{tc.err, ErrRetryable}}
			st := openLocal(t, t.TempDir(), RetryPolicy{MaxAttempts: 4, BaseDelay: 0}, hook)
			_, err := st.Put(context.Background(), "docs/stop.txt", &boundedReader{data: body}, PutOptions{CollectionID: "col-s"})
			if err != tc.err {
				t.Fatalf("Put err = %v, want %v", err, tc.err)
			}
			if attempts(t, st) != 1 || len(hook.attempts) != 1 {
				t.Fatalf("attempts = %d, hook = %v", attempts(t, st), hook.attempts)
			}
			if _, _, err := st.Get(context.Background(), "docs/stop.txt"); err != ErrNotFound {
				t.Fatalf("Get = %v", err)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hook := &scriptedHook{errs: []error{ErrRetryable, nil}}
	hook.onCall = func(attempt int) {
		if attempt == 1 {
			time.AfterFunc(30*time.Millisecond, cancel)
		}
	}
	st := openLocal(t, t.TempDir(), RetryPolicy{MaxAttempts: 3, BaseDelay: 5 * time.Second, MaxDelay: 5 * time.Second}, hook)
	start := time.Now()
	_, err := st.Put(ctx, "docs/backoff.txt", &boundedReader{data: body}, PutOptions{CollectionID: "col-s"})
	if err != context.Canceled {
		t.Fatalf("backoff err = %v", err)
	}
	if time.Since(start) >= 2*time.Second {
		t.Fatalf("backoff ignored cancel for %s", time.Since(start))
	}
	if attempts(t, st) != 1 {
		t.Fatalf("CommitAttempts = %d, want 1", attempts(t, st))
	}
	if _, _, err := st.Get(context.Background(), "docs/backoff.txt"); err != ErrNotFound {
		t.Fatalf("Get after canceled backoff = %v", err)
	}
}

func TestDeleteClearsIdempotencyAndChecksum(t *testing.T) {
	dir := t.TempDir()
	st := openLocal(t, dir, RetryPolicy{}, nil)
	body1 := bytes.Repeat([]byte("delete-one-"), CopyBufSize)
	body2 := bytes.Repeat([]byte("delete-two-"), CopyBufSize)
	key := model.ObjectKey("docs/gone.txt")
	opts := PutOptions{ContentType: "text/plain", CollectionID: "col-d", IdempotencyKey: "idem-d"}
	meta, err := st.Put(context.Background(), key, &boundedReader{data: body1}, opts)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := st.Delete(context.Background(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := st.Get(context.Background(), key); err != ErrNotFound {
		t.Fatalf("Get after delete = %v", err)
	}
	if _, err := st.Stat(context.Background(), key); err != ErrNotFound {
		t.Fatalf("Stat after delete = %v", err)
	}
	if _, ok, err := st.ExistsByChecksum(context.Background(), meta.Checksum); err != nil || ok {
		t.Fatalf("checksum after delete = %v, %v", ok, err)
	}
	if err := st.Delete(context.Background(), key); err != ErrNotFound {
		t.Fatalf("second Delete = %v", err)
	}
	reopened := openLocal(t, dir, RetryPolicy{}, nil)
	if _, _, err := reopened.Get(context.Background(), key); err != ErrNotFound {
		t.Fatalf("Get after reopen = %v", err)
	}
	if _, ok, err := reopened.ExistsByChecksum(context.Background(), meta.Checksum); err != nil || ok {
		t.Fatalf("checksum after reopen = %v, %v", ok, err)
	}
	meta2, err := st.Put(context.Background(), key, &boundedReader{data: body2}, opts)
	if err != nil {
		t.Fatalf("Put after delete: %v", err)
	}
	if meta2 == meta || meta2.Checksum == meta.Checksum {
		t.Fatalf("idempotency survived delete: %+v and %+v", meta, meta2)
	}
	if !bytes.Equal(readObject(t, st, key), body2) {
		t.Fatal("put after delete did not store the new body")
	}
	if _, ok, err := st.ExistsByChecksum(context.Background(), meta2.Checksum); err != nil || !ok {
		t.Fatalf("new checksum = %v, %v", ok, err)
	}
}

func TestIdempotencyScopedToCollection(t *testing.T) {
	st := openLocal(t, t.TempDir(), RetryPolicy{}, nil)
	bodyA := bytes.Repeat([]byte("collection-a-"), CopyBufSize)
	bodyB := bytes.Repeat([]byte("collection-b-"), CopyBufSize)
	optsA := PutOptions{ContentType: "text/plain", CollectionID: "c1", IdempotencyKey: "shared"}
	optsB := PutOptions{ContentType: "text/plain", CollectionID: "c2", IdempotencyKey: "shared"}
	metaA, err := st.Put(context.Background(), "a", &boundedReader{data: bodyA}, optsA)
	if err != nil {
		t.Fatalf("Put A: %v", err)
	}
	metaB, err := st.Put(context.Background(), "b", &boundedReader{data: bodyB}, optsB)
	if err != nil {
		t.Fatalf("Put B: %v", err)
	}
	if metaA == metaB || metaA.Checksum == metaB.Checksum {
		t.Fatalf("different collections collapsed: %+v %+v", metaA, metaB)
	}
	if !bytes.Equal(readObject(t, st, "a"), bodyA) || !bytes.Equal(readObject(t, st, "b"), bodyB) {
		t.Fatal("collection scope lost a body")
	}
	second := &boundedReader{data: bodyB, fail: errors.New("same collection must not read a second body")}
	metaC, err := st.Put(context.Background(), "c", second, optsA)
	if err != nil {
		t.Fatalf("same collection second Put: %v", err)
	}
	if second.reads != 0 {
		t.Fatalf("same collection reread %d times", second.reads)
	}
	if metaC != metaA {
		t.Fatalf("same collection meta = %+v, want %+v", metaC, metaA)
	}
	if _, _, err := st.Get(context.Background(), "c"); err != ErrNotFound {
		t.Fatalf("second key Get = %v", err)
	}
}

func TestRejectEscapingKeys(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside-object")
	st := openLocal(t, root, RetryPolicy{}, nil)
	body := bytes.Repeat([]byte("escape-body-"), CopyBufSize)
	keys := []model.ObjectKey{
		"../outside-object",
		"sub/../../outside-object",
		"..",
		model.ObjectKey(outside),
		"/tmp/urag-storage-escape",
		"foo..bar",
		"",
	}
	for _, key := range keys {
		_, err := st.Put(context.Background(), key, &boundedReader{data: body}, PutOptions{CollectionID: "col-e"})
		if err == nil {
			t.Fatalf("Put accepted key %q", key)
		}
		if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
			t.Fatalf("key %q wrote %s: %v", key, outside, statErr)
		}
	}
	if attempts(t, st) != 0 {
		t.Fatalf("rejected keys committed %d times", attempts(t, st))
	}
	if leftover := regularFiles(t, root); len(leftover) != 0 {
		t.Fatalf("rejected keys left files: %v", leftover)
	}
	key := model.ObjectKey("nested/ok.txt")
	if _, err := st.Put(context.Background(), key, &boundedReader{data: body}, PutOptions{CollectionID: "col-e"}); err != nil {
		t.Fatalf("nested key: %v", err)
	}
	if !bytes.Equal(readObject(t, st, key), body) {
		t.Fatal("nested key round trip failed")
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("valid put wrote outside: %v", statErr)
	}
}

func TestNewS3RejectsIncompleteConfig(t *testing.T) {
	const access = "test-access-value"
	const secret = "test-secret-value"
	incomplete := []S3Config{
		{},
		{Endpoint: "http://127.0.0.1:9", Bucket: "bucket", AccessKey: access, SecretKey: secret},
		{Region: "us-east-1", Bucket: "bucket", AccessKey: access, SecretKey: secret},
		{Endpoint: "http://127.0.0.1:9", Region: "us-east-1", AccessKey: access, SecretKey: secret},
		{Endpoint: "http://127.0.0.1:9", Region: "us-east-1", Bucket: "bucket", SecretKey: secret},
		{Endpoint: "http://127.0.0.1:9", Region: "us-east-1", Bucket: "bucket", AccessKey: access},
	}
	for i, cfg := range incomplete {
		_, err := NewS3(cfg)
		if err == nil || errors.Is(err, ErrNotImplemented) {
			t.Fatalf("config %d err = %v", i, err)
		}
		if strings.Contains(err.Error(), access) || strings.Contains(err.Error(), secret) {
			t.Fatalf("config %d leaked credential: %v", i, err)
		}
	}
	store, err := NewS3(S3Config{
		Endpoint:  "http://127.0.0.1:9",
		Region:    "us-east-1",
		Bucket:    "bucket",
		AccessKey: access,
		SecretKey: secret,
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("complete config: %v", err)
	}
	if store == nil {
		t.Fatal("complete config returned a nil store")
	}
}

func TestConfigFromEnv(t *testing.T) {
	if _, err := ConfigFromEnv(nil); err == nil {
		t.Fatal("nil getenv returned nil error")
	}
	cfg, err := ConfigFromEnv(func(key string) string {
		switch key {
		case "URAG_S3_ENDPOINT":
			return "http://127.0.0.1:9000"
		case "URAG_S3_REGION":
			return "us-east-1"
		case "URAG_S3_BUCKET":
			return "bucket"
		case "URAG_S3_ACCESS_KEY":
			return "test-access-value"
		case "URAG_S3_SECRET_KEY":
			return "test-secret-value"
		case "URAG_S3_PATH_STYLE":
			return "true"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Endpoint != "http://127.0.0.1:9000" || cfg.Region != "us-east-1" || cfg.Bucket != "bucket" || cfg.AccessKey != "test-access-value" || cfg.SecretKey != "test-secret-value" || !cfg.PathStyle {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.PartSize != 0 || cfg.Retry != (RetryPolicy{}) {
		t.Fatalf("PartSize or Retry was read from the environment: %+v", cfg)
	}
	empty, err := ConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("missing fields err = %v", err)
	}
	if empty != (S3Config{}) {
		t.Fatalf("missing fields cfg = %+v", empty)
	}
	for _, v := range []string{"1", "true", "TRUE", "True"} {
		got, err := ConfigFromEnv(func(key string) string {
			if key == "URAG_S3_PATH_STYLE" {
				return v
			}
			return ""
		})
		if err != nil || !got.PathStyle {
			t.Fatalf("PATH_STYLE %q => %+v, %v", v, got, err)
		}
	}
	for _, v := range []string{"", "false", "yes", "0"} {
		got, err := ConfigFromEnv(func(key string) string {
			if key == "URAG_S3_PATH_STYLE" {
				return v
			}
			return ""
		})
		if err != nil || got.PathStyle {
			t.Fatalf("PATH_STYLE %q => %+v, %v", v, got, err)
		}
	}
}

type boundedReader struct {
	data         []byte
	off          int
	reads        int
	fail         error
	cancelOnRead int
	cancel       context.CancelFunc
}

func (r *boundedReader) Read(p []byte) (int, error) {
	if len(p) > CopyBufSize {
		return 0, fmt.Errorf("read size %d exceeds CopyBufSize %d", len(p), CopyBufSize)
	}
	if r.fail != nil {
		r.reads++
		return 0, r.fail
	}
	if len(p) == 0 {
		return 0, nil
	}
	r.reads++
	if r.cancelOnRead > 0 && r.reads == r.cancelOnRead {
		if r.cancel != nil {
			r.cancel()
		}
		return 0, context.Canceled
	}
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	if r.off >= len(r.data) {
		return n, io.EOF
	}
	return n, nil
}

type scriptedHook struct {
	errs       []error
	attempts   []int
	readCounts []int
	reader     *boundedReader
	onCall     func(attempt int)
}

func (h *scriptedHook) OnCommit(attempt int) error {
	h.attempts = append(h.attempts, attempt)
	if h.reader != nil {
		h.readCounts = append(h.readCounts, h.reader.reads)
	}
	if h.onCall != nil {
		h.onCall(attempt)
	}
	idx := len(h.attempts) - 1
	if idx >= 0 && idx < len(h.errs) && h.errs[idx] != nil {
		return h.errs[idx]
	}
	return nil
}

func openLocal(t *testing.T, dir string, policy RetryPolicy, hook Hook) Store {
	t.Helper()
	store, err := NewLocal(dir, policy, hook)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if store == nil {
		t.Fatal("NewLocal returned a nil store")
	}
	return store
}

func attempts(t *testing.T, st Store) int {
	t.Helper()
	counter, ok := st.(interface{ CommitAttempts() int })
	if !ok {
		t.Fatalf("store %T has no CommitAttempts", st)
	}
	return counter.CommitAttempts()
}

func readObject(t *testing.T, st Store, key model.ObjectKey) []byte {
	t.Helper()
	rc, _, err := st.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get %s: %v", key, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return body
}

func checksumOf(body []byte) model.Checksum {
	sum := sha256.Sum256(body)
	return model.Checksum(hex.EncodeToString(sum[:]))
}

func countPayload(t *testing.T, dir string, payload []byte) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		got, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Equal(got, payload) {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func regularFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return files
}

func samePartNumbers(parts []Part, numbers []int) bool {
	if len(parts) != len(numbers) {
		return false
	}
	for i := range parts {
		if parts[i].PartNumber != numbers[i] {
			return false
		}
	}
	return true
}

func sameInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func exportedFunc(fn *ast.FuncDecl) bool {
	if fn == nil || fn.Name == nil || !fn.Name.IsExported() {
		return false
	}
	if fn.Recv == nil {
		return true
	}
	if len(fn.Recv.List) == 0 {
		return false
	}
	id := receiverIdent(fn.Recv.List[0].Type)
	return id != nil && id.IsExported()
}

func receiverIdent(expr ast.Expr) *ast.Ident {
	switch t := expr.(type) {
	case *ast.Ident:
		return t
	case *ast.StarExpr:
		return receiverIdent(t.X)
	default:
		return nil
	}
}

func mapImportPaths(file *ast.File) map[string]string {
	paths := make(map[string]string, len(file.Imports))
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		local := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			local = path[i+1:]
		}
		if imp.Name != nil && imp.Name.Name != "_" {
			local = imp.Name.Name
		}
		paths[local] = path
	}
	return paths
}

func checkVendorTypes(t *testing.T, file, where string, node ast.Node, imports map[string]string) {
	t.Helper()
	if node == nil {
		return
	}
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			checkVendorName(t, file, where, x.Name)
		case *ast.SelectorExpr:
			qualified := x.Sel.Name
			if id, ok := x.X.(*ast.Ident); ok {
				checkVendorName(t, file, where, id.Name)
				if path, ok := imports[id.Name]; ok {
					qualified = path + "." + x.Sel.Name
				} else {
					qualified = id.Name + "." + x.Sel.Name
				}
			}
			checkVendorName(t, file, where, x.Sel.Name)
			checkVendorName(t, file, where, qualified)
		}
		return true
	})
}

func checkVendorName(t *testing.T, file, where, name string) {
	t.Helper()
	lower := strings.ToLower(name)
	for _, banned := range []string{"aws", "minio", "aliyun", "tencent", "oss", "cos"} {
		if strings.Contains(lower, banned) {
			t.Errorf("%s: exported signature %s contains %q in %q", file, where, banned, name)
		}
	}
}
