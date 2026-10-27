package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuhang113/urag/internal/model"
)

var errInvalidKey = errors.New("storage: invalid object key")

func validateKey(key model.ObjectKey) error {
	s := string(key)
	if s == "" || strings.ContainsRune(s, 0) || strings.Contains(s, "..") {
		return errInvalidKey
	}
	if filepath.IsAbs(s) || strings.HasPrefix(s, "/") || strings.HasPrefix(s, `\`) {
		return errInvalidKey
	}
	if filepath.VolumeName(s) != "" {
		return errInvalidKey
	}
	return nil
}

func ensureWithin(root, target string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	tgtAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, tgtAbs)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errInvalidKey
	}
	return nil
}

func safeBase(name string) bool {
	return name != "" && name == filepath.Base(name) && !strings.Contains(name, "..") && !strings.ContainsRune(name, 0)
}

func normalizeCtx(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return err
}

func shouldRetry(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrChecksumMismatch) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return errors.Is(err, ErrRetryable)
}

func retryCommit(ctx context.Context, policy RetryPolicy, hook Hook, mark func(), publish func() error) error {
	return retryCommitIf(ctx, policy, hook, mark, publish, shouldRetry)
}

func retryCommitIf(ctx context.Context, policy RetryPolicy, hook Hook, mark func(), publish func() error, retryable func(error) bool) error {
	if retryable == nil {
		retryable = shouldRetry
	}
	max := policy.MaxAttempts
	if max <= 0 {
		max = 3
	}
	var last error
	for attempt := 1; attempt <= max; attempt++ {
		if attempt > 1 {
			delay := backoff(policy, attempt-1)
			if delay > 0 {
				if err := sleepCtx(ctx, delay); err != nil {
					return normalizeCtx(err)
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return normalizeCtx(err)
		}
		if mark != nil {
			mark()
		}
		if hook != nil {
			err := hook.OnCommit(attempt)
			if err != nil {
				last = err
				if !retryable(err) || attempt == max {
					return normalizeCtx(err)
				}
				continue
			}
		}
		if err := publish(); err != nil {
			last = err
			if !retryable(err) || attempt == max {
				return normalizeCtx(err)
			}
			continue
		}
		return nil
	}
	if last == nil {
		return errors.New("storage: commit failed")
	}
	return normalizeCtx(last)
}

func backoff(p RetryPolicy, failed int) time.Duration {
	if p.BaseDelay <= 0 || failed <= 0 {
		return 0
	}
	d := p.BaseDelay
	for i := 1; i < failed; i++ {
		if p.MaxDelay > 0 && d >= p.MaxDelay {
			return p.MaxDelay
		}
		next := d * 2
		if next <= 0 {
			if p.MaxDelay > 0 {
				return p.MaxDelay
			}
			return d
		}
		d = next
	}
	if p.MaxDelay > 0 && d > p.MaxDelay {
		return p.MaxDelay
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func writeLimited(ctx context.Context, dst io.Writer, src io.Reader) (int64, model.Checksum, error) {
	buf := make([]byte, CopyBufSize)
	h := sha256.New()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, "", normalizeCtx(err)
		}
		nr, er := src.Read(buf)
		if nr > 0 {
			if _, err := dst.Write(buf[:nr]); err != nil {
				return total, "", err
			}
			if _, err := h.Write(buf[:nr]); err != nil {
				return total, "", err
			}
			total += int64(nr)
		}
		if er != nil {
			if errors.Is(er, io.EOF) {
				return total, model.Checksum(hex.EncodeToString(h.Sum(nil))), nil
			}
			return total, "", normalizeCtx(er)
		}
		if nr == 0 {
			return total, "", errors.New("storage: reader returned no progress")
		}
	}
}

func spoolFile(ctx context.Context, dir string, r io.Reader) (string, int64, model.Checksum, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, "", err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", 0, "", err
	}
	name := f.Name()
	if err := ensureWithin(dir, name); err != nil {
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
	size, sum, err := writeLimited(ctx, f, r)
	if err != nil {
		return "", 0, "", err
	}
	if err := f.Sync(); err != nil {
		return "", 0, "", err
	}
	if err := f.Close(); err != nil {
		return "", 0, "", err
	}
	remove = false
	return name, size, sum, nil
}

func writeJSONAtomic(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	remove := true
	defer func() {
		if remove {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	remove = false
	return nil
}

func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(v); err != nil {
		return err
	}
	return nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func idemIndex(col model.CollectionID, idem model.IdempotencyKey) string {
	return string(col) + "\x00" + string(idem)
}

func metaFileName(key model.ObjectKey) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".json"
}

type shaState struct {
	hash.Hash
}

func sha256New() *shaState {
	return &shaState{Hash: sha256.New()}
}

func (s *shaState) sum() model.Checksum {
	return model.Checksum(hex.EncodeToString(s.Sum(nil)))
}

func copyInto(ctx context.Context, dst io.Writer, extra io.Writer, src io.Reader, buf []byte) (int64, error) {
	if len(buf) == 0 || len(buf) > CopyBufSize {
		buf = make([]byte, CopyBufSize)
	}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, normalizeCtx(err)
		}
		nr, er := src.Read(buf)
		if nr > 0 {
			if _, err := dst.Write(buf[:nr]); err != nil {
				return total, err
			}
			if extra != nil {
				if _, err := extra.Write(buf[:nr]); err != nil {
					return total, err
				}
			}
			total += int64(nr)
		}
		if er != nil {
			if errors.Is(er, io.EOF) {
				return total, nil
			}
			return total, normalizeCtx(er)
		}
		if nr == 0 {
			return total, errors.New("storage: reader returned no progress")
		}
	}
}
