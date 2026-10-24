package configs

import "fmt"

// BBSNetwork is read at startup. Secrets are loaded separately and never become
// part of config snapshots or the admin configuration API. Port 0 disables BBS.
type BBSNetwork struct {
	Host                 ConfigString `yaml:"Host"`
	Port                 ConfigInt    `yaml:"Port"`
	CertificateFile      ConfigString `yaml:"CertificateFile"`
	KeyFile              ConfigString `yaml:"KeyFile"`
	CredentialsFile      ConfigString `yaml:"CredentialsFile"`
	HandshakeSeconds     ConfigInt    `yaml:"HandshakeSeconds"`
	MaxConnections       ConfigInt    `yaml:"MaxConnections"`
	MaxConnectionsPerBBS ConfigInt    `yaml:"MaxConnectionsPerBBS"`
	MaxSessionMinutes    ConfigInt    `yaml:"MaxSessionMinutes"`
	AttemptsPerMinute    ConfigInt    `yaml:"AttemptsPerMinute"`
}

func (b BBSNetwork) Check() error {
	if b.Port == 0 {
		return nil
	}
	if b.Port < 1 || b.Port > 65535 {
		return fmt.Errorf("invalid BBS port")
	}
	if b.CertificateFile == "" || b.KeyFile == "" || b.CredentialsFile == "" {
		return fmt.Errorf("BBS requires certificate, key and credentials files")
	}
	if b.HandshakeSeconds < 1 || b.HandshakeSeconds > 60 || b.MaxConnections < 1 || b.MaxConnectionsPerBBS < 1 || b.MaxConnectionsPerBBS > b.MaxConnections || b.MaxSessionMinutes < 1 || b.MaxSessionMinutes > 10080 || b.AttemptsPerMinute < 1 {
		return fmt.Errorf("invalid BBS connection, timeout or session limits")
	}
	return nil
}
