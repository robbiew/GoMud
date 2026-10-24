// Package bbsprotocol implements the bbsmud version 1 BBS relay handshake.
package bbsprotocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const Version = 1
const MaxFrame = 8192

// Handshake is sent only over a verified TLS connection. Token is a secret
// scoped to BBSID; servers must authenticate it before trusting other fields.
type Handshake struct {
	Version  int    `json:"version"`
	BBSID    string `json:"bbs_id"`
	Token    string `json:"token"`
	Alias    string `json:"alias"`
	NodeNum  int    `json:"node_num"`
	TimeLeft int    `json:"time_left"` // Whole BBS minutes remaining.
	Columns  int    `json:"columns"`
	Rows     int    `json:"rows"`
}

type Ack struct {
	Version int    `json:"version"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

var bbsID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func (h Handshake) Validate() error {
	if h.Version != Version {
		return errors.New("unsupported protocol version")
	}
	if !bbsID.MatchString(h.BBSID) {
		return errors.New("bbs_id must be 1-64 ASCII letters, digits, underscores or hyphens")
	}
	if len(h.Token) == 0 || len(h.Token) > 1024 {
		return errors.New("BBS token must contain 1-1024 bytes")
	}
	if !utf8.ValidString(h.Alias) || strings.TrimSpace(h.Alias) == "" || len(h.Alias) > 128 || strings.ContainsFunc(h.Alias, unicode.IsControl) {
		return errors.New("invalid player alias")
	}
	if h.NodeNum < 0 || h.NodeNum > 65535 {
		return errors.New("invalid node number")
	}
	if h.TimeLeft < 1 || h.TimeLeft > 10080 {
		return errors.New("time_left must be between 1 and 10080 minutes")
	}
	if h.Columns < 1 || h.Columns > 1000 || h.Rows < 1 || h.Rows > 1000 {
		return errors.New("invalid terminal dimensions")
	}
	return nil
}

// WriteFrame writes one bounded JSON object prefixed by a four-byte big-endian
// byte length. It handles partial writes; no buffered writer is left unflushed.
func WriteFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) == 0 || len(b) > MaxFrame {
		return errors.New("invalid frame length")
	}
	frame := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(frame, uint32(len(b)))
	copy(frame[4:], b)
	for len(frame) > 0 {
		n, err := w.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}

// ReadFrame consumes exactly one frame, leaving coalesced game output intact.
func ReadFrame(r io.Reader, v any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > MaxFrame {
		return errors.New("invalid frame length")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	if !utf8.Valid(b) || len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '{' {
		return errors.New("expected UTF-8 JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid handshake frame: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
