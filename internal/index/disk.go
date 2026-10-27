package index

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/model"
)

const (
	diskMagic      = "URAGVEC1"
	metaFile       = "meta.json"
	recordsFile    = "records.bin"
	writeBatchSize = 32
	maxStringBytes = 32 << 20
)

// ErrModelMismatch 表示磁盘上的模型名或维度与这次打开的不一致。
// 出现这个错误时必须停止搜索，不能用旧向量继续查。
var ErrModelMismatch = errors.New("index: model meta mismatch")

// VectorRecord 是磁盘索引里的一条子块。
// 向量只在写入和按批扫描时出现，搜索结果不保留向量。
type VectorRecord struct {
	ChunkID      string
	ParentID     int64
	DocID        int64
	Version      int
	CollectionID model.CollectionID
	HeadingPath  string
	Page         int
	Body         string
	Vector       embed.Vector
}

type diskMeta struct {
	ModelName string `json:"model_name"`
	Dim       int    `json:"dim"`
}

// Disk 是按集合分目录的磁盘索引。
// 一个集合一个目录。搜索时复用一条向量缓冲，不把全部向量留在内存里。
type Disk struct {
	root string
	meta embed.ModelMeta

	mu    sync.Mutex
	open  map[model.CollectionID]*diskIndex
	locks map[model.CollectionID]*sync.RWMutex
}

type diskIndex struct {
	id   model.CollectionID
	disk *Disk
}

type storedRecord struct {
	rec        VectorRecord
	documentID string
	startByte  int
	endByte    int
	ordinal    int
}

// NewDisk 打开 root 上的磁盘索引。
// 目录里还没有元数据时，把 meta 的模型名和维度写进去。
// 已有元数据与 meta 不一致时返回 ErrModelMismatch，不搜索旧向量。
func NewDisk(root string, meta embed.ModelMeta) (Registry, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("index: empty disk root")
	}
	if meta.Dim <= 0 {
		return nil, fmt.Errorf("index: model dimension %d", meta.Dim)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(root, metaFile)
	if _, err := os.Stat(path); err == nil {
		got, err := readMeta(path)
		if err != nil {
			return nil, err
		}
		if got.Name != meta.Name || got.Dim != meta.Dim {
			return nil, modelMismatch(got, meta)
		}
	} else if os.IsNotExist(err) {
		if err := writeMeta(path, meta); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	return &Disk{
		root:  root,
		meta:  meta,
		open:  make(map[model.CollectionID]*diskIndex),
		locks: make(map[model.CollectionID]*sync.RWMutex),
	}, nil
}

func (d *Disk) Open(ctx context.Context, id model.CollectionID) (Index, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validCollection(id); err != nil {
		return nil, err
	}
	if err := d.confirmMeta(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	if idx, ok := d.open[id]; ok {
		d.mu.Unlock()
		return idx, nil
	}
	d.mu.Unlock()

	mu := d.fileMu(id)
	mu.Lock()
	dir, err := d.collectionDir(id)
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		mu.Unlock()
		return nil, err
	}
	if err := ensureCollectionMeta(dir, d.meta); err != nil {
		mu.Unlock()
		return nil, err
	}
	mu.Unlock()

	d.mu.Lock()
	defer d.mu.Unlock()
	if idx, ok := d.open[id]; ok {
		return idx, nil
	}
	idx := &diskIndex{id: id, disk: d}
	d.open[id] = idx
	return idx, nil
}

func (d *Disk) Get(id model.CollectionID) (Index, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	idx, ok := d.open[id]
	if !ok {
		return nil, false
	}
	return idx, true
}

// UpsertRecords 按集合把记录写进各自的目录。
// 同一批里同一个片段以后出现的为准。向量按 writeBatchSize 条一批写入。
// 向量长度必须等于打开时的维度，否则整批不写。
func (d *Disk) UpsertRecords(ctx context.Context, recs []VectorRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := d.confirmMeta(); err != nil {
		return err
	}
	grouped := make(map[model.CollectionID][]storedRecord)
	order := make([]model.CollectionID, 0)
	for _, rec := range recs {
		if err := validCollection(rec.CollectionID); err != nil {
			return err
		}
		if rec.ChunkID == "" {
			return errors.New("index: empty chunk id")
		}
		if len(rec.Vector) != d.meta.Dim {
			return fmt.Errorf("index: vector length %d, want %d", len(rec.Vector), d.meta.Dim)
		}
		if _, ok := grouped[rec.CollectionID]; !ok {
			order = append(order, rec.CollectionID)
		}
		grouped[rec.CollectionID] = append(grouped[rec.CollectionID], storedRecord{
			rec:        rec,
			documentID: strconv.FormatInt(rec.DocID, 10),
		})
	}
	for _, id := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.writeStored(ctx, id, grouped[id]); err != nil {
			return err
		}
	}
	return nil
}

