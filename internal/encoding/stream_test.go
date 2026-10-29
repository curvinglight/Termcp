package encoding

import (
	"strings"
	"testing"
)

// The streaming decoder exists for one failure: a burst boundary inside a
// multi-byte character. Split every boundary and the output must still be the
// same text a single-shot decode would have produced.

func TestStreamingDecoderSurvivesSplitCharacters(t *testing.T) {
	// "中文测试" in GBK: each hanzi is two bytes, so a chunk size of one byte
	// cuts every character in half. The bytes are written literally because a
	// Go string literal would be UTF-8 — the one encoding this decoder must
	// not translate.
	src := []byte{0xD6, 0xD0, 0xCE, 0xC4, 0xB2, 0xE2, 0xCA, 0xD4}
	want := "中文测试"
	d := NewStreamingDecoder("gbk")
	if d == nil {
		t.Fatal("gbk should map to a decoder")
	}
	var out []byte
	for _, b := range src {
		out = append(out, d.Decode([]byte{b})...)
	}
	out = append(out, d.Decode(nil)...) // flush the held tail
	if string(out) != want {
		t.Fatalf("byte-at-a-time decode = %q, want %q", out, want)
	}
}

func TestStreamingDecoderKeepsTailAcrossChunks(t *testing.T) {
	// "温度: 25\r\nOK" in GBK (温度 = CE C2 B6 C8).
	src := []byte{0xCE, 0xC2, 0xB6, 0xC8, ':', ' ', '2', '5', '\r', '\n', 'O', 'K'}
	d := NewStreamingDecoder("gbk")
	var out []byte
	off := 0
	for off < len(src) {
		n := 3
		if off+n > len(src) {
			n = len(src) - off
		}
		out = append(out, d.Decode(src[off:off+n])...)
		off += n
	}
	out = append(out, d.Decode(nil)...)
	if !strings.Contains(string(out), "温度: 25") {
		t.Fatalf("decoded stream lost the text: %q", out)
	}
}

func TestStreamingDecoderPassthroughForUTF8(t *testing.T) {
	if NewStreamingDecoder("utf-8") != nil {
		t.Error("utf-8 needs no server-side decoder")
	}
	if NewStreamingDecoder("") != nil || NewStreamingDecoder("nope") != nil {
		t.Error("unknown names must pass through, not guess")
	}
	src := []byte{0xD6, 0xD0}
	if got := NewStreamingDecoder("nope"); got != nil {
		_ = got
	}
	// A nil decoder returns the chunk unchanged.
	var d *StreamingDecoder
	if got := d.Decode(src); string(got) != string(src) {
		t.Fatalf("nil decoder changed the bytes: %x", got)
	}
}

func TestSniffIdentifiesGBKAndFallsBackToUTF8(t *testing.T) {
	gbk := []byte{0xD6, 0xD0, 0xCE, 0xC4} // "中文" in GBK
	if cs := Sniff(gbk); cs != "gbk" {
		t.Fatalf("Sniff(GBK 中文) = %q, want gbk", cs)
	}
	utf8zh := []byte("中文")
	if cs := Sniff(utf8zh); cs != "" {
		t.Fatalf("Sniff(UTF-8 中文) = %q, want \"\" (stay UTF-8)", cs)
	}
	// A ROUND TRIP THROUGH A LEGACY TABLE IS NOT EVIDENCE. This is a kernel log
	// line in UTF-8; every byte of it also decodes and re-encodes as shift_jis, so
	// the legacy loop used to name shift_jis for text this build reads as-is — and
	// the session's charset latched to it, garbling everything after.
	for _, utf8Text := range []string{
		"[    0.3] 系统启动中\r\n",
		"设备版本 v1.2\r\n",
		"重启完成。\r\n",
	} {
		if cs := Sniff([]byte(utf8Text)); cs != "" {
			t.Fatalf("Sniff(%q) = %q, want \"\" (already UTF-8)", utf8Text, cs)
		}
	}
	// Plain ASCII says nothing about a charset. "gbk" was a latch on the boot
	// banner: an ASCII-only first frame decided the session's encoding before any
	// Chinese had arrived.
	if cs := Sniff([]byte("[    0.000000] Booting Linux on physical CPU 0x0\r\n")); cs != "" {
		t.Fatalf("Sniff(ASCII) = %q, want \"\" (no evidence)", cs)
	}
	// A frame that ends inside a character is still a UTF-8 frame.
	split := []byte{'[', 0xE7, 0xB3} // 系 cut after two of its three bytes
	if cs := Sniff(split); cs != "" {
		t.Fatalf("Sniff(split UTF-8 tail) = %q, want \"\"", cs)
	}
	// A BOM is proof, not a guess.
	bom := append([]byte{0xFF, 0xFE}, []byte("a\x00b\x00")...)
	if cs := Sniff(bom); cs != "utf-16le" {
		t.Fatalf("Sniff(UTF-16LE BOM) = %q, want utf-16le", cs)
	}
}

func TestCharsetValidAcceptsAutoAndTableNames(t *testing.T) {
	for _, ok := range []string{"", "auto", "utf-8", "gbk", "cp936", "big5"} {
		if !CharsetValid(ok) {
			t.Errorf("CharsetValid(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"gbkrate", "u t f"} {
		if CharsetValid(bad) {
			t.Errorf("CharsetValid(%q) = true, want false", bad)
		}
	}
}
