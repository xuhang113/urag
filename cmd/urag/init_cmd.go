package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/xuhang113/urag/internal/catalog"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
)

const (
	fixtureSourcePrefix = "fixture://urag/"
	fixtureMarker       = "uragfixture"
	bootstrapFixtureRel = "testdata/bootstrap/fixture.json"
)

type bootstrapFixture struct {
	CollectionID string         `json:"collection_id"`
	ProbeQuery   string         `json:"probe_query"`
	Documents    []bootstrapDoc `json:"documents"`
}

type bootstrapDoc struct {
	SourceURI   string `json:"source_uri"`
	Title       string `json:"title"`
	HeadingPath string `json:"heading_path"`
	Body        string `json:"body"`
}

// initDB 执行仓库里的 schema.sql，并写入 testdata/bootstrap/fixture.json。
// 只替换 fixture://urag/ 这几行，不读取、不删除生产库的其他来源。
func initDB(args []string) error {
	if len(args) != 0 {
		return errors.New("urag: init-db 不接收参数")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fx, err := loadRepoFixture()
	if err != nil {
		return err
	}
	db, err := openCatalog(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, doc := range fx.Documents {
		sum := sha256.Sum256([]byte(doc.Body))
		docID, err := db.PutDocument(ctx, catalog.Document{
			CollectionID: fx.CollectionID,
			Version:      1,
			SourceURI:    doc.SourceURI,
			Checksum:     hex.EncodeToString(sum[:]),
			MediaType:    "text/plain",
			License:      "fixture",
			Title:        doc.Title,
			FetchedAt:    time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			return err
		}
		if err := db.ReplaceSections(ctx, docID, []catalog.ParentWithChildren{{
			Parent: catalog.Parent{
				HeadingPath: doc.HeadingPath,
				PageFrom:    1,
				PageTo:      1,
				Body:        doc.Body,
			},
			Children: []catalog.Child{{
				Ordinal:     0,
				StartByte:   0,
				EndByte:     len(doc.Body),
				HeadingPath: doc.HeadingPath,
				Page:        1,
				Body:        doc.Body,
			}},
		}}); err != nil {
			return err
		}
	}
	fmt.Printf("台账夹具写入 %d 篇，集合 %s\n", len(fx.Documents), fx.CollectionID)
	return nil
}

// initES 确保索引 public 使用生产映射（smartcn、BM25），并写入同一份台账夹具。
// 索引已存在时不删除、不重建。不嵌入，不读取向量，也不收录。
func initES(args []string) error {
	if len(args) != 0 {
		return errors.New("urag: init-es 不接收参数")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fx, err := loadRepoFixture()
	if err != nil {
		return err
	}
	addr, err := elasticAddr()
	if err != nil {
		return err
	}
	if err := waitElastic(ctx, addr); err != nil {
		return err
	}
	db, err := openCatalog(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.ListBySourcePrefix(ctx, fixtureSourcePrefix)
	if err != nil {
		return err
	}
	if len(rows) != len(fx.Documents) {
		return fmt.Errorf("urag: 台账夹具 %d 条，fixture 文件 %d 篇，先执行 init-db", len(rows), len(fx.Documents))
	}
	recs := make([]index.VectorRecord, 0, len(rows))
	for _, row := range rows {
		if row.CollectionID != fx.CollectionID || !strings.HasPrefix(row.SourceURI, fixtureSourcePrefix) {
			return errors.New("urag: 夹具行不属于 public 的 fixture 来源")
		}
		if !strings.Contains(row.Body, fixtureMarker) {
			return errors.New("urag: 夹具正文缺少标记")
		}
		recs = append(recs, index.VectorRecord{
			ChunkID:      row.ChunkID,
			ParentID:     row.ParentID,
			DocID:        row.DocID,
			Version:      row.Version,
			CollectionID: model.CollectionID(row.CollectionID),
			HeadingPath:  row.HeadingPath,
			Page:         row.Page,
			Body:         row.Body,
			StartByte:    row.StartByte,
			EndByte:      row.EndByte,
		})
	}
	keywords, err := index.NewElastic(addr)
	if err != nil {
		return err
	}
	if err := keywords.UpsertRecords(ctx, recs); err != nil {
		_ = keywords.Close()
		return err
	}
	if err := keywords.Close(); err != nil {
		return err
	}
	if err := searchFixture(ctx, addr, fx); err != nil {
		return err
	}
	fmt.Printf("Elasticsearch 索引 %s 写入 %d 条夹具。缺索引时按 smartcn 与 BM25 创建，已有索引不重建。\n", fx.CollectionID, len(recs))
	return nil
}

func searchFixture(ctx context.Context, addr string, fx bootstrapFixture) error {
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		keywords, err := index.NewElastic(addr)
		if err != nil {
			return err
		}
		idx, err := keywords.Open(ctx, model.CollectionID(fx.CollectionID))
		if err != nil {
			_ = keywords.Close()
			last = err
		} else {
			hits, err := idx.SearchKeyword(ctx, index.Query{
				CollectionID: model.CollectionID(fx.CollectionID),
				Text:         fx.ProbeQuery,
				TopK:         10,
				Rank:         index.RankBM25,
			})
			_ = keywords.Close()
			if err != nil {
				last = err
			} else {
				for _, hit := range hits {
					if strings.Contains(hit.Chunk.Text, fx.ProbeQuery) {
						return nil
					}
				}
				last = errors.New("urag: 夹具查询没有命中")
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if last == nil {
		return errors.New("urag: 夹具查询没有命中")
	}
	return last
}

func loadRepoFixture() (bootstrapFixture, error) {
	root, err := repoRoot()
	if err != nil {
		return bootstrapFixture{}, err
	}
	return loadBootstrapFixture(filepath.Join(root, bootstrapFixtureRel))
}

func loadBootstrapFixture(path string) (bootstrapFixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return bootstrapFixture{}, fmt.Errorf("urag: 读取夹具: %w", err)
	}
	var fx bootstrapFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		return bootstrapFixture{}, fmt.Errorf("urag: 解析夹具: %w", err)
	}
	if err := validateBootstrapFixture(fx); err != nil {
		return bootstrapFixture{}, err
	}
	return fx, nil
}

func validateBootstrapFixture(fx bootstrapFixture) error {
	if fx.CollectionID != "public" {
		return errors.New("urag: 夹具集合必须是 public")
	}
	probe := strings.TrimSpace(fx.ProbeQuery)
	if probe == "" || !strings.Contains(probe, fixtureMarker) {
		return errors.New("urag: 夹具查询缺少标记")
	}
	if len(fx.Documents) == 0 || len(fx.Documents) > 8 {
		return errors.New("urag: 夹具篇数必须在 1 到 8 之间")
	}
	seen := map[string]struct{}{}
	foundProbe := false
	for _, doc := range fx.Documents {
		if !strings.HasPrefix(doc.SourceURI, fixtureSourcePrefix) {
			return fmt.Errorf("urag: 夹具来源 %s 必须以 %s 开头", doc.SourceURI, fixtureSourcePrefix)
		}
		if _, ok := seen[doc.SourceURI]; ok {
			return fmt.Errorf("urag: 夹具来源重复 %s", doc.SourceURI)
		}
		seen[doc.SourceURI] = struct{}{}
		if strings.TrimSpace(doc.Title) == "" || strings.TrimSpace(doc.HeadingPath) == "" {
			return errors.New("urag: 夹具缺少标题或路径")
		}
		if !strings.Contains(doc.Body, fixtureMarker) || len(doc.Body) > 4096 {
			return errors.New("urag: 夹具正文必须短，并且带有标记")
		}
		if strings.Contains(doc.Body, probe) {
			foundProbe = true
		}
	}
	if !foundProbe {
		return errors.New("urag: 夹具查询必须出现在某一篇正文里")
	}
	return nil
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("urag: 请在仓库根目录执行")
		}
		dir = parent
	}
}

func openCatalog(ctx context.Context) (*catalog.MySQL, error) {
	dsn, err := mysqlDSN()
	if err != nil {
		return nil, err
	}
	var last error
	for attempt := 0; attempt < 30; attempt++ {
		db, err := catalog.NewMySQL(dsn)
		if err != nil {
			return nil, err
		}
		err = db.ApplySchema(ctx)
		if err == nil {
			return db, nil
		}
		_ = db.Close()
		last = err
		if !temporaryMySQL(err) || attempt == 29 {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, last
}

func mysqlDSN() (string, error) {
	if v := strings.TrimSpace(os.Getenv("URAG_MYSQL_DSN")); v != "" {
		return v, nil
	}
	pass := strings.TrimRight(os.Getenv("URAG_MYSQL_ROOT_PASSWORD"), "\r\n")
	if pass == "" {
		got, err := rootPasswordFromContainer()
		if err != nil || strings.TrimSpace(got) == "" {
			return "", errors.New("urag: 缺少 URAG_MYSQL_DSN，无法初始化台账")
		}
		pass = strings.TrimRight(got, "\r\n")
	}
	addr := strings.TrimSpace(os.Getenv("URAG_MYSQL_ADDR"))
	if addr == "" {
		addr = "127.0.0.1:3306"
	}
	name := strings.TrimSpace(os.Getenv("URAG_MYSQL_DATABASE"))
	if name == "" {
		name = "urag"
	}
	cfg := mysql.NewConfig()
	cfg.User = "root"
	cfg.Passwd = pass
	cfg.Net = "tcp"
	cfg.Addr = addr
	cfg.DBName = name
	return cfg.FormatDSN(), nil
}

func rootPasswordFromContainer() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "exec", "urag-mysql", "cat", "/run/secrets/mysql_root")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func temporaryMySQL(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, part := range []string{
		"connection refused",
		"i/o timeout",
		"unexpected EOF",
		"bad connection",
		"Error 2002",
		"Error 2003",
	} {
		if strings.Contains(msg, part) {
			return true
		}
	}
	return false
}

func waitElastic(ctx context.Context, addr string) error {
	for attempt := 0; attempt < 30; attempt++ {
		ok, err := index.ElasticReady(ctx, addr)
		if err == nil && ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("urag: Elasticsearch 未就绪")
}
