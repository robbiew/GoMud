package bbs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoMudEngine/GoMud/internal/bbsprotocol"
	"github.com/GoMudEngine/GoMud/internal/configs"
)

const token = "0123456789abcdef0123456789abcdef"

func fixture(t *testing.T, handler Handler) *Server {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath, creds := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), filepath.Join(dir, "credentials.json")
	for path, data := range map[string][]byte{certPath: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPath: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: raw}), creds: []byte(`{"bbs":["` + token + `"],"other":["` + strings.Repeat("b", 32) + `"]}`)} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(configs.BBSNetwork{Host: "127.0.0.1", Port: 4445, CertificateFile: configs.ConfigString(certPath), KeyFile: configs.ConfigString(keyPath), CredentialsFile: configs.ConfigString(creds), HandshakeSeconds: 1, MaxConnections: 2, MaxConnectionsPerBBS: 1, MaxSessionMinutes: 1, AttemptsPerMinute: 3}, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.StopAccepting)
	return s
}
func handshake() bbsprotocol.Handshake {
	return bbsprotocol.Handshake{Version: 1, BBSID: "bbs", Token: token, Alias: "Player", NodeNum: 2, TimeLeft: 60, Columns: 80, Rows: 24}
}
func connect(t *testing.T, s *Server) (*tls.Conn, <-chan struct{}) {
	t.Helper()
	raw, remote := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); s.serve(remote) }()
	cert, err := x509.ParseCertificate(s.tls.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	c := tls.Client(raw, &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13})
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = raw.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("handler leaked")
		}
	})
	return c, done
}
func TestAuthenticationRejectsBeforeHandler(t *testing.T) {
	for _, mode := range []string{"wrong-token", "wrong-bbs", "invalid", "version", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			var called atomic.Bool
			s := fixture(t, func(context.Context, net.Conn, bbsprotocol.Handshake, time.Time, time.Time) { called.Store(true) })
			c, _ := connect(t, s)
			h := handshake()
			switch mode {
			case "wrong-token":
				h.Token = "wrong"
			case "wrong-bbs":
				h.BBSID = "other"
			case "invalid":
				h.TimeLeft = 0
			case "version":
				h.Version = 2
			}
			if mode == "oversized" {
				_, _ = c.Write([]byte{0, 0, 32, 1})
			} else {
				if err := bbsprotocol.WriteFrame(c, h); err != nil {
					t.Fatal(err)
				}
			}
			var ack bbsprotocol.Ack
			if err := bbsprotocol.ReadFrame(c, &ack); err != nil {
				t.Fatal(err)
			}
			if ack.OK || called.Load() {
				t.Fatal("unauthenticated handler entered")
			}
		})
	}
}
func TestAuthenticationRedactsTokenClampsTimeAndLimitsBBS(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	s := fixture(t, func(ctx context.Context, c net.Conn, h bbsprotocol.Handshake, expiry, deadline time.Time) {
		if h.Token != "" || time.Until(expiry) > time.Minute || time.Until(deadline) > time.Second {
			t.Error("secret/limit contract violated")
		}
		_ = bbsprotocol.WriteFrame(c, bbsprotocol.Ack{Version: 1, OK: true})
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
	})
	c, _ := connect(t, s)
	_ = bbsprotocol.WriteFrame(c, handshake())
	var ack bbsprotocol.Ack
	if err := bbsprotocol.ReadFrame(c, &ack); err != nil || !ack.OK {
		t.Fatalf("ack %v %v", ack, err)
	}
	<-entered
	duplicate, _ := connect(t, s)
	_ = bbsprotocol.WriteFrame(duplicate, handshake())
	if err := bbsprotocol.ReadFrame(duplicate, &ack); err != nil || ack.OK {
		t.Fatalf("BBS cap not enforced: %v", err)
	}
}
func TestSlowHandshakeAndShutdownAreBounded(t *testing.T) {
	s := fixture(t, func(context.Context, net.Conn, bbsprotocol.Handshake, time.Time, time.Time) {
		t.Error("unexpected handler")
	})
	raw, remote := net.Pipe()
	defer raw.Close()
	done := make(chan struct{})
	go func() { s.serve(remote); close(done) }()
	s.StopAccepting()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("TLS handshake stranded")
	}
	s2 := fixture(t, func(context.Context, net.Conn, bbsprotocol.Handshake, time.Time, time.Time) {
		t.Error("unexpected handler")
	})
	c, _ := connect(t, s2)
	var ack bbsprotocol.Ack
	start := time.Now()
	_ = bbsprotocol.ReadFrame(c, &ack)
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatal("slow handshake not bounded")
	}
}
func TestCredentialRotationAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	data, _ := json.Marshal(map[string][]string{"bbs": {token, strings.Repeat("a", 32)}})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	tokens, err := loadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{tokens: tokens}
	if !s.authenticate("bbs", token) || !s.authenticate("bbs", strings.Repeat("a", 32)) || s.authenticate("other", token) {
		t.Fatal("credential binding failed")
	}
	_ = os.Chmod(path, 0644)
	if _, err := loadCredentials(path); err == nil {
		t.Fatal("accepted exposed credential file")
	}
}
func TestAttemptLimiterBoundsPeersAndWindow(t *testing.T) {
	s := fixture(t, func(context.Context, net.Conn, bbsprotocol.Handshake, time.Time, time.Time) {})
	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}
	for i := 0; i < 3; i++ {
		if !s.allowAttempt(addr) {
			t.Fatal("unexpected denial")
		}
	}
	if s.allowAttempt(addr) {
		t.Fatal("rate limit not enforced")
	}
	s.attempts["127.0.0.1"] = attempt{start: time.Now().Add(-2 * time.Minute), count: 100}
	if !s.allowAttempt(addr) {
		t.Fatal("window did not expire")
	}
}
