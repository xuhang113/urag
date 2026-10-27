package job

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/clean"
	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/eval"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
	"github.com/xuhang113/urag/internal/storage"
)

func TestNewRejectsMissingDeps(t *testing.T) {
	_, err := New(Config{}, Deps{})
	if err == nil {
		t.Fatal("missing deps returned a pipeline")
	}
}

func TestAcceptUploadStreamsWithinCopyBuf(t *testing.T) {
	lab := newLab(t, Config{Inline: true}, nil)
	payload := bytes.Repeat([]byte("z"), storage.CopyBufSize+128)
	body := &boundedReader{r: bytes.NewReader(payload), max: storage.CopyBufSize}
	acc, err := lab.p.AcceptUpload(context.Background(), UploadInput{
		CollectionID:   "alpha",
		IdempotencyKey: "stream-1",
		Filename:       "wide.txt",
		ContentType:    "text/plain",
		Body:           body,
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])
	if acc.Job.ID == "" {
		t.Fatal("empty job id")
	}
	if acc.Checksum != model.Checksum(want) || acc.Job.Checksum != want {
		t.Fatalf("checksum %s job %s want %s", acc.Checksum, acc.Job.Checksum, want)
	}
	if acc.Size != int64(len(payload)) || acc.ObjectKey == "" {
		t.Fatalf("meta key %s size %d", acc.ObjectKey, acc.Size)
	}
	if acc.Job.State != StateSucceeded {
		t.Fatalf("state %s error %s", acc.Job.State, acc.Job.Error)
	}
	if lab.clean.calls != 1 {
		t.Fatalf("cleaner calls %d", lab.clean.calls)
	}
}