func (m *diskIndex) Collection() model.CollectionID { return m.id }

func (m *diskIndex) Upsert(ctx context.Context, chunks []model.Chunk, vectors []embed.Vector) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunks) != len(vectors) {
		return fmt.Errorf("index: %d chunks and %d vectors", len(chunks), len(vectors))
	}
	if len(chunks) == 0 {
		return nil
	}
	recs := make([]storedRecord, len(chunks))
	for i, chunk := range chunks {
		if chunk.CollectionID != m.id {
			return fmt.Errorf("index: chunk %s belongs to %s", chunk.ID, chunk.CollectionID)
		}
		if len(vectors[i]) != m.disk.meta.Dim {
			return fmt.Errorf("index: vector length %d, want %d", len(vectors[i]), m.disk.meta.Dim)
		}
		recs[i] = storedFromChunk(chunk, vectors[i])
	}
	return m.disk.writeStored(ctx, m.id, recs)
}

func (m *diskIndex) SearchVector(ctx context.Context, q Query) ([]Hit, error) {
	if err := m.check(ctx, q); err != nil {
		return nil, err
	}
	return m.search(ctx, q, func(_ storedRecord, vec []float32) float64 {
		return cosine(q.Vector, embed.Vector(vec))
	})
}

func (m *diskIndex) SearchKeyword(ctx context.Context, q Query) ([]Hit, error) {
	if err := m.check(ctx, q); err != nil {
		return nil, err
	}
	return m.search(ctx, q, func(rec storedRecord, _ []float32) float64 {
		return keywordScore(q.Tokens, rec.rec.Body)
	})
}

func (m *diskIndex) check(ctx context.Context, q Query) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if q.CollectionID != m.id {
		return fmt.Errorf("index: collection %s is not %s", q.CollectionID, m.id)
	}
	return nil
}

func (m *diskIndex) search(ctx context.Context, q Query, scoreOf func(storedRecord, []float32) float64) ([]Hit, error) {
	if err := m.disk.confirmMeta(); err != nil {
		return nil, err
	}
	if q.TopK <= 0 {
		return []Hit{}, nil
	}
	hits := make([]Hit, 0)
	pos := make(map[string]int)
	err := m.scan(ctx, func(rec storedRecord, vec []float32) error {
		if rec.rec.CollectionID != m.id {
			return nil
		}
		score := scoreOf(rec, vec)
		id := rec.rec.ChunkID
		if score <= 0 || math.IsNaN(score) {
			if i, ok := pos[id]; ok {
				hits[i].Score = math.NaN()
			}
			return nil
		}
		hit := hitFromStored(m.id, rec, score)
		if i, ok := pos[id]; ok {
			hits[i] = hit
			return nil
		}
		pos[id] = len(hits)
		hits = append(hits, hit)
		return nil
	})
	if err != nil {
		return nil, err
	}
	kept := hits[:0]
	for _, hit := range hits {
		if hit.Score <= 0 || math.IsNaN(hit.Score) {
			continue
		}
		kept = append(kept, hit)
	}
	hits = kept
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].Chunk.ID < hits[j].Chunk.ID
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > q.TopK {
		hits = hits[:q.TopK]
	}
	return hits, nil
}

func hitFromStored(id model.CollectionID, rec storedRecord, score float64) Hit {
	docID := rec.documentID
	if docID == "" {
		docID = strconv.FormatInt(rec.rec.DocID, 10)
	}
	return Hit{
		Chunk: model.Chunk{
			ID:           model.ChunkID(rec.rec.ChunkID),
			DocumentID:   model.DocumentID(docID),
			CollectionID: rec.rec.CollectionID,
			Ordinal:      rec.ordinal,
			Text:         rec.rec.Body,
			StartByte:    rec.startByte,
			EndByte:      rec.endByte,
		},
		Score:        score,
		CollectionID: id,
		ParentID:     rec.rec.ParentID,
		HeadingPath:  rec.rec.HeadingPath,
	}
}

