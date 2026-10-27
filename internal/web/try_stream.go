package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/xuhang113/urag/internal/retrieve"
)

func wantsTryStream(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// streamTry 在同一次查询里按真实顺序写出阶段。
// 阶段没做完时 done 为 false。查询结束、超时或失败都不再把后面的步骤补成完成。
func (s *server) streamTry(w http.ResponseWriter, r *http.Request, query string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "web: stream unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(": try\n\n")); err != nil {
		return
	}
	flusher.Flush()

	ctx, cancel := context.WithTimeout(r.Context(), trySearchTimeout)
	defer cancel()
	log := newLiveLog()
	ctx = retrieve.WithProgress(ctx, log.step)
	type outcome struct {
		hits []retrieve.Hit
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		hits, err := s.p.Try(ctx, query)
		done <- outcome{hits: hits, err: err}
	}()
	for {
		if !writeFrames(w, flusher, log.take()) {
			return
		}
		select {
		case <-log.wake:
			continue
		case got := <-done:
			if !writeFrames(w, flusher, log.take()) {
				return
			}
			writeTryOutcome(w, flusher, ctx, got.hits, got.err)
			return
		case <-ctx.Done():
			select {
			case got := <-done:
				if !writeFrames(w, flusher, log.take()) {
					return
				}
				writeTryOutcome(w, flusher, ctx, got.hits, got.err)
			default:
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					_ = writeFrames(w, flusher, [][]byte{errorFrame(tryTimeoutText)})
				}
			}
			return
		}
	}
}

func writeTryOutcome(w http.ResponseWriter, flusher http.Flusher, ctx context.Context, hits []retrieve.Hit, err error) {
	if err != nil {
		if errors.Is(err, context.Canceled) && !tryTimedOut(ctx, err) {
			return
		}
		msg := err.Error()
		if tryTimedOut(ctx, err) {
			msg = tryTimeoutText
		}
		_ = writeFrames(w, flusher, [][]byte{errorFrame(msg)})
		return
	}
	if hits == nil {
		hits = []retrieve.Hit{}
	}
	raw, marshalErr := json.Marshal(struct {
		Hits []retrieve.Hit `json:"hits"`
	}{Hits: hits})
	if marshalErr != nil {
		_ = writeFrames(w, flusher, [][]byte{errorFrame("检索失败")})
		return
	}
	_ = writeFrames(w, flusher, [][]byte{[]byte("event: result\ndata: " + string(raw) + "\n\n")})
}

func errorFrame(message string) []byte {
	raw, err := json.Marshal(struct {
		Message string `json:"message"`
	}{Message: message})
	if err != nil {
		raw = []byte(`{"message":"检索失败"}`)
	}
	return []byte("event: error\ndata: " + string(raw) + "\n\n")
}

func writeFrames(w http.ResponseWriter, flusher http.Flusher, frames [][]byte) bool {
	if len(frames) == 0 {
		return true
	}
	for _, frame := range frames {
		if _, err := w.Write(frame); err != nil {
			return false
		}
	}
	flusher.Flush()
	return true
}

type liveLog struct {
	mu   sync.Mutex
	buf  [][]byte
	wake chan struct{}
}

func newLiveLog() *liveLog {
	return &liveLog{wake: make(chan struct{}, 1)}
}

func (l *liveLog) step(name string, done bool) {
	raw, err := json.Marshal(struct {
		Name string `json:"name"`
		Done bool   `json:"done"`
	}{Name: name, Done: done})
	if err != nil {
		return
	}
	l.add(fmt.Sprintf("event: step\ndata: %s\n\n", raw))
}

func (l *liveLog) add(frame string) {
	l.mu.Lock()
	l.buf = append(l.buf, []byte(frame))
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *liveLog) take() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) == 0 {
		return nil
	}
	out := l.buf
	l.buf = nil
	return out
}
