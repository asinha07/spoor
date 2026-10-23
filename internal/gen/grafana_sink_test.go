package gen

import (
	"bytes"
	"testing"
)

func TestSnappyLiteralHeader(t *testing.T) {
	src := bytes.Repeat([]byte("a"), 70000)
	out := snappyLiteral(src)
	// uvarint(70000) = 0xf0 0xa2 0x04, then a 65536-byte literal (tag 61<<2).
	if !bytes.HasPrefix(out, []byte{0xf0, 0xa2, 0x04, 61 << 2, 0xff, 0xff}) {
		t.Errorf("header = % x", out[:6])
	}
	if len(out) != 3+3+65536+3+(70000-65536) {
		t.Errorf("length = %d", len(out))
	}
}

func TestParseSeries(t *testing.T) {
	l, rest, err := parseSeries(`http_server_request_duration_seconds_bucket{service_name="pay",le="0.1"} 12 1700000000000`)
	if err != nil || l["__name__"] != "http_server_request_duration_seconds_bucket" || l["le"] != "0.1" || l["service_name"] != "pay" || rest != " 12 1700000000000" {
		t.Fatalf("labels %v rest %q err %v", l, rest, err)
	}
}
