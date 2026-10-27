package clean

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/transform"
)

// extractPDF 用纯 Go 读取经典 xref 和 FlateDecode 内容流，抽出 Tj/TJ 里的可见文字。
// 最新的 github.com/ledongthuc/pdf 要求 Go 1.24，本模块保持 Go 1.22，因此不依赖它。
func extractPDF(raw []byte) (string, string, error) {
	head := raw
	if len(head) > 1024 {
		head = head[:1024]
	}
	if !bytes.Contains(head, []byte("%PDF")) {
		return "", "", errors.New("clean: pdf: not a pdf")
	}
	offsets, err := parseXRef(raw)
	if err != nil {
		return "", "", err
	}
	nums := make([]int, 0, len(offsets))
	for n := range offsets {
		nums = append(nums, n)
	}
	sort.Ints(nums)

	var b strings.Builder
	enc := "winansi"
	for _, n := range nums {
		dict, stream, ok := readStream(raw, offsets[n])
		if !ok || len(stream) == 0 || skipStream(dict) {
			continue
		}
		decoded, decErr := decodeStream(dict, stream)
		if decErr != nil {
			continue
		}
		piece, pieceEnc := extractContent(decoded)
		piece = strings.TrimSpace(piece)
		if piece == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(piece)
		if strings.Contains(pieceEnc, "utf-16") {
			enc = pieceEnc
		}
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return "", "", errors.New("clean: pdf: no text")
	}
	return text, enc, nil
}

func parseXRef(data []byte) (map[int]int, error) {
	idx := bytes.LastIndex(data, []byte("startxref"))
	if idx < 0 {
		return nil, errors.New("clean: pdf: no startxref")
	}
	rest := bytes.TrimLeft(data[idx+len("startxref"):], " \r\n")
	var pos int
	if _, err := fmt.Sscan(string(rest), &pos); err != nil || pos < 0 || pos >= len(data) {
		return nil, errors.New("clean: pdf: bad startxref")
	}
	if !bytes.HasPrefix(data[pos:], []byte("xref")) {
		return nil, errors.New("clean: pdf: xref missing")
	}
	lines := bytes.Split(data[pos:], []byte("\n"))
	offsets := map[int]int{}
	expect := 0
	obj := 0
	for _, line := range lines[1:] {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte("trailer")) {
			break
		}
		if expect == 0 {
			var start, count int
			if _, err := fmt.Sscanf(string(line), "%d %d", &start, &count); err != nil {
				return nil, errors.New("clean: pdf: bad xref subsection")
			}
			obj = start
			expect = count
			continue
		}
		var off, gen int
		var flag byte
		if _, err := fmt.Sscanf(string(line), "%d %d %c", &off, &gen, &flag); err != nil {
			return nil, errors.New("clean: pdf: bad xref entry")
		}
		if flag == 'n' {
			offsets[obj] = off
		}
		obj++
		expect--
	}
	if len(offsets) == 0 {
		return nil, errors.New("clean: pdf: empty xref")
	}
	return offsets, nil
}

func readStream(data []byte, off int) (dict string, raw []byte, ok bool) {
	if off < 0 || off >= len(data) {
		return "", nil, false
	}
	chunk := data[off:]
	rel := bytes.Index(chunk, []byte("obj"))
	if rel < 0 || rel > 64 {
		return "", nil, false
	}
	body := bytes.TrimLeft(chunk[rel+3:], " \r\n")
	if !bytes.HasPrefix(body, []byte("<<")) {
		return "", nil, false
	}
	dictEnd := bytes.Index(body, []byte(">>"))
	if dictEnd < 0 {
		return "", nil, false
	}
	dict = string(body[:dictEnd+2])
	rest := bytes.TrimLeft(body[dictEnd+2:], " \r\n")
	if !bytes.HasPrefix(rest, []byte("stream")) {
		return dict, nil, false
	}
	rest = rest[len("stream"):]
	if len(rest) > 0 && rest[0] == '\r' {
		rest = rest[1:]
	}
	if len(rest) > 0 && rest[0] == '\n' {
		rest = rest[1:]
	}
	length, hasLen := dictLength(dict)
	if !hasLen {
		end := bytes.Index(rest, []byte("endstream"))
		if end < 0 {
			return dict, nil, false
		}
		return dict, bytes.TrimRight(rest[:end], "\r\n"), true
	}
	if length < 0 || length > len(rest) {
		return dict, nil, false
	}
	return dict, rest[:length], true
}

func dictLength(dict string) (int, bool) {
	i := strings.Index(dict, "/Length")
	if i < 0 {
		return 0, false
	}
	rest := strings.TrimSpace(dict[i+len("/Length"):])
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:j])
	if err != nil {
		return 0, false
	}
	after := strings.TrimSpace(rest[j:])
	if strings.HasPrefix(after, "0 R") {
		return 0, false
	}
	return n, true
}

func skipStream(dict string) bool {
	return strings.Contains(dict, "/Image") || strings.Contains(dict, "/FontFile")
}

func decodeStream(dict string, raw []byte) ([]byte, error) {
	if strings.Contains(dict, "FlateDecode") {
		r, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(r)
	}
	if strings.Contains(dict, "/Filter") {
		return nil, errors.New("clean: pdf: unsupported filter")
	}
	return raw, nil
}

type pdfOpKind int

const (
	pdfStr pdfOpKind = iota
	pdfNum
)

type pdfOp struct {
	kind pdfOpKind
	str  string
	enc  string
	num  float64
}

