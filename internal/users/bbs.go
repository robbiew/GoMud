package users

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/GoMudEngine/GoMud/internal/bbsprotocol"
	"github.com/GoMudEngine/GoMud/internal/connections"
)

// BBSIdentity uses exact, case-sensitive UTF-8 aliases. Names are not slugged
// for identity purposes. Renames require explicit operator migration.
type BBSIdentity struct {
	ID    string `yaml:"id"`
	Alias string `yaml:"alias"`
}

var bbsLoginMu sync.Mutex

func bbsUsername(id, alias string) string {
	sum := sha256.Sum256([]byte(id + "\x00" + alias))
	return "bbs" + hex.EncodeToString(sum[:])[:29]
}

// LoginBBS must be called only after BBS credential verification, on the engine
// world thread. Deterministic indexed usernames avoid a second identity store;
// persisted identity is checked as well, so even a hash collision fails closed.
func LoginBBS(id, alias string, connID connections.ConnectionId) (*UserRecord, error) {
	check := bbsprotocol.Handshake{Version: 1, BBSID: id, Token: "validated", Alias: alias, TimeLeft: 1, Columns: 80, Rows: 24}
	if err := check.Validate(); err != nil {
		return nil, err
	}
	bbsLoginMu.Lock()
	defer bbsLoginMu.Unlock()
	username := bbsUsername(id, alias)
	if Exists(username) {
		u, err := LoadUser(username, true)
		if err != nil {
			return nil, err
		}
		if u.BBS == nil || u.BBS.ID != id || u.BBS.Alias != alias {
			return nil, errors.New("external identity collision")
		}
		logged, _, err := LoginUser(u, connID)
		return logged, err
	}
	u := NewUserRecord(0, connID)
	if err := u.SetUsername(username); err != nil {
		return nil, fmt.Errorf("BBS internal username policy: %w", err)
	}
	u.BBS = &BBSIdentity{ID: id, Alias: alias}
	u.Password = "!external-bbs-only" // PasswordMatches explicitly rejects all input.
	// Display names contain only safe engine name characters, with a stable
	// suffix to distinguish equal aliases across BBSes and prevent markup injection.
	prefix := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return -1
	}, alias)
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	if prefix == "" {
		prefix = "BBS"
	}
	display := prefix + "_" + username[len(username)-12:]
	if err := ValidateName(display); err != nil {
		display = "BBS_" + username[len(username)-12:]
		if err := ValidateName(display); err != nil {
			return nil, fmt.Errorf("BBS display name policy: %w", err)
		}
	}
	if _, found := GetCharacterIndex().Find(display); found {
		return nil, errors.New("BBS display name collision")
	}
	u.Character.Name = display
	if err := CreateUser(u); err != nil {
		return nil, err
	}
	GetCharacterIndex().Add(display, u.UserId)
	return u, nil
}
