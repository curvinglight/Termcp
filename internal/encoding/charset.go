package encoding

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// The charsets a session's output can be in.
//
// A TERMINAL IS NOT A UTF-8 DEVICE. On a Chinese Windows the console code page is
// 936, so every byte a shell writes there is GBK: a path, a compiler diagnostic, a
// `dir` listing. The daemon used to assume UTF-8 everywhere, which turned that into
// three separate failures — a read that refused the whole window, a `--grep` that
// scanned bytes against a UTF-8 pattern and found nothing, and a JSON-RPC frame from
// an extension that could not be read at all.
//
// The names are the ones a caller can look up: IANA names, plus the code-page
// numbers Windows users say out loud (cp936 is GBK, cp65001 is UTF-8).
var charsets = map[string]encoding.Encoding{
	"":        nil, // UTF-8: no decoder needed
	"utf-8":   nil,
	"utf8":    nil,
	"cp65001": nil,

	"gbk":          simplifiedchinese.GBK,
	"gb2312":       simplifiedchinese.HZGB2312, // the encoding name callers use for it
	"gb18030":      simplifiedchinese.GB18030,
	"cp936":        simplifiedchinese.GBK,
	"big5":         traditionalchinese.Big5,
	"cp950":        traditionalchinese.Big5,
	"shift_jis":    japanese.ShiftJIS,
	"shift-jis":    japanese.ShiftJIS,
	"sjis":         japanese.ShiftJIS,
	"cp932":        japanese.ShiftJIS,
	"euc-jp":       japanese.EUCJP,
	"euc-kr":       korean.EUCKR,
	"cp949":        korean.EUCKR,
	"windows-1252": charmap.Windows1252,
	"cp1252":       charmap.Windows1252,
	"latin-1":      charmap.ISO8859_1,
	"iso-8859-1":   charmap.ISO8859_1,
}

// KnownCharset reports whether name is a charset this build can decode, and what it
// normalises to. The second answer is what goes in a reply, so a caller that guessed
// `CP936` is told the spelling the rest of the replies use.
func KnownCharset(name string) (string, bool) {
	key := normaliseCharset(name)
	if _, ok := charsets[key]; !ok {
		return "", false
	}
	if key == "" || key == "utf8" || key == "cp65001" {
		return "utf-8", true
	}
	return key, true
}

// Decode turns bytes in the named charset into UTF-8.
//
// An unknown name is an error rather than a silent UTF-8 read: the caller asked for
// an interpretation, and answering with a different one is how mojibake gets
// attributed to the wrong layer. The empty name means UTF-8, which needs no work.
func Decode(name string, data []byte) ([]byte, error) {
	enc, ok := charsets[normaliseCharset(name)]
	if !ok {
		return nil, &UnknownCharsetError{Name: name}
	}
	if enc == nil {
		return data, nil
	}
	// x/text's decoder maps bytes it cannot represent to U+FFFD, which is exactly
	// the substitution this package's Render refuses. A caller that asked for GBK and
	// handed us something else is told, rather than handed replacement characters.
	out, err := enc.NewDecoder().Bytes(data)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(out) {
		return nil, &UnknownCharsetError{Name: name, Detail: "the bytes are not valid " + name}
	}
	return out, nil
}

// Sniff names the charset these bytes are, when it can be established rather than
// guessed.
//
// The test is a ROUND TRIP: decode, encode, and require the original bytes back. That
// is what separates "these bytes are GBK" from "these bytes happen to decode as
// GBK" — a claim the daemon is willing to put in an error message only because the
// bytes confirm it. Order matters and is stated: GBK before Big5, because a zh-CN
// Windows console is the case being diagnosed and GBK's byte ranges are a superset of
// Big5's common ones.
//
// It returns "" when nothing round-trips. That is a real answer: the bytes are not
// text in a charset this build knows, and the caller is told nothing rather than
// told a guess.
func Sniff(data []byte) string {
	for _, name := range []string{"gbk", "gb18030", "big5", "shift_jis", "euc-kr"} {
		enc := charsets[name]
		if enc == nil {
			continue
		}
		out, err := enc.NewDecoder().Bytes(data)
		if err != nil || !utf8.Valid(out) {
			continue
		}
		back, err := enc.NewEncoder().Bytes(out)
		if err != nil || !bytes.Equal(back, data) {
			continue
		}
		return name
	}
	return ""
}

// UnknownCharsetError names a charset this build cannot decode.
type UnknownCharsetError struct {
	Name   string
	Detail string
}

func (e *UnknownCharsetError) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	return "unknown charset " + e.Name + "; use utf-8, gbk (cp936), gb18030, big5, shift_jis or euc-kr"
}

func normaliseCharset(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