func (m *diskIndex) scan(ctx context.Context, fn func(storedRecord, []float32) error) error {
	dir, err := m.disk.collectionDir(m.id)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, recordsFile)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	got, err := readMeta(filepath.Join(dir, metaFile))
	if err != nil {
		return err
	}
	if got.Name != m.disk.meta.Name || got.Dim != m.disk.meta.Dim {
		return modelMismatch(got, m.disk.meta)
	}
	mu := m.disk.fileMu(m.id)
	mu.RLock()
	defer mu.RUnlock()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	header, err := readHeader(f)
	if err != nil {
		return err
	}
	if header.Name != m.disk.meta.Name || header.Dim != m.disk.meta.Dim {
		return modelMismatch(header, m.disk.meta)
	}
	vec := make([]float32, m.disk.meta.Dim)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, err := decodeRecord(f, vec)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(rec, vec); err != nil {
			return err
		}
	}
}

func (d *Disk) writeStored(ctx context.Context, id model.CollectionID, incoming []storedRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validCollection(id); err != nil {
		return err
	}
	for _, rec := range incoming {
		if rec.rec.ChunkID == "" {
			return errors.New("index: empty chunk id")
		}
		if rec.rec.CollectionID != id {
			return fmt.Errorf("index: chunk %s belongs to %s", rec.rec.ChunkID, rec.rec.CollectionID)
		}
		if len(rec.rec.Vector) != d.meta.Dim {
			return fmt.Errorf("index: vector length %d, want %d", len(rec.rec.Vector), d.meta.Dim)
		}
	}
	incoming = dedupeStored(incoming)
	dir, err := d.collectionDir(id)
	if err != nil {
		return err
	}
	mu := d.fileMu(id)
	mu.Lock()
	defer mu.Unlock()
	if err := d.confirmMeta(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := ensureCollectionMeta(dir, d.meta); err != nil {
		return err
	}
	finalPath := filepath.Join(dir, recordsFile)
	tmp := finalPath + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished {
			out.Close()
			os.Remove(tmp)
		}
	}()
	if err := writeHeader(out, d.meta); err != nil {
		return err
	}
	skip := make(map[string]struct{}, len(incoming))
	for _, rec := range incoming {
		skip[rec.rec.ChunkID] = struct{}{}
	}
	if err := streamCopy(ctx, finalPath, out, d.meta, skip); err != nil {
		return err
	}
	if err := writeRecordBatches(out, incoming); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, finalPath); err != nil {
		return err
	}
	finished = true
	return nil
}

func streamCopy(ctx context.Context, src string, dst io.Writer, meta embed.ModelMeta, skip map[string]struct{}) error {
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer in.Close()
	header, err := readHeader(in)
	if err != nil {
		return err
	}
	if header.Name != meta.Name || header.Dim != meta.Dim {
		return modelMismatch(header, meta)
	}
	vec := make([]float32, meta.Dim)
	var buf []byte
	batch := 0
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		if _, err := dst.Write(buf); err != nil {
			return err
		}
		buf = buf[:0]
		batch = 0
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, err := decodeRecord(in, vec)
		if err == io.EOF {
			return flush()
		}
		if err != nil {
			return err
		}
		if _, drop := skip[rec.rec.ChunkID]; drop {
			continue
		}
		encoded, err := encodeRecord(rec, vec)
		if err != nil {
			return err
		}
		buf = append(buf, encoded...)
		batch++
		if batch == writeBatchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
}

func writeRecordBatches(dst io.Writer, recs []storedRecord) error {
	var buf []byte
	batch := 0
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		if _, err := dst.Write(buf); err != nil {
			return err
		}
		buf = buf[:0]
		batch = 0
		return nil
	}
	for _, rec := range recs {
		encoded, err := encodeRecord(rec, rec.rec.Vector)
		if err != nil {
			return err
		}
		buf = append(buf, encoded...)
		batch++
		if batch == writeBatchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func (d *Disk) confirmMeta() error {
	got, err := readMeta(filepath.Join(d.root, metaFile))
	if err != nil {
		return err
	}
	if got.Name != d.meta.Name || got.Dim != d.meta.Dim {
		return modelMismatch(got, d.meta)
	}
	return nil
}

func (d *Disk) fileMu(id model.CollectionID) *sync.RWMutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.locks == nil {
		d.locks = make(map[model.CollectionID]*sync.RWMutex)
	}
	mu, ok := d.locks[id]
	if !ok {
		mu = &sync.RWMutex{}
		d.locks[id] = mu
	}
	return mu
}

