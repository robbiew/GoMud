// Package bbs implements the dedicated authenticated TLS transport for bbsmud.
package bbs

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/GoMudEngine/GoMud/internal/bbsprotocol"
	"github.com/GoMudEngine/GoMud/internal/configs"
)

// Handler must acknowledge only after engine attachment succeeds. The socket
// retains its handshake deadline until Handler explicitly clears it.
type Handler func(context.Context, net.Conn, bbsprotocol.Handshake, time.Time, time.Time)
type attempt struct {
	start time.Time
	count int
}
type Server struct {
	cfg      configs.BBSNetwork
	tls      *tls.Config
	tokens   map[string][][32]byte
	handler  Handler
	ctx      context.Context
	cancel   context.CancelFunc
	listener net.Listener
	slots    chan struct{}
	mu       sync.Mutex
	perBBS   map[string]int
	attempts map[string]attempt
	wg       sync.WaitGroup
}

func New(cfg configs.BBSNetwork, handler Handler) (*Server, error) {
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	if cfg.Port == 0 || handler == nil {
		return nil, errors.New("BBS listener is disabled or has no handler")
	}
	cert, err := tls.LoadX509KeyPair(string(cfg.CertificateFile), string(cfg.KeyFile))
	if err != nil {
		return nil, fmt.Errorf("load BBS certificate: %w", err)
	}
	tokens, err := loadCredentials(string(cfg.CredentialsFile))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{cfg: cfg, tls: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}, tokens: tokens, handler: handler, ctx: ctx, cancel: cancel, slots: make(chan struct{}, int(cfg.MaxConnections)), perBBS: map[string]int{}, attempts: map[string]attempt{}}, nil
}

// loadCredentials supports overlapping tokens for rotation. Restart the server
// after editing the file. Hashes are kept in memory instead of plaintext tokens.
func loadCredentials(path string) (map[string][][32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open BBS credentials: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 {
		return nil, errors.New("BBS credentials must be a regular owner-only file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("BBS credentials file too large")
	}
	var plain map[string][]string
	if json.Unmarshal(data, &plain) != nil || len(plain) == 0 {
		return nil, errors.New("invalid BBS credentials file")
	}
	tokens := make(map[string][][32]byte, len(plain))
	for id, list := range plain {
		check := bbsprotocol.Handshake{Version: 1, BBSID: id, Token: "validation", Alias: "validation", TimeLeft: 1, Columns: 80, Rows: 24}
		if check.Validate() != nil || len(list) == 0 || len(list) > 4 {
			return nil, errors.New("invalid BBS credentials entry")
		}
		for _, token := range list {
			if len(token) < 32 || len(token) > 1024 {
				return nil, errors.New("BBS tokens require 32-1024 bytes")
			}
			tokens[id] = append(tokens[id], sha256.Sum256([]byte(token)))
		}
	}
	return tokens, nil
}
func (s *Server) authenticate(id, token string) bool {
	sum := sha256.Sum256([]byte(token))
	match := 0
	for _, want := range s.tokens[id] {
		match |= subtle.ConstantTimeCompare(sum[:], want[:])
	}
	return match == 1
}
func (s *Server) allowAttempt(addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, a := range s.attempts {
		if now.Sub(a.start) >= time.Minute {
			delete(s.attempts, key)
		}
	}
	a, found := s.attempts[host]
	if !found {
		if len(s.attempts) >= 4096 {
			return false
		}
		a.start = now
	}
	a.count++
	s.attempts[host] = a
	return a.count <= int(s.cfg.AttemptsPerMinute)
}
func (s *Server) claimBBS(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perBBS[id] >= int(s.cfg.MaxConnectionsPerBBS) {
		return false
	}
	s.perBBS[id]++
	return true
}
func (s *Server) releaseBBS(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.perBBS[id]--
	if s.perBBS[id] == 0 {
		delete(s.perBBS, id)
	}
}

func (s *Server) Listen() error {
	l, err := net.Listen("tcp", net.JoinHostPort(string(s.cfg.Host), strconv.Itoa(int(s.cfg.Port))))
	if err != nil {
		return err
	}
	s.listener = l
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if !s.allowAttempt(c.RemoteAddr()) {
				_ = c.Close()
				continue
			}
			select {
			case s.slots <- struct{}{}:
			default:
				_ = c.Close()
				continue
			}
			s.wg.Add(1)
			go func() { defer s.wg.Done(); defer func() { <-s.slots }(); s.serve(c) }()
		}
	}()
	return nil
}
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// StopAccepting also interrupts pending TLS handshakes. Close joins handlers;
// call it while the engine world worker is still running.
func (s *Server) StopAccepting() {
	s.cancel()
	if s.listener != nil {
		_ = s.listener.Close()
	}
}
func (s *Server) Close() { s.StopAccepting(); s.wg.Wait() }
func Reject(c net.Conn) {
	_ = bbsprotocol.WriteFrame(c, bbsprotocol.Ack{Version: 1, OK: false, Error: "session rejected"})
}
func (s *Server) serve(raw net.Conn) {
	defer raw.Close()
	stop := context.AfterFunc(s.ctx, func() { _ = raw.Close() })
	defer stop()
	deadline := time.Now().Add(time.Duration(s.cfg.HandshakeSeconds) * time.Second)
	_ = raw.SetDeadline(deadline)
	c := tls.Server(raw, s.tls)
	if c.HandshakeContext(s.ctx) != nil {
		return
	}
	var h bbsprotocol.Handshake
	if bbsprotocol.ReadFrame(c, &h) != nil || h.Validate() != nil || !s.authenticate(h.BBSID, h.Token) {
		Reject(c)
		return
	}
	h.Token = "" // Credentials are never passed into game code or logging.
	if !s.claimBBS(h.BBSID) {
		Reject(c)
		return
	}
	defer s.releaseBBS(h.BBSID)
	minutes := h.TimeLeft
	if minutes > int(s.cfg.MaxSessionMinutes) {
		minutes = int(s.cfg.MaxSessionMinutes)
	}
	expiry := time.Now().Add(time.Duration(minutes) * time.Minute)
	// Preserve the bounded handshake context through potentially queued attachment.
	s.handler(s.ctx, c, h, expiry, deadline)
}