func extractContent(data []byte) (string, string) {
	enc := "winansi"
	var b strings.Builder
	var ops []pdfOp
	flush := func(newline bool) {
		if newline && b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\n')
		}
		for _, op := range ops {
			if op.kind == pdfStr {
				b.WriteString(op.str)
				if strings.Contains(op.enc, "utf-16") {
					enc = op.enc
				}
				continue
			}
			if op.num <= -200 {
				b.WriteByte(' ')
			}
		}
		ops = nil
	}
	i := 0
	for i < len(data) {
		c := data[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == 0:
			i++
		case c == '%':
			for i < len(data) && data[i] != '\n' {
				i++
			}
		case c == '(':
			s, kind, next := parseLiteral(data, i)
			ops = append(ops, pdfOp{kind: pdfStr, str: s, enc: kind})
			i = next
		case c == '<' && i+1 < len(data) && data[i+1] != '<':
			s, kind, next := parseHexString(data, i)
			ops = append(ops, pdfOp{kind: pdfStr, str: s, enc: kind})
			i = next
		case c == '[' || c == ']':
			i++
		case c == '/':
			i++
			for i < len(data) && !isPDFDelim(data[i]) {
				i++
			}
		default:
			if isNumStart(data, i) {
				j := i + 1
				for j < len(data) && isNumChar(data[j]) {
					j++
				}
				n, err := strconv.ParseFloat(string(data[i:j]), 64)
				if err == nil {
					ops = append(ops, pdfOp{kind: pdfNum, num: n})
				}
				i = j
				continue
			}
			j := i + 1
			if c == '\'' || c == '"' {
				j = i + 1
			} else {
				for j < len(data) && !isPDFDelim(data[j]) {
					j++
				}
			}
			if j <= i {
				i++
				continue
			}
			switch string(data[i:j]) {
			case "Tj", "TJ":
				flush(false)
			case "'", "\"":
				flush(true)
			case "T*":
				if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
					b.WriteByte('\n')
				}
				ops = nil
			default:
				ops = nil
			}
			i = j
		}
	}
	return b.String(), enc
}

func parseLiteral(data []byte, i int) (string, string, int) {
	i++
	var raw []byte
	depth := 1
	for i < len(data) && depth > 0 {
		c := data[i]
		if c == '\\' {
			i++
			if i >= len(data) {
				break
			}
			switch data[i] {
			case 'n':
				raw = append(raw, '\n')
			case 'r':
				raw = append(raw, '\r')
			case 't':
				raw = append(raw, '\t')
			case 'b':
				raw = append(raw, '\b')
			case 'f':
				raw = append(raw, '\f')
			case '(', ')', '\\':
				raw = append(raw, data[i])
			case '\n':
			case '\r':
				if i+1 < len(data) && data[i+1] == '\n' {
					i++
				}
			default:
				if data[i] >= '0' && data[i] <= '7' {
					oct := int(data[i] - '0')
					count := 1
					for count < 3 && i+1 < len(data) && data[i+1] >= '0' && data[i+1] <= '7' {
						i++
						oct = oct*8 + int(data[i]-'0')
						count++
					}
					raw = append(raw, byte(oct))
				} else {
					raw = append(raw, data[i])
				}
			}
			i++
			continue
		}
		if c == '(' {
			depth++
			raw = append(raw, c)
			i++
			continue
		}
		if c == ')' {
			depth--
			if depth == 0 {
				i++
				break
			}
			raw = append(raw, c)
			i++
			continue
		}
		raw = append(raw, c)
		i++
	}
	text, enc := decodePDFString(raw)
	return text, enc, i
}

func parseHexString(data []byte, i int) (string, string, int) {
	i++
	var digits []byte
	for i < len(data) && data[i] != '>' {
		if isHex(data[i]) {
			digits = append(digits, data[i])
		}
		i++
	}
	if i < len(data) && data[i] == '>' {
		i++
	}
	if len(digits)%2 == 1 {
		digits = append(digits, '0')
	}
	raw := make([]byte, len(digits)/2)
	for k := 0; k < len(raw); k++ {
		hi := hexVal(digits[k*2])
		lo := hexVal(digits[k*2+1])
		raw[k] = hi<<4 | lo
	}
	text, enc := decodePDFString(raw)
	return text, enc, i
}

func decodePDFString(raw []byte) (string, string) {
	if len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF {
		return decodePDFUTF16(raw[2:], false), "utf-16be"
	}
	if len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE {
		return decodePDFUTF16(raw[2:], true), "utf-16le"
	}
	out, _, err := transform.Bytes(charmap.Windows1252.NewDecoder(), raw)
	if err != nil {
		return string(raw), "winansi"
	}
	return string(out), "winansi"
}

func decodePDFUTF16(b []byte, little bool) string {
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		if little {
			units[i] = uint16(b[i*2]) | uint16(b[i*2+1])<<8
		} else {
			units[i] = uint16(b[i*2])<<8 | uint16(b[i*2+1])
		}
	}
	return string(utf16.Decode(units))
}

func isPDFDelim(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '\f', 0, '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	default:
		return false
	}
}

func isNumStart(data []byte, i int) bool {
	c := data[i]
	if c >= '0' && c <= '9' {
		return true
	}
	if c == '.' && i+1 < len(data) && data[i+1] >= '0' && data[i+1] <= '9' {
		return true
	}
	if (c == '+' || c == '-') && i+1 < len(data) {
		n := data[i+1]
		return (n >= '0' && n <= '9') || n == '.'
	}
	return false
}

func isNumChar(c byte) bool {
	return (c >= '0' && c <= '9') || c == '.' || c == '+' || c == '-'
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}
