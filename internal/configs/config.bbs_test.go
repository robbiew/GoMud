package configs

import "testing"

func TestBBSRequiresExplicitSafeConfiguration(t *testing.T) {
	if err := (BBSNetwork{}).Check(); err != nil {
		t.Fatal("default listener must stay disabled")
	}
	valid := BBSNetwork{Port: 4445, CertificateFile: "cert", KeyFile: "key", CredentialsFile: "credentials", HandshakeSeconds: 10, MaxConnections: 50, MaxConnectionsPerBBS: 10, MaxSessionMinutes: 60, AttemptsPerMinute: 30}
	if err := valid.Check(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*BBSNetwork){func(b *BBSNetwork) { b.Port = -1 }, func(b *BBSNetwork) { b.Port = 65536 }, func(b *BBSNetwork) { b.KeyFile = "" }, func(b *BBSNetwork) { b.HandshakeSeconds = 0 }, func(b *BBSNetwork) { b.MaxConnectionsPerBBS = 51 }, func(b *BBSNetwork) { b.MaxSessionMinutes = 0 }, func(b *BBSNetwork) { b.AttemptsPerMinute = 0 }} {
		cfg := valid
		mutate(&cfg)
		if err := cfg.Check(); err == nil {
			t.Fatal("accepted unsafe enabled BBS config")
		}
	}
}