func (d *Disk) collectionDir(id model.CollectionID) (string, error) {
	if err := validCollection(id); err != nil {
		return "", err
	}
	return filepath.Join(d.root, "collections", string(id)), nil
}

func validCollection(id model.CollectionID) error {
	s := string(id)
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("index: invalid collection %q", id)
	}
	return nil
}

func storedFromChunk(chunk model.Chunk, vec embed.Vector) storedRecord {
	var docID int64
	text := string(chunk.DocumentID)
	if n, err := strconv.ParseInt(text, 10, 64); err == nil && strconv.FormatInt(n, 10) == text {
		docID = n
	}
	return storedRecord{
		rec: VectorRecord{
			ChunkID:      string(chunk.ID),
			DocID:        docID,
			CollectionID: chunk.CollectionID,
			Body:         chunk.Text,
			Vector:       vec,
		},
		documentID: text,
		startByte:  chunk.StartByte,
		endByte:    chunk.EndByte,
		ordinal:    chunk.Ordinal,
	}
}

func dedupeStored(in []storedRecord) []storedRecord {
	pos := make(map[string]int, len(in))
	out := make([]storedRecord, 0, len(in))
	for _, rec := range in {
		if i, ok := pos[rec.rec.ChunkID]; ok {
			out[i] = rec
			continue
		}
		pos[rec.rec.ChunkID] = len(out)
		out = append(out, rec)
	}
	return out
}

func modelMismatch(stored, open embed.ModelMeta) error {
	return fmt.Errorf("%w: stored name %q dim %d, open name %q dim %d; refusing to search old vectors",
		ErrModelMismatch, stored.Name, stored.Dim, open.Name, open.Dim)
}

func readMeta(path string) (embed.ModelMeta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return embed.ModelMeta{}, err
	}
	var got diskMeta
	if err := json.Unmarshal(raw, &got); err != nil {
		return embed.ModelMeta{}, fmt.Errorf("index: meta %s: %w", path, err)
	}
	return embed.ModelMeta{Name: got.ModelName, Dim: got.Dim}, nil
}

