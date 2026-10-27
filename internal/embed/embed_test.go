package embed_test

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuhang113/urag/internal/embed"
)

func TestTokens(t *testing.T) {
	got := embed.Tokens("Crimson, lantern!")
	want := []string{"crimson", "lantern"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokens = %v", got)
	}
	if got := embed.Tokens("alpha2beta east-stone"); !reflect.DeepEqual(got, []string{"alpha", "beta", "east", "stone"}) {
		t.Fatalf("split = %v", got)
	}
	if got := embed.Tokens("Cat cat"); !reflect.DeepEqual(got, []string{"cat", "cat"}) {
		t.Fatalf("duplicates = %v", got)
	}
	if len(embed.Tokens("")) != 0 || len(embed.Tokens("123 ...")) != 0 {
		t.Fatal("expected no tokens")
	}
}

func TestFixtureEmbed(t *testing.T) {
	if embed.NewFixture(0).Dim() != 64 || embed.NewFixture(-3).Dim() != 64 {
		t.Fatal("non-positive dim should be 64")
	}
	emb := embed.NewFixture(8)
	if emb.Dim() != 8 {
		t.Fatal(emb.Dim())
	}
	ctx := context.Background()
	none, err := emb.Embed(ctx, nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("empty input: %v %v", none, err)
	}
	vecs, err := emb.Embed(ctx, []string{"", "...", "Crimson Lantern", "crimson lantern"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 4 || len(vecs[0]) != 8 {
		t.Fatalf("shape %d", len(vecs))
	}
	assertZero(t, vecs[0])
	assertZero(t, vecs[1])
	if !reflect.DeepEqual(vecs[2], vecs[3]) {
		t.Fatalf("case differs\n%v\n%v", vecs[2], vecs[3])
	}
	again, err := emb.Embed(ctx, []string{"crimson lantern"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(vecs[3], again[0]) {
		t.Fatal("same text changed")
	}
	if !reflect.DeepEqual(vecs[3], specVector("crimson lantern", 8)) {
		t.Fatalf("vector = %v\nwant %v", vecs[3], specVector("crimson lantern", 8))
	}
	one := embed.NewFixture(1)
	got, err := one.Embed(ctx, []string{"Hello"})
	if err != nil || len(got) != 1 || got[0][0] != 1 {
		t.Fatalf("dim1 = %v %v", got, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := emb.Embed(canceled, []string{"a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

func TestHTTPEmbed(t *testing.T) {
	const secret = "test-embed-key"
	t.Run("empty", func(t *testing.T) {
		called := false
		emb := embed.NewHTTP(embed.Config{}, func(*http.Request) (*http.Response, error) {
			called = true
			return nil, errors.New("should not send")
		})
		got, err := emb.Embed(context.Background(), nil)
		if err != nil || len(got) != 0 || called {
			t.Fatalf("got %v err %v called %v", got, err, called)
		}
		if emb.Dim() != 0 {
			t.Fatal(emb.Dim())
		}
	})

	t.Run("batches", func(t *testing.T) {
		t.Setenv("CUSTOM_EMBED_KEY", secret)
		var mu sync.Mutex
		inflight, maxInflight := 0, 0
		var lengths []int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			inflight++
			if inflight > maxInflight {
				maxInflight = inflight
			}
			mu.Unlock()
			defer func() {
				mu.Lock()
				inflight--
				mu.Unlock()
			}()
			time.Sleep(30 * time.Millisecond)
			if r.Method != http.MethodPost {
				http.Error(w, "method", http.StatusBadRequest)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+secret {
				http.Error(w, "auth", http.StatusUnauthorized)
				return
			}
			var body struct {
				Input []string `json:"input"`
				Model string   `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if body.Model != "fixture-model" {
				http.Error(w, "model", http.StatusBadRequest)
				return
			}
			mu.Lock()
			lengths = append(lengths, len(body.Input))
			mu.Unlock()
			vecs := make([][]float32, len(body.Input))
			for i, text := range body.Input {
				vecs[i] = []float32{float32(len(text))}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"vectors": vecs})
		}))
		defer srv.Close()

		emb := embed.NewHTTP(embed.Config{
			BatchSize:   1,
			Concurrency: 2,
			Endpoint:    srv.URL,
			Model:       "fixture-model",
			APIKeyEnv:   "CUSTOM_EMBED_KEY",
		}, srv.Client().Do)
		if emb.Dim() != 0 {
			t.Fatal("dim before embed")
		}
		texts := []string{"a", "bb", "ccc", "dddd", "eeeee", "ffffff"}
		got, err := emb.Embed(context.Background(), texts)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(texts) || emb.Dim() != 1 {
			t.Fatalf("len %d dim %d", len(got), emb.Dim())
		}
		for i, text := range texts {
			if len(got[i]) != 1 || got[i][0] != float32(len(text)) {
				t.Fatalf("result[%d] = %v", i, got[i])
			}
		}
		if maxInflight > 2 {
			t.Fatalf("inflight %d", maxInflight)
		}
		if len(lengths) != len(texts) {
			t.Fatalf("batches %v", lengths)
		}
		for _, n := range lengths {
			if n != 1 {
				t.Fatalf("batch size %d", n)
			}
		}
	})

	t.Run("defaults", func(t *testing.T) {
		t.Setenv("URAG_EMBED_API_KEY", secret)
		var mu sync.Mutex
		var sizes []int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+secret {
				http.Error(w, "auth", http.StatusUnauthorized)
				return
			}
			var body struct {
				Input []string `json:"input"`
				Model string   `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			sizes = append(sizes, len(body.Input))
			mu.Unlock()
			vecs := make([][]float32, len(body.Input))
			for i := range vecs {
				vecs[i] = []float32{1, 0}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"vectors": vecs})
		}))
		defer srv.Close()
		emb := embed.NewHTTP(embed.Config{Endpoint: srv.URL, Model: "m"}, srv.Client().Do)
		texts := make([]string, 33)
		for i := range texts {
			texts[i] = "x"
		}
		got, err := emb.Embed(context.Background(), texts)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 33 || emb.Dim() != 2 {
			t.Fatalf("len %d dim %d", len(got), emb.Dim())
		}
		seen32, seen1 := false, false
		for _, n := range sizes {
			switch n {
			case 32:
				seen32 = true
			case 1:
				seen1 = true
			default:
				t.Fatalf("unexpected batch %d", n)
			}
		}
		if !seen32 || !seen1 {
			t.Fatalf("sizes %v", sizes)
		}
	})

	t.Run("redact", func(t *testing.T) {
		t.Setenv("URAG_EMBED_API_KEY", secret)
		emb := embed.NewHTTP(embed.Config{Endpoint: "http://example.invalid"}, func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("dial " + req.Header.Get("Authorization"))
		})
		_, err := emb.Embed(context.Background(), []string{"hello"})
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatal("error string contains api key")
		}

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "denied "+secret, http.StatusForbidden)
		}))
		defer srv.Close()
		emb = embed.NewHTTP(embed.Config{Endpoint: srv.URL}, srv.Client().Do)
		_, err = emb.Embed(context.Background(), []string{"hello"})
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatal("status error contains api key")
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		t.Setenv("URAG_EMBED_API_KEY", secret)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"vectors": [][]float32{{1}}})
		}))
		defer srv.Close()
		emb := embed.NewHTTP(embed.Config{Endpoint: srv.URL, BatchSize: 8}, srv.Client().Do)
		got, err := emb.Embed(context.Background(), []string{"a", "b"})
		if err == nil || got != nil {
			t.Fatalf("got %v err %v", got, err)
		}
	})
}

func assertZero(t *testing.T, vec embed.Vector) {
	t.Helper()
	for _, v := range vec {
		if v != 0 {
			t.Fatalf("want zero, got %v", vec)
		}
	}
}

func specVector(text string, dim int) embed.Vector {
	vec := make(embed.Vector, dim)
	for _, tok := range embed.Tokens(text) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(tok))
		vec[int(h.Sum64()%uint64(dim))]++
	}
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return vec
	}
	norm := math.Sqrt(sum)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}
