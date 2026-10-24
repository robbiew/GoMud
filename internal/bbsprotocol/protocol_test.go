package bbsprotocol

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

func valid() Handshake {
	return Handshake{Version: 1, BBSID: "bbs-1", Token: "secret", Alias: "Player", NodeNum: 2, TimeLeft: 25, Columns: 80, Rows: 25}
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return w.Buffer.Write(p)
}
func TestFramingPreservesFollowingANSIAndHandlesShortWrites(t *testing.T) {
	var w shortWriter
	h := valid()
	if err := WriteFrame(&w, h); err != nil {
		t.Fatal(err)
	}
	tail := "\x1b[31mWelcome\xff\x00"
	w.Buffer.WriteString(tail)
	var got Handshake
	if err := ReadFrame(&w, &got); err != nil {
		t.Fatal(err)
	}
	if got != h || w.String() != tail {
		t.Fatalf("frame consumed or altered stream")
	}
}
func frame(s string) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(len(s)))
	return append(b, []byte(s)...)
}
func TestRejectMalformedFrames(t *testing.T) {
	for _, b := range [][]byte{{0, 0, 0, 0}, {0, 0, 32, 1}, {0, 0, 0, 2, '{'}, frame("null"), frame("{} {}"), frame(`{"version":1,"surprise":true}`), frame("{\xff}")} {
		var ack Ack
		if err := ReadFrame(bytes.NewReader(b), &ack); err == nil {
			t.Errorf("accepted malformed frame %q", b)
		}
	}
}
func TestHandshakeValidation(t *testing.T) {
	for _, change := range []func(*Handshake){func(h *Handshake) { h.Version = 2 }, func(h *Handshake) { h.BBSID = "../other" }, func(h *Handshake) { h.Token = "" }, func(h *Handshake) { h.Alias = "bad\x1bname" }, func(h *Handshake) { h.Alias = "\xff" }, func(h *Handshake) { h.TimeLeft = 0 }, func(h *Handshake) { h.TimeLeft = -1 }, func(h *Handshake) { h.Columns = 0 }, func(h *Handshake) { h.NodeNum = -1 }} {
		h := valid()
		change(&h)
		if h.Validate() == nil {
			t.Errorf("accepted invalid handshake")
		}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(io.Discard, strings.Repeat("x", MaxFrame)); err == nil {
		t.Fatal("accepted oversized frame")
	}
}