func writeMeta(path string, meta embed.ModelMeta) error {
	raw, err := json.MarshalIndent(diskMeta{ModelName: meta.Name, Dim: meta.Dim}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ensureCollectionMeta(dir string, meta embed.ModelMeta) error {
	path := filepath.Join(dir, metaFile)
	if _, err := os.Stat(path); err == nil {
		got, err := readMeta(path)
		if err != nil {
			return err
		}
		if got.Name != meta.Name || got.Dim != meta.Dim {
			return modelMismatch(got, meta)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeMeta(path, meta)
}

// records.bin 文件头：8 字节魔数 URAGVEC1，随后是小端 uint32 维度，再是模型名。
func writeHeader(w io.Writer, meta embed.ModelMeta) error {
	if _, err := io.WriteString(w, diskMagic); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(meta.Dim)); err != nil {
		return err
	}
	return writeString(w, meta.Name)
}

func readHeader(r io.Reader) (embed.ModelMeta, error) {
	magic := make([]byte, len(diskMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return embed.ModelMeta{}, err
	}
	if string(magic) != diskMagic {
		return embed.ModelMeta{}, errors.New("index: bad vector file")
	}
	var dim uint32
	if err := binary.Read(r, binary.LittleEndian, &dim); err != nil {
		return embed.ModelMeta{}, err
	}
	name, err := readString(r)
	if err != nil {
		return embed.ModelMeta{}, err
	}
	return embed.ModelMeta{Name: name, Dim: int(dim)}, nil
}

func encodeRecord(rec storedRecord, vec []float32) ([]byte, error) {
	for _, s := range []string{rec.rec.ChunkID, string(rec.rec.CollectionID), rec.rec.HeadingPath, rec.rec.Body, rec.documentID} {
		if len(s) > maxStringBytes {
			return nil, fmt.Errorf("index: string length %d", len(s))
		}
	}
	page, err := int32Checked(rec.rec.Page)
	if err != nil {
		return nil, err
	}
	ordinal, err := int32Checked(rec.ordinal)
	if err != nil {
		return nil, err
	}
	raw := appendFloat32(nil, vec)
	raw = appendString(raw, rec.rec.ChunkID)
	raw = appendInt64(raw, rec.rec.ParentID)
	raw = appendInt64(raw, rec.rec.DocID)
	raw = appendInt64(raw, int64(rec.rec.Version))
	raw = appendString(raw, string(rec.rec.CollectionID))
	raw = appendString(raw, rec.rec.HeadingPath)
	raw = appendInt32(raw, page)
	raw = appendString(raw, rec.rec.Body)
	raw = appendString(raw, rec.documentID)
	raw = appendInt64(raw, int64(rec.startByte))
	raw = appendInt64(raw, int64(rec.endByte))
	raw = appendInt32(raw, ordinal)
	return raw, nil
}

func decodeRecord(r io.Reader, vec []float32) (storedRecord, error) {
	if err := binary.Read(r, binary.LittleEndian, vec); err != nil {
		return storedRecord{}, err
	}
	var rec storedRecord
	var err error
	if rec.rec.ChunkID, err = readString(r); err != nil {
		return storedRecord{}, err
	}
	if rec.rec.ParentID, err = readInt64(r); err != nil {
		return storedRecord{}, err
	}
	if rec.rec.DocID, err = readInt64(r); err != nil {
		return storedRecord{}, err
	}
	version, err := readInt64(r)
	if err != nil {
		return storedRecord{}, err
	}
	rec.rec.Version = int(version)
	collection, err := readString(r)
	if err != nil {
		return storedRecord{}, err
	}
	rec.rec.CollectionID = model.CollectionID(collection)
	if rec.rec.HeadingPath, err = readString(r); err != nil {
		return storedRecord{}, err
	}
	page, err := readInt32(r)
	if err != nil {
		return storedRecord{}, err
	}
	rec.rec.Page = int(page)
	if rec.rec.Body, err = readString(r); err != nil {
		return storedRecord{}, err
	}
	if rec.documentID, err = readString(r); err != nil {
		return storedRecord{}, err
	}
	start, err := readInt64(r)
	if err != nil {
		return storedRecord{}, err
	}
	end, err := readInt64(r)
	if err != nil {
		return storedRecord{}, err
	}
	ordinal, err := readInt32(r)
	if err != nil {
		return storedRecord{}, err
	}
	rec.startByte = int(start)
	rec.endByte = int(end)
	rec.ordinal = int(ordinal)
	return rec, nil
}

func appendFloat32(dst []byte, vec []float32) []byte {
	for _, v := range vec {
		dst = binary.LittleEndian.AppendUint32(dst, math.Float32bits(v))
	}
	return dst
}

func appendInt64(dst []byte, v int64) []byte {
	return binary.LittleEndian.AppendUint64(dst, uint64(v))
}

func appendInt32(dst []byte, v int32) []byte {
	return binary.LittleEndian.AppendUint32(dst, uint32(v))
}

func appendString(dst []byte, s string) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(s)))
	return append(dst, s...)
}

func writeString(w io.Writer, s string) error {
	if len(s) > maxStringBytes {
		return fmt.Errorf("index: string length %d", len(s))
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(s))); err != nil {
		return err
	}
	_, err := io.WriteString(w, s)
	return err
}

func readString(r io.Reader) (string, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}
	if n > maxStringBytes {
		return "", fmt.Errorf("index: string length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func readInt64(r io.Reader) (int64, error) {
	var v int64
	if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
		return 0, err
	}
	return v, nil
}

func readInt32(r io.Reader) (int32, error) {
	var v int32
	if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
		return 0, err
	}
	return v, nil
}

var (
	_ Registry = (*Disk)(nil)
	_ Index    = (*diskIndex)(nil)
)

func int32Checked(n int) (int32, error) {
	if n > math.MaxInt32 || n < math.MinInt32 {
		return 0, fmt.Errorf("index: value %d out of range", n)
	}
	return int32(n), nil
}
