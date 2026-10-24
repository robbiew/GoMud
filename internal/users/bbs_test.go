package users

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoMudEngine/GoMud/internal/configs"
	"github.com/GoMudEngine/GoMud/internal/copyover"
	"github.com/GoMudEngine/GoMud/internal/events"
	"github.com/GoMudEngine/GoMud/internal/mudlog"
)

func setupBBSUsers(t *testing.T) string {
	t.Helper()
	mudlog.SetupLogger(events.GetLogger(), "error", "", false)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(wd, "../.."))
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "users"), 0700); err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(dir, "overrides.yaml")
	if err := os.WriteFile(override, []byte(fmt.Sprintf("FilePaths.DataFiles: %q\nServer.Seed: test\n", dir)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", override)
	if err := configs.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	oldCharacters := characterIndex
	characterIndex = &CharacterIndex{byName: make(map[string]int)}
	t.Cleanup(func() { characterIndex = oldCharacters })
	old := userIndex
	userIndex = nil
	resetUserManager()
	InitUserIndex()
	if err := GetUserIndex().Create(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { userIndex = old; resetUserManager() })
	return dir
}
func TestBBSProvisionReconnectIsolationAndPasswordDenial(t *testing.T) {
	setupBBSUsers(t)
	first, err := LoginBBS("bbs-one", "Admin", 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.Role != RoleUser || len(first.Permissions) != 0 || first.HasPlaintextPassword() || first.PasswordMatches(first.Password) || first.PasswordMatches("") {
		t.Fatal("unsafe BBS account")
	}
	id := first.UserId
	if _, err := LoginBBS("bbs-one", "Admin", 2); err == nil {
		t.Fatal("duplicate live login allowed")
	}
	other, err := LoginBBS("bbs-two", "Admin", 3)
	if err != nil {
		t.Fatal(err)
	}
	if other.UserId == id || other.Character.Name == first.Character.Name {
		t.Fatal("cross-BBS alias collision")
	}
	caseVariant, err := LoginBBS("bbs-one", "admin", 4)
	if err != nil {
		t.Fatal(err)
	}
	if caseVariant.UserId == id {
		t.Fatal("case-sensitive identity changed")
	}
	if err := LogOutUserByConnectionId(1); err != nil {
		t.Fatal(err)
	}
	// Reload the derived index to prove the identity survives an engine restart.
	InitUserIndex()
	again, err := LoginBBS("bbs-one", "Admin", 5)
	if err != nil {
		t.Fatal(err)
	}
	if again.UserId != id || again.BBS.ID != "bbs-one" || again.BBS.Alias != "Admin" {
		t.Fatal("identity not persistent")
	}
}
func TestBBSCannotClaimLocalAccountOrInjectMarkup(t *testing.T) {
	setupBBSUsers(t)
	local := NewUserRecord(0, 1)
	if err := local.SetUsername(bbsUsername("bbs", "Caller")); err != nil {
		t.Fatal(err)
	}
	if err := CreateUser(local); err != nil {
		t.Fatal(err)
	}
	if _, err := LoginBBS("bbs", "Caller", 2); err == nil {
		t.Fatal("claimed existing local account")
	}
	safe, err := LoginBBS("bbs", "<ansi fg=red>Caller", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range safe.Character.Name {
		if r == '<' || r == '>' {
			t.Fatal("display markup injected")
		}
	}
}
func TestBBSProvisionFailureDoesNotIndexPhantomUser(t *testing.T) {
	dir := setupBBSUsers(t)
	// Force a record write failure while leaving users.idx available.
	path := filepath.Join(dir, "users", "1.yaml")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoginBBS("bbs", "Caller", 1); err == nil {
		t.Fatal("expected save failure")
	}
	if Exists(bbsUsername("bbs", "Caller")) {
		t.Fatal("failed user indexed")
	}
	if GetByConnectionId(1) != nil {
		t.Fatal("failed user attached")
	}
}

func TestBBSCopyoverOmitsExternalSessionAndKeepsLocalUser(t *testing.T) {
	setupBBSUsers(t)
	external, err := LoginBBS("bbs", "Caller", 1)
	if err != nil {
		t.Fatal(err)
	}
	local := NewUserRecord(0, 2)
	if err := local.SetUsername("localcaller"); err != nil {
		t.Fatal(err)
	}
	if err := CreateUser(local); err != nil {
		t.Fatal(err)
	}
	copyover.ResetRegistry()
	defer copyover.ResetRegistry()
	copyover.Register(CopyoverContributor())
	var buf bytes.Buffer
	if err := copyover.Save(&buf); err != nil {
		t.Fatal(err)
	}
	var count uint32
	_ = binary.Read(&buf, binary.BigEndian, &count)
	var nameSize uint16
	_ = binary.Read(&buf, binary.BigEndian, &nameSize)
	name := make([]byte, nameSize)
	_, _ = io.ReadFull(&buf, name)
	var size uint32
	_ = binary.Read(&buf, binary.BigEndian, &size)
	data := make([]byte, size)
	_, _ = io.ReadFull(&buf, data)
	var state usersState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Entries) != 1 || state.Entries[0].UserId != local.UserId || state.Entries[0].UserId == external.UserId {
		t.Fatal("copyover session filtering failed")
	}
}