func TestCancelDoesNotRetry(t *testing.T) {
	gate := &gateCleaner{started: make(chan struct{})}
	lab := newLab(t, Config{Inline: false}, gate)
	payload := []byte("cancel me")
	acc, err := lab.p.AcceptUpload(context.Background(), UploadInput{
		CollectionID: "alpha",
		Filename:     "c.txt",
		Body:         bytes.NewReader(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	if acc.Job.State != StateQueued && acc.Job.State != StateRunning {
		t.Fatalf("state %s", acc.Job.State)
	}
	select {
	case <-gate.started:
	case <-time.After(2 * time.Second):
		t.Fatal("cleaner did not start")
	}
	if err := lab.p.Cancel(context.Background(), acc.Job.ID); err != nil {
		t.Fatal(err)
	}
	waitDone(t, lab.p, acc.Job.ID)
	job, err := lab.p.GetJob(context.Background(), acc.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != StateCanceled {
		t.Fatalf("state %s error %s", job.State, job.Error)
	}
	if job.Attempts != 1 || gate.calls() != 1 {
		t.Fatalf("attempts %d cleaner %d", job.Attempts, gate.calls())
	}
}

func TestChecksumMismatchDoesNotRetry(t *testing.T) {
	lab := newLab(t, Config{Inline: true}, nil)
	lab.store.putErr = storage.ErrChecksumMismatch
	_, err := lab.p.AcceptUpload(context.Background(), UploadInput{
		CollectionID: "alpha",
		Filename:     "bad.txt",
		Body:         bytes.NewReader([]byte("nope")),
	})
	if !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatalf("err %v", err)
	}
	if lab.store.puts != 1 {
		t.Fatalf("puts %d", lab.store.puts)
	}
}

func TestIdempotencyUsesStore(t *testing.T) {
	lab := newLab(t, Config{Inline: true}, nil)
	first := bytes.Repeat([]byte("a"), storage.CopyBufSize+32)
	second := bytes.Repeat([]byte("b"), storage.CopyBufSize+32)
	a, err := lab.p.AcceptUpload(context.Background(), UploadInput{
		CollectionID:   "alpha",
		IdempotencyKey: "same-key",
		Filename:       "a.txt",
		Body:           &boundedReader{r: bytes.NewReader(first), max: storage.CopyBufSize},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := lab.p.AcceptUpload(context.Background(), UploadInput{
		CollectionID:   "alpha",
		IdempotencyKey: "same-key",
		Filename:       "b.txt",
		Body:           &boundedReader{r: bytes.NewReader(second), max: storage.CopyBufSize},
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(first)
	want := hex.EncodeToString(sum[:])
	if a.ObjectKey != b.ObjectKey || string(a.Checksum) != want || a.Checksum != b.Checksum {
		t.Fatalf("a %+v b %+v want %s", a, b, want)
	}
	if lab.store.objectCount() != 1 {
		t.Fatalf("objects %d", lab.store.objectCount())
	}
}

func TestDocumentsReturnDuplicates(t *testing.T) {
	lab := newLab(t, Config{Inline: true}, nil)
	body := []byte("duplicate text")
	if _, err := lab.p.AcceptUpload(context.Background(), UploadInput{
		CollectionID:   "col",
		IdempotencyKey: "d1",
		Filename:       "a.txt",
		Body:           bytes.NewReader(body),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := lab.p.AcceptUpload(context.Background(), UploadInput{
		CollectionID:   "col",
		IdempotencyKey: "d2",
		Filename:       "b.txt",
		Body:           bytes.NewReader(body),
	}); err != nil {
		t.Fatal(err)
	}
	docs, err := lab.p.Documents(context.Background(), "col")
	if err != nil {
		t.Fatal(err)
	}
	var first model.DocumentID
	for _, doc := range docs {
		if !doc.Duplicate && doc.ID != "" {
			first = doc.ID
			break
		}
	}
	if first == "" {
		t.Fatalf("docs %+v", docs)
	}
	saw := false
	for _, doc := range docs {
		if doc.Duplicate && doc.Lineage.DuplicateOf == first {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("missing duplicate of %s in %+v", first, docs)
	}
	other, err := lab.p.Documents(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("other collection %+v", other)
	}
}

func TestAskForwardsCollections(t *testing.T) {
	lab := newLab(t, Config{Inline: true}, nil)
	if _, err := lab.reg.Open(context.Background(), "gamma"); err != nil {
		t.Fatal(err)
	}
	lab.reg.resetOpened()
	_, err := lab.p.Ask(context.Background(), retrieve.Request{
		Collections: []model.CollectionID{"alpha", "beta"},
		Query:       "crimson lantern",
		TopK:        4,
		Hybrid:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := lab.reg.openedIDs()
	want := []model.CollectionID{"alpha", "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("opened %v", got)
	}
}

func TestGetJobUnknown(t *testing.T) {
	lab := newLab(t, Config{Inline: true}, nil)
	_, err := lab.p.GetJob(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error")
	}
	if err := lab.p.Cancel(context.Background(), "missing"); err == nil {
		t.Fatal("cancel missing")
	}
}

func TestCheckpointSkipsProcessedObjectKeys(t *testing.T) {
	lab := newLab(t, Config{Inline: true}, nil)
	meta := lab.store.putBytes(t, "alpha", "obj-1", []byte("checkpoint body"))
	rec := lab.p.newRecord(KindUpload, "alpha")
	rec.Job.ObjectKey = string(meta.Key)
	rec.Job.Checksum = string(meta.Checksum)
	rec.filename = "a.txt"
	lab.p.mu.Lock()
	lab.p.jobs[rec.Job.ID] = rec
	lab.p.mu.Unlock()

	ctx := context.Background()
	if err := lab.p.processKey(ctx, rec, meta.Key, "a.txt", ""); err != nil {
		t.Fatal(err)
	}
	if lab.clean.calls != 1 {
		t.Fatalf("calls %d", lab.clean.calls)
	}
	job, err := lab.p.GetJob(ctx, rec.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(job.Checkpoint, []byte(meta.Key)) {
		t.Fatalf("checkpoint %s", job.Checkpoint)
	}
	if err := lab.p.processKey(ctx, rec, meta.Key, "a.txt", ""); err != nil {
		t.Fatal(err)
	}
	if lab.clean.calls != 1 {
		t.Fatalf("reprocessed calls %d", lab.clean.calls)
	}
}

func TestEvaluateContrastDirection(t *testing.T) {
	lab := newLab(t, Config{
		Inline:      true,
		FixturePath: fixturePath(t),
		Weights:     retrieve.Weights{Vector: 0.7, Keyword: 0.3},
	}, nil)
	normal, err := lab.p.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if normal.Direction != "baseline" || normal.RecallDelta != 0 || normal.MRRDelta != 0 {
		t.Fatalf("normal %+v", normal)
	}
	requireBuckets(t, normal.Scores)
	contrast, err := lab.p.EvaluateContrast(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requireBuckets(t, contrast.Scores)
	if contrast.Direction != "down" {
		t.Fatalf("direction %s recall delta %v mrr delta %v scores %+v", contrast.Direction, contrast.RecallDelta, contrast.MRRDelta, contrast.Scores)
	}
	if contrast.RecallDelta >= 0 {
		t.Fatalf("contrast recall delta %v", contrast.RecallDelta)
	}
	for _, score := range contrast.Scores {
		if score.CollectionID == "merged" {
			continue
		}
		if score.RecallAtK > 0.4 {
			t.Fatalf("contrast %s recall %v", score.CollectionID, score.RecallAtK)
		}
	}
	for _, score := range normal.Scores {
		if score.CollectionID == "merged" {
			continue
		}
		if score.RecallAtK < 0.8 || score.MRR < 0.6 {
			t.Fatalf("normal %s recall %v mrr %v", score.CollectionID, score.RecallAtK, score.MRR)
		}
	}
	idx, ok := lab.reg.Get("alpha")
	if !ok {
		t.Fatal("alpha index missing")
	}
	item, ok := idx.(*fakeIndex).items["alpha-lantern#0"]
	if !ok {
		t.Fatal("missing alpha-lantern#0")
	}
	if !strings.Contains(item.chunk.Text, "crimson lantern") {
		t.Fatalf("chunk %s text %q", item.chunk.ID, item.chunk.Text)
	}
}

type lab struct {
	store *fakeStore
	emb   *fakeEmbedder
	reg   *fakeRegistry
	clean *fakeCleaner
	p     *Pipeline
}

func newLab(t *testing.T, cfg Config, cleaner clean.Cleaner) *lab {
	t.Helper()
	l := &lab{
		store: newFakeStore(),
		emb:   &fakeEmbedder{dim: 64},
		reg:   newFakeRegistry(),
		clean: newFakeCleaner(),
	}
	used := cleaner
	if used == nil {
		used = l.clean
	}
	if cfg.FixturePath == "" {
		cfg.FixturePath = fixturePath(t)
	}
	p, err := New(cfg, Deps{
		Store:    l.store,
		Embedder: l.emb,
		Registry: l.reg,
		Cleaner:  used,
		Splitter: chunk.NewParagraph(),
	})
	if err != nil {
		t.Fatal(err)
	}
	l.p = p
	return l
}

func fixturePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "testdata", "recall", "fixture.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitDone(t *testing.T, p *Pipeline, id string) {
	t.Helper()
	p.mu.Lock()
	rec := p.jobs[id]
	p.mu.Unlock()
	if rec == nil || rec.done == nil {
		t.Fatal("missing job")
	}
	select {
	case <-rec.done:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not finish")
	}
}

type boundedReader struct {
	r   io.Reader
	max int
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if len(p) > b.max {
		return 0, errors.New("read exceeds CopyBufSize")
	}
	return b.r.Read(p)
}

type stored struct {
	meta storage.ObjectMeta
	data []byte
}

type fakeStore struct {
	mu      sync.Mutex
	objects map[model.ObjectKey]stored
	byIdem  map[model.IdempotencyKey]storage.ObjectMeta
	puts    int
	putErr  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		objects: map[model.ObjectKey]stored{},
		byIdem:  map[model.IdempotencyKey]storage.ObjectMeta{},
	}
}

func (s *fakeStore) Put(ctx context.Context, key model.ObjectKey, r io.Reader, opts storage.PutOptions) (storage.ObjectMeta, error) {
	s.mu.Lock()
	s.puts++
	if s.putErr != nil {
		err := s.putErr
		s.mu.Unlock()
		return storage.ObjectMeta{}, err
	}
	if opts.IdempotencyKey != "" {
		if meta, ok := s.byIdem[opts.IdempotencyKey]; ok {
			s.mu.Unlock()
			return meta, nil
		}
	}
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return storage.ObjectMeta{}, err
	}
	buf := make([]byte, storage.CopyBufSize)
	h := sha256.New()
	var data []byte
	for {
		if err := ctx.Err(); err != nil {
			return storage.ObjectMeta{}, err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := h.Write(buf[:n]); werr != nil {
				return storage.ObjectMeta{}, werr
			}
			data = append(data, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return storage.ObjectMeta{}, err
		}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if opts.ExpectedChecksum != "" && string(opts.ExpectedChecksum) != sum {
		return storage.ObjectMeta{}, storage.ErrChecksumMismatch
	}
	meta := storage.ObjectMeta{
		Key:            key,
		Size:           int64(len(data)),
		Checksum:       model.Checksum(sum),
		ContentType:    opts.ContentType,
		CollectionID:   opts.CollectionID,
		IdempotencyKey: opts.IdempotencyKey,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if opts.IdempotencyKey != "" {
		if prev, ok := s.byIdem[opts.IdempotencyKey]; ok {
			return prev, nil
		}
		s.byIdem[opts.IdempotencyKey] = meta
	}
	s.objects[key] = stored{meta: meta, data: data}
	return meta, nil
}

func (s *fakeStore) Get(_ context.Context, key model.ObjectKey) (io.ReadCloser, storage.ObjectMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[key]
	if !ok {
		return nil, storage.ObjectMeta{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), obj.data...))), obj.meta, nil
}

func (s *fakeStore) Delete(context.Context, model.ObjectKey) error { return nil }

func (s *fakeStore) Stat(_ context.Context, key model.ObjectKey) (storage.ObjectMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[key]
	if !ok {
		return storage.ObjectMeta{}, storage.ErrNotFound
	}
	return obj.meta, nil
}

func (s *fakeStore) ExistsByChecksum(context.Context, model.Checksum) (storage.ObjectMeta, bool, error) {
	return storage.ObjectMeta{}, false, nil
}

func (s *fakeStore) CreateMultipart(context.Context, model.ObjectKey, storage.PutOptions) (storage.MultipartSession, error) {
	return storage.MultipartSession{}, storage.ErrNotImplemented
}

func (s *fakeStore) UploadPart(context.Context, storage.MultipartSession, int, io.Reader) (storage.Part, error) {
	return storage.Part{}, storage.ErrNotImplemented
}

func (s *fakeStore) ListParts(context.Context, storage.MultipartSession) ([]storage.Part, error) {
	return nil, storage.ErrNotImplemented
}

func (s *fakeStore) CompleteMultipart(context.Context, storage.MultipartSession, []storage.Part) (storage.ObjectMeta, error) {
	return storage.ObjectMeta{}, storage.ErrNotImplemented
}

func (s *fakeStore) AbortMultipart(context.Context, storage.MultipartSession) error {
	return storage.ErrNotImplemented
}

func (s *fakeStore) objectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

func (s *fakeStore) putBytes(t *testing.T, col model.CollectionID, idem string, payload []byte) storage.ObjectMeta {
	t.Helper()
	meta, err := s.Put(context.Background(), model.ObjectKey("raw/"+idem), bytes.NewReader(payload), storage.PutOptions{
		CollectionID:   col,
		IdempotencyKey: model.IdempotencyKey(idem),
	})
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

type fakeEmbedder struct {
	dim int
}

func (e *fakeEmbedder) Dim() int {
	if e.dim <= 0 {
		return 64
	}
	return e.dim
}

func (e *fakeEmbedder) Embed(_ context.Context, texts []string) ([]embed.Vector, error) {
	out := make([]embed.Vector, len(texts))
	for i, text := range texts {
		out[i] = embedText(text, e.Dim())
	}
	return out, nil
}

func embedText(text string, dim int) embed.Vector {
	vec := make(embed.Vector, dim)
	for _, word := range tokenize(text) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(word))
		vec[int(h.Sum64()%uint64(dim))]++
	}
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return vec
	}
	norm := float32(math.Sqrt(sum))
	for i := range vec {
		vec[i] /= norm
	}
	return vec
}

type fakeRegistry struct {
	mu     sync.Mutex
	idx    map[model.CollectionID]*fakeIndex
	opened []model.CollectionID
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{idx: map[model.CollectionID]*fakeIndex{}}
}

func (r *fakeRegistry) Open(_ context.Context, id model.CollectionID) (index.Index, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opened = append(r.opened, id)
	idx, ok := r.idx[id]
	if !ok {
		idx = &fakeIndex{col: id, items: map[model.ChunkID]indexed{}}
		r.idx[id] = idx
	}
	return idx, nil
}

func (r *fakeRegistry) Get(id model.CollectionID) (index.Index, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx, ok := r.idx[id]
	return idx, ok
}

func (r *fakeRegistry) resetOpened() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opened = nil
}

func (r *fakeRegistry) openedIDs() []model.CollectionID {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]model.CollectionID(nil), r.opened...)
	return out
}

type indexed struct {
	chunk model.Chunk
	vec   embed.Vector
}

type fakeIndex struct {
	col   model.CollectionID
	mu    sync.Mutex
	items map[model.ChunkID]indexed
}

func (idx *fakeIndex) Collection() model.CollectionID { return idx.col }

func (idx *fakeIndex) Upsert(_ context.Context, chunks []model.Chunk, vectors []embed.Vector) error {
	if len(chunks) != len(vectors) {
		return errors.New("len")
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for i, ch := range chunks {
		if ch.CollectionID != idx.col {
			return errors.New("collection")
		}
		idx.items[ch.ID] = indexed{chunk: ch, vec: append(embed.Vector(nil), vectors[i]...)}
	}
	return nil
}

func (idx *fakeIndex) SearchVector(_ context.Context, q index.Query) ([]index.Hit, error) {
	return idx.search(q, true)
}

func (idx *fakeIndex) SearchKeyword(_ context.Context, q index.Query) ([]index.Hit, error) {
	return idx.search(q, false)
}

func (idx *fakeIndex) search(q index.Query, vector bool) ([]index.Hit, error) {
	if q.CollectionID != idx.col {
		return nil, errors.New("wrong collection")
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	hits := make([]index.Hit, 0, len(idx.items))
	for _, item := range idx.items {
		var score float64
		if vector {
			score = cosine(q.Vector, item.vec)
		} else {
			score = keywordOverlap(q.Tokens, tokenize(item.chunk.Text))
		}
		hits = append(hits, index.Hit{
			Chunk:        item.chunk,
			Score:        score,
			CollectionID: idx.col,
		})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].Chunk.ID < hits[j].Chunk.ID
		}
		return hits[i].Score > hits[j].Score
	})
	if q.TopK > 0 && len(hits) > q.TopK {
		hits = hits[:q.TopK]
	}
	return hits, nil
}

func cosine(a, b embed.Vector) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func keywordOverlap(query, chunk []string) float64 {
	if len(query) == 0 {
		return 0
	}
	set := make(map[string]struct{}, len(chunk))
	for _, tok := range chunk {
		set[tok] = struct{}{}
	}
	n := 0
	seen := map[string]struct{}{}
	for _, tok := range query {
		if _, ok := seen[tok]; ok {
			continue
		}
		seen[tok] = struct{}{}
		if _, ok := set[tok]; ok {
			n++
		}
	}
	return float64(n) / float64(len(unique(query)))
}

func unique(toks []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, tok := range toks {
		if _, ok := seen[tok]; ok {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	return out
}

type fakeCleaner struct {
	mu    sync.Mutex
	seen  map[model.CollectionID]map[string]model.DocumentID
	seq   int
	calls int
}

func newFakeCleaner() *fakeCleaner {
	return &fakeCleaner{seen: map[model.CollectionID]map[string]model.DocumentID{}}
}

func (c *fakeCleaner) Clean(_ context.Context, in clean.Input) (model.Document, error) {
	buf := make([]byte, storage.CopyBufSize)
	var data []byte
	for {
		n, err := in.Body.Read(buf)
		data = append(data, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return model.Document{}, err
		}
	}
	text := string(data)
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	col := c.seen[in.CollectionID]
	if col == nil {
		col = map[string]model.DocumentID{}
		c.seen[in.CollectionID] = col
	}
	if prev, ok := col[hash]; ok {
		return model.Document{
			ID:           model.DocumentID(hash[:12]),
			CollectionID: in.CollectionID,
			Text:         text,
			Duplicate:    true,
			Lineage: model.Lineage{
				RawObjectKey: in.ObjectKey,
				DocumentID:   model.DocumentID(hash[:12]),
				CollectionID: in.CollectionID,
				ContentHash:  hash,
				DuplicateOf:  prev,
				SourceURI:    in.SourceURI,
				MediaType:    "txt",
				Encoding:     "utf-8",
				Steps: []model.CleanStep{
					{Name: "detect", Detail: "txt"},
					{Name: "strip", Detail: "plain"},
					{Name: "dedupe", Detail: string(prev)},
				},
			},
		}, nil
	}
	c.seq++
	id := model.DocumentID("doc-" + itoa(c.seq))
	col[hash] = id
	return model.Document{
		ID:           id,
		CollectionID: in.CollectionID,
		Text:         text,
		Lineage: model.Lineage{
			RawObjectKey: in.ObjectKey,
			DocumentID:   id,
			CollectionID: in.CollectionID,
			ContentHash:  hash,
			SourceURI:    in.SourceURI,
			MediaType:    "txt",
			Encoding:     "utf-8",
		},
	}, nil
}

type gateCleaner struct {
	mu      sync.Mutex
	n       int
	started chan struct{}
}

func (g *gateCleaner) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

func (g *gateCleaner) Clean(ctx context.Context, _ clean.Input) (model.Document, error) {
	g.mu.Lock()
	g.n++
	n := g.n
	g.mu.Unlock()
	if n == 1 {
		close(g.started)
	}
	<-ctx.Done()
	return model.Document{}, ctx.Err()
}

func TestContrastChunksDropWholeWords(t *testing.T) {
	doc := model.Document{
		ID:           "d",
		CollectionID: "c",
		Text:         "The crimson lantern festival is held. paper lanterns stay.",
	}
	chunks := contrastChunks(doc, []string{"Crimson", "LANTERN"})
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	var joined strings.Builder
	for _, ch := range chunks {
		if len(ch.Text) > 20 {
			t.Fatalf("chunk %d bytes: %q", len(ch.Text), ch.Text)
		}
		if ch.Text != doc.Text[ch.StartByte:ch.EndByte] {
			t.Fatalf("span %q != %q", doc.Text[ch.StartByte:ch.EndByte], ch.Text)
		}
		for _, word := range tokenize(ch.Text) {
			if word == "crimson" || word == "lantern" {
				t.Fatalf("kept %s in %q", word, ch.Text)
			}
		}
		joined.WriteString(ch.Text)
	}
	if !strings.Contains(joined.String(), "festival") || !strings.Contains(joined.String(), "lanterns") {
		t.Fatalf("over-dropped: %s", joined.String())
	}
}

func requireBuckets(t *testing.T, scores []eval.Score) {
	t.Helper()
	seen := map[string]bool{}
	for _, score := range scores {
		seen[score.CollectionID] = true
	}
	for _, id := range []string{"alpha", "beta", "merged"} {
		if !seen[id] {
			t.Fatalf("missing %s in %+v", id, scores)
		}
	}
	if seen["gamma"] {
		t.Fatalf("gamma was scored: %+v", scores)
	}
}
