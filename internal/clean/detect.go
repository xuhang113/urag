package clean

import (
	"bytes"
	"encoding/binary"
	"path"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

func detectMedia(filename string, raw []byte) (string, error) {
	switch strings.ToLower(path.Ext(filename)) {
	case ".txt":
		return "txt", nil
	case ".md":
		return "md", nil
	case ".html", ".htm":
		return "html", nil
	case ".pdf":
		return "pdf", nil
	}
	trimmed := bytes.TrimSpace(raw)
	if bytes.HasPrefix(trimmed, []byte("%PDF")) {
		return "pdf", nil
	}
	if looksLikeHTML(trimmed) {
		return "html", nil
	}
	return "", ErrUnsupportedType
}

func looksLikeHTML(raw []byte) bool {
	sample := raw
	if len(sample) > 1024 {
		sample = sample[:1024]
	}
	s := strings.ToLower(string(sample))
	if strings.HasPrefix(s, "<!doctype html") || strings.HasPrefix(s, "<html") {
		return true
	}
	return strings.Contains(s, "<html") || strings.Contains(s, "<head") || strings.Contains(s, "<body")
}

func decodeText(raw []byte) (string, string, error) {
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		return string(raw[3:]), "utf-8", nil
	}
	if len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE {
		return decodeUTF16(raw[2:], true), "utf-16le", nil
	}
	if len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF {
		return decodeUTF16(raw[2:], false), "utf-16be", nil
	}
	if utf8.Valid(raw) {
		return string(raw), "utf-8", nil
	}
	if s, ok := tryDecode(raw, simplifiedchinese.GBK); ok {
		return s, "gbk", nil
	}
	if s, ok := tryDecode(raw, simplifiedchinese.GB18030); ok {
		return s, "gb18030", nil
	}
	out, _, err := transform.Bytes(simplifiedchinese.GB18030.NewDecoder(), raw)
	if err != nil {
		return "", "", err
	}
	return string(out), "gb18030", nil
}

func tryDecode(raw []byte, enc encoding.Encoding) (string, bool) {
	out, _, err := transform.Bytes(enc.NewDecoder(), raw)
	if err != nil {
		return "", false
	}
	if strings.ContainsRune(string(out), utf8.RuneError) {
		return "", false
	}
	return string(out), true
}

func decodeUTF16(b []byte, little bool) string {
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		if little {
			units[i] = binary.LittleEndian.Uint16(b[i*2:])
		} else {
			units[i] = binary.BigEndian.Uint16(b[i*2:])
		}
	}
	return string(utf16.Decode(units))
}
