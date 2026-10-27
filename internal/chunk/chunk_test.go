package chunk_test

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/model"
)

func TestParagraphPackAndOverlap(t *testing.T) {
	sp := chunk.NewParagraph()
	if sp.Name() != "paragraph" {
		t.Fatalf("name %s", sp.Name())
	}

	t.Run("pack with overlap", func(t *testing.T) {
		doc := model.Document{ID: "doc-1", CollectionID: "col", Text: "aaaaaa\n\nbbbbbb"}
		got, err := sp.Split(doc, chunk.Options{MaxLen: 10, Overlap: 2})
		if err != nil {
			t.Fatal(err)
		}
		want := []model.Chunk{
			{ID: "doc-1#0", DocumentID: "doc-1", CollectionID: "col", Ordinal: 0, Text: "aaaaaa", StartByte: 0, EndByte: 6},
			{ID: "doc-1#1", DocumentID: "doc-1", CollectionID: "col", Ordinal: 1, Text: doc.Text[4:14], StartByte: 4, EndByte: 14},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v", got)
		}
		if got[0].EndByte-got[1].StartByte != 2 {
			t.Fatalf("overlap %d", got[0].EndByte-got[1].StartByte)
		}
	})

	t.Run("overlap that does not fit stays within maxlen", func(t *testing.T) {
		doc := model.Document{ID: "doc-1", CollectionID: "col", Text: "aaaaaa\n\nbbbbbb"}
		got, err := sp.Split(doc, chunk.Options{MaxLen: 10, Overlap: 4})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[1].Text != "bbbbbb" || got[1].EndByte-got[1].StartByte > 10 {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("merge short paragraphs", func(t *testing.T) {
		doc := model.Document{ID: "d", CollectionID: "c", Text: "aa\n\nbb"}
		got, err := sp.Split(doc, chunk.Options{MaxLen: 10, Overlap: -1})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Text != "aa\n\nbb" {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("blank line separates", func(t *testing.T) {
		doc := model.Document{ID: "d", CollectionID: "c", Text: "aaa\n\nbbb"}
		got, err := sp.Split(doc, chunk.Options{MaxLen: 3, Overlap: -1})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Text != "aaa" || got[1].Text != "bbb" {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("single newline stays one paragraph", func(t *testing.T) {
		doc := model.Document{ID: "d", CollectionID: "c", Text: "ab\ncd"}
		got, err := sp.Split(doc, chunk.Options{MaxLen: 10, Overlap: -1})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Text != "ab\ncd" {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("crlf blank line", func(t *testing.T) {
		doc := model.Document{ID: "d", CollectionID: "c", Text: "aa\r\n\r\nbb"}
		got, err := sp.Split(doc, chunk.Options{MaxLen: 2, Overlap: -1})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Text != "aa" || got[1].Text != "bb" {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		text := strings.Repeat("a", 900)
		doc := model.Document{ID: "d", CollectionID: "c", Text: text}
		got, err := sp.Split(doc, chunk.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Text != text[:800] || got[1].StartByte != 700 || got[1].EndByte != 900 {
			t.Fatalf("starts %d %d lens %d", got[0].StartByte, got[1].StartByte, len(got))
		}
		if got[0].ID != "d#0" || got[1].ID != "d#1" || got[1].CollectionID != "c" || got[1].DocumentID != "d" {
			t.Fatalf("ids %#v", got)
		}
	})

	t.Run("negative overlap", func(t *testing.T) {
		text := strings.Repeat("b", 250)
		doc := model.Document{ID: "d", CollectionID: "c", Text: text}
		got, err := sp.Split(doc, chunk.Options{MaxLen: 100, Overlap: -5})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 || got[0].EndByte != 100 || got[1].StartByte != 100 || got[2].StartByte != 200 || got[2].EndByte != 250 {
			t.Fatalf("%#v", bounds(got))
		}
	})

	t.Run("cap overlap at half", func(t *testing.T) {
		text := strings.Repeat("c", 250)
		doc := model.Document{ID: "d", CollectionID: "c", Text: text}
		got, err := sp.Split(doc, chunk.Options{MaxLen: 100, Overlap: 90})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 4 {
			t.Fatalf("len %d %#v", len(got), bounds(got))
		}
		wantStart := []int{0, 50, 100, 150}
		for i, start := range wantStart {
			if got[i].StartByte != start || got[i].EndByte-got[i].StartByte > 100 {
				t.Fatalf("chunk %d %#v", i, got[i])
			}
			if doc.Text[got[i].StartByte:got[i].EndByte] != got[i].Text {
				t.Fatal("slice")
			}
		}
		if got[3].EndByte != 250 {
			t.Fatalf("end %d", got[3].EndByte)
		}
	})

	t.Run("utf-8 boundaries", func(t *testing.T) {
		text := strings.Repeat("灯", 10)
		doc := model.Document{ID: "d", CollectionID: "c", Text: text}
		got, err := sp.Split(doc, chunk.Options{MaxLen: 10, Overlap: -1})
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, ch := range got {
			if len(ch.Text) > 10 || !utf8.ValidString(ch.Text) {
				t.Fatalf("chunk %q", ch.Text)
			}
			if doc.Text[ch.StartByte:ch.EndByte] != ch.Text {
				t.Fatal("slice")
			}
			b.WriteString(ch.Text)
		}
		if b.String() != text {
			t.Fatalf("coverage %q", b.String())
		}
	})

	t.Run("empty", func(t *testing.T) {
		for _, text := range []string{"", "\n\n", " \n\t"} {
			got, err := sp.Split(model.Document{ID: "d", Text: text}, chunk.Options{})
			if err != nil || len(got) != 0 {
				t.Fatalf("text %q got %#v err %v", text, got, err)
			}
		}
	})
}

func TestDropKeywordsWholeWord(t *testing.T) {
	terms := []string{"Lantern", "Crimson"}
	sp := chunk.NewDropKeywords(terms)
	if sp.Name() != "drop_keywords" {
		t.Fatalf("name %s", sp.Name())
	}
	terms[0] = "zzzz"
	doc := model.Document{
		ID:           "doc",
		CollectionID: "alpha",
		Text:         "Crimson lanterns and a Lantern.",
	}
	got, err := sp.Split(doc, chunk.Options{MaxLen: 800, Overlap: 100})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinChunks(got)
	if containsWord(joined, "lantern") || containsWord(joined, "crimson") {
		t.Fatalf("dropped words remain in %q", joined)
	}
	if !strings.Contains(strings.ToLower(joined), "lanterns") || !strings.Contains(joined, "and") {
		t.Fatalf("kept text %q", joined)
	}
	assertChunkBounds(t, doc, got, 20)

	long := model.Document{ID: "doc", CollectionID: "alpha", Text: strings.Repeat("word ", 10)}
	pieces, err := chunk.NewDropKeywords([]string{"missing"}).Split(long, chunk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if joinChunks(pieces) != long.Text {
		t.Fatalf("shred %q", joinChunks(pieces))
	}
	assertChunkBounds(t, long, pieces, 20)
	if pieces[0].ID != "doc#0" || pieces[0].Ordinal != 0 || pieces[0].DocumentID != "doc" || pieces[0].CollectionID != "alpha" {
		t.Fatalf("first %#v", pieces[0])
	}

	empty, err := chunk.NewDropKeywords(nil).Split(model.Document{ID: "d"}, chunk.Options{})
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty %#v err %v", empty, err)
	}
}

func assertChunkBounds(t *testing.T, doc model.Document, chunks []model.Chunk, maxLen int) {
	t.Helper()
	for i, ch := range chunks {
		if ch.StartByte < 0 || ch.EndByte > len(doc.Text) || ch.StartByte > ch.EndByte {
			t.Fatalf("range %+v", ch)
		}
		if doc.Text[ch.StartByte:ch.EndByte] != ch.Text {
			t.Fatalf("slice %+v", ch)
		}
		if len(ch.Text) > maxLen || !utf8.ValidString(ch.Text) {
			t.Fatalf("len %d text %q", len(ch.Text), ch.Text)
		}
		if ch.Ordinal != i {
			t.Fatalf("ordinal %d", ch.Ordinal)
		}
	}
}

func joinChunks(chunks []model.Chunk) string {
	var b strings.Builder
	for _, ch := range chunks {
		b.WriteString(ch.Text)
	}
	return b.String()
}

func containsWord(text, word string) bool {
	text = strings.ToLower(text)
	word = strings.ToLower(word)
	for i := 0; i+len(word) <= len(text); i++ {
		if text[i:i+len(word)] != word {
			continue
		}
		before := i == 0 || !isASCIIWord(text[i-1])
		after := i+len(word) == len(text) || !isASCIIWord(text[i+len(word)])
		if before && after {
			return true
		}
	}
	return false
}

func isASCIIWord(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func bounds(chunks []model.Chunk) [][2]int {
	out := make([][2]int, len(chunks))
	for i, ch := range chunks {
		out[i] = [2]int{ch.StartByte, ch.EndByte}
	}
	return out
}
