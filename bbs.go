package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/GoMudEngine/GoMud/internal/bbs"
	"github.com/GoMudEngine/GoMud/internal/bbsprotocol"
	"github.com/GoMudEngine/GoMud/internal/configs"
	"github.com/GoMudEngine/GoMud/internal/connections"
	"github.com/GoMudEngine/GoMud/internal/mudlog"
)

// bbsStream suppresses output until the framed acknowledgment has been sent.
// Writes are serialized and bounded so stalled callers cannot block the world.
// Only this reader owns the buffered terminal stream.
type bbsStream struct {
	net.Conn
	reader  *bufio.Reader
	active  atomic.Bool
	writeMu sync.Mutex
	lastCR  bool
}

func newBBSStream(c net.Conn) *bbsStream { return &bbsStream{Conn: c, reader: bufio.NewReader(c)} }
func (s *bbsStream) Write(p []byte) (int, error) {
	if !s.active.Load() {
		return len(p), nil
	}
	// A BBS relay is not a Telnet endpoint. Plugins may offer IAC features;
	// drop these protocol-only writes instead of leaking them to the terminal.
	if len(p) > 0 && p[0] == 255 {
		return len(p), nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return 0, err
	}
	n, err := s.Conn.Write(p)
	if err != nil {
		_ = s.Conn.Close()
	}
	return n, err
}

// Read yields one logical key for the existing engine handlers. This makes
// command boundaries and UTF-8/ANSI sequences independent of TCP chunking.
func (s *bbsStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		b, err := s.reader.ReadByte()
		if err != nil {
			return 0, err
		}
		if s.lastCR && (b == '\n' || b == 0) {
			s.lastCR = false
			continue
		}
		s.lastCR = b == '\r'
		key := []byte{b}
		if b == 27 {
			_ = s.Conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			next, err := s.reader.ReadByte()
			if err != nil {
				_ = s.Conn.SetReadDeadline(time.Time{})
				if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
					return 0, err
				}
			} else if next == '[' || next == 'O' {
				_ = s.Conn.SetReadDeadline(time.Time{})
				key = append(key, next)
				for len(key) < 64 {
					c, e := s.reader.ReadByte()
					if e != nil {
						_ = s.Conn.SetReadDeadline(time.Time{})
						return 0, e
					}
					key = append(key, c)
					if c >= 0x40 && c <= 0x7e {
						break
					}
				}
				_ = s.Conn.SetReadDeadline(time.Time{})
			} else {
				_ = s.reader.UnreadByte()
				_ = s.Conn.SetReadDeadline(time.Time{})
			}
		} else if b >= utf8.RuneSelf {
			for !utf8.FullRune(key) && len(key) < utf8.UTFMax {
				next, e := s.reader.ReadByte()
				if e != nil {
					return 0, e
				}
				key = append(key, next)
			}
		}
		if len(p) < len(key) {
			return 0, io.ErrShortBuffer
		}
		return copy(p, key), nil
	}
}

func handleBBSConnection(ctx context.Context, c net.Conn, h bbsprotocol.Handshake, expiry, handshakeDeadline time.Time) {
	stream := newBBSStream(c)
	cd := connections.Add(stream, nil, connections.ConnBBS)
	id := cd.ConnectionId()
	defer connections.Remove(id)
	defer func() {
		if err := worldManager.runBBS(context.Background(), func() error { worldManager.disconnectBBS(id); return nil }); err != nil {
			mudlog.Error("BBS cleanup", "connectionId", id, "error", err)
		}
	}()
	attachCtx, cancelAttach := context.WithDeadline(ctx, handshakeDeadline)
	err := worldManager.runBBS(attachCtx, func() error { return worldManager.attachBBS(cd, h, expiry) })
	cancelAttach()
	if err != nil {
		mudlog.Warn("BBS attachment rejected", "connectionId", id, "error", err)
		bbs.Reject(c)
		return
	}
	if err := bbsprotocol.WriteFrame(c, bbsprotocol.Ack{Version: 1, OK: true}); err != nil {
		return
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		return
	}
	stream.active.Store(true)
	if err := worldManager.runBBS(ctx, func() error { return worldManager.enterBBS(id) }); err != nil {
		return
	}

	sessionCtx, cancel := context.WithCancel(ctx)
	timerDone := make(chan struct{})
	go func() {
		defer close(timerDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case <-ticker.C:
				if !time.Now().Before(expiry) {
					_ = worldManager.runBBS(sessionCtx, func() error { worldManager.disconnectBBS(id); return nil })
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-timerDone }()
	buffer := make([]byte, connections.ReadBufferSize)
	for {
		n, err := stream.Read(buffer)
		if err != nil {
			return
		}
		key := append([]byte(nil), buffer[:n]...)
		if err := worldManager.runBBS(ctx, func() error { return worldManager.inputBBS(id, key) }); err != nil {
			return
		}
	}
}

var errBBSUnavailable = errors.New("BBS session unavailable")

func startBBSListener(cfg configs.BBSNetwork) error {
	if cfg.Port == 0 {
		return nil
	}
	listener, err := bbs.New(cfg, handleBBSConnection)
	if err != nil {
		return err
	}
	if err := listener.Listen(); err != nil {
		listener.Close()
		return err
	}
	bbsListener.Store(listener)
	return nil
}
