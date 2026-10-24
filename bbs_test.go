package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/GoMudEngine/GoMud/internal/bbsprotocol"
	"github.com/GoMudEngine/GoMud/internal/configs"
	"github.com/GoMudEngine/GoMud/internal/connections"
	"github.com/GoMudEngine/GoMud/internal/events"
	"github.com/GoMudEngine/GoMud/internal/hooks"
	"github.com/GoMudEngine/GoMud/internal/mudlog"
	"github.com/GoMudEngine/GoMud/internal/rooms"
	"github.com/GoMudEngine/GoMud/internal/templates"
	"github.com/GoMudEngine/GoMud/internal/users"
	"github.com/GoMudEngine/GoMud/internal/util"
)

func setupBBSWorld(t *testing.T) (*World, *rooms.Room, *atomic.Int32) {
	t.Helper()
	mudlog.SetupLogger(events.GetLogger(), "M", "", false)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "users"), 0700)
	override := filepath.Join(dir, "overrides.yaml")
	data := fmt.Sprintf("FilePaths.DataFiles: %q\nServer.OnLoginCommands: []\nTiming.TurnMs: 1\nNetwork.LinkDeadSeconds: 60\n", dir)
	if err := os.WriteFile(override, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", override)
	if err := configs.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	users.ResetActiveUsers()
	var characterNames []string
	users.GetCharacterIndex().ForEach(func(name string, _ int) bool { characterNames = append(characterNames, name); return true })
	for _, name := range characterNames {
		users.GetCharacterIndex().Remove(name)
	}
	users.InitUserIndex()
	if err := users.GetUserIndex().Create(); err != nil {
		t.Fatal(err)
	}
	events.ClearListeners()
	old := worldManager
	w := &World{bbsTasks: make(chan func(), 128), bbsDone: make(chan struct{}), bbsSessions: make(map[connections.ConnectionId]*bbsSession)}
	worldManager = w
	serverAlive.Store(true)
	// Exercise actual room departure and persistence hooks, with a minimal spawn
	// callback so these lifecycle tests need no NPC scripts or world simulation.
	roomID, err := rooms.CreateZone(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	room := rooms.LoadRoom(roomID)
	if room == nil {
		t.Fatal("fixture room not available")
	}
	templates.RegisterFS(fstest.MapFS{"templates/player-despawn.template": {Data: []byte("{{.}} left the room.\r\n")}, "templates/goodbye.template": {Data: []byte("Goodbye.\r\n")}})
	departures := new(atomic.Int32)
	events.RegisterListener(events.PlayerSpawn{}, func(e events.Event) events.ListenerReturn {
		evt := e.(events.PlayerSpawn)
		u := users.GetByUserId(evt.UserId)
		u.Character.RoomId = roomID
		room.AddPlayer(u.UserId)
		connections.SendTo([]byte("\x1b[32mWelcome\r\n"), evt.ConnectionId)
		return events.Continue
	})
	events.RegisterListener(events.PlayerDespawn{}, func(e events.Event) events.ListenerReturn { departures.Add(1); return events.Continue })
	events.RegisterListener(events.PlayerDespawn{}, hooks.HandleLeave, events.Last)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(w.bbsDone)
		for {
			select {
			case task := <-w.bbsTasks:
				util.LockMud()
				task()
				util.UnlockMud()
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = w.runBBS(context.Background(), func() error { w.disconnectAllBBS(); return nil })
		close(stop)
		wg.Wait()
		worldManager = old
		serverAlive.Store(false)
		connections.Cleanup()
		users.ResetActiveUsers()
		events.ClearListeners()
	})
	return w, room, departures
}
func testHandshake() bbsprotocol.Handshake {
	return bbsprotocol.Handshake{Version: 1, BBSID: "test-bbs", Alias: "Caller", NodeNum: 3, TimeLeft: 1, Columns: 100, Rows: 30}
}
func startBBSHandler(t *testing.T, h bbsprotocol.Handshake, expiry time.Time) (net.Conn, <-chan struct{}) {
	t.Helper()
	c, s := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer s.Close()
		handleBBSConnection(context.Background(), s, h, expiry, time.Now().Add(time.Second))
	}()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	t.Cleanup(func() {
		_ = c.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("BBS handler leaked")
		}
	})
	return c, done
}
func ackAndWelcome(t *testing.T, c net.Conn) {
	t.Helper()
	var ack bbsprotocol.Ack
	if err := bbsprotocol.ReadFrame(c, &ack); err != nil || !ack.OK {
		t.Fatalf("ack %v %v", ack, err)
	}
	b := make([]byte, len("\x1b[32mWelcome\r\n"))
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "\x1b[32mWelcome\r\n" {
		t.Fatalf("pre-game data %q %v", b, err)
	}
}
func TestBBSAcknowledgmentInputExpiryAndSavedCharacter(t *testing.T) {
	w, room, departures := setupBBSWorld(t)
	var roomMessages atomic.Int32
	var commands []string
	events.RegisterListener(events.Message{}, func(e events.Event) events.ListenerReturn {
		if strings.Contains(e.(events.Message).Text, "left the room") {
			roomMessages.Add(1)
		}
		return events.Continue
	})
	events.RegisterListener(events.Input{}, func(e events.Event) events.ListenerReturn {
		commands = append(commands, e.(events.Input).InputText)
		return events.Continue
	})
	c, done := startBBSHandler(t, testHandshake(), time.Now().Add(150*time.Millisecond))
	ackAndWelcome(t, c)
	var userID int
	var username string
	if err := w.runBBS(context.Background(), func() error {
		for id, s := range w.bbsSessions {
			userID = s.userID
			u := users.GetByUserId(userID)
			username = u.Username
			cs := connections.GetClientSettings(id)
			if s.bbsID != "test-bbs" || s.nodeNum != 3 {
				t.Error("BBS identity/node metadata missing")
			}
			if cs.Display.ScreenWidth != 100 || cs.Display.ScreenHeight != 30 || cs.SendTelnetGoAhead || !cs.DetectionComplete {
				t.Error("BBS settings not attached")
			}
			u.Character.Gold = 12345
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Keep consuming output so normal echo/goodbye cannot block this pipe.
	received := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(c); received <- b }()
	if _, err := c.Write([]byte("loox\bk\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("expiry did not hang up")
	}
	output := <-received
	if bytes.Contains(output, []byte{255}) {
		t.Fatal("Telnet negotiation leaked")
	}
	if departures.Load() != 1 {
		t.Fatalf("departures=%d", departures.Load())
	}
	if err := w.runBBS(context.Background(), func() error {
		events.ProcessEvents()
		if roomMessages.Load() != 1 {
			t.Errorf("room messages=%d", roomMessages.Load())
		}
		if len(commands) != 1 || commands[0] != "look" {
			t.Errorf("commands=%q", commands)
		}
		if users.GetByUserId(userID) != nil {
			t.Error("expired BBS user remains active")
		}
		if _, found := room.RemovePlayer(userID); found {
			t.Error("room retained expired BBS player")
		}
		w.disconnectBBS(999999) // Stale descriptor requests are harmless.
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := users.LoadUser(username, true)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Character.Gold != 12345 {
		t.Fatal("expiry did not save latest mutable state")
	}
}
func TestBBSDuplicateAndEOFAreIdempotent(t *testing.T) {
	w, _, departures := setupBBSWorld(t)
	first, done := startBBSHandler(t, testHandshake(), time.Now().Add(time.Minute))
	ackAndWelcome(t, first)
	second, _ := startBBSHandler(t, testHandshake(), time.Now().Add(time.Minute))
	var ack bbsprotocol.Ack
	if err := bbsprotocol.ReadFrame(second, &ack); err != nil || ack.OK {
		t.Fatalf("duplicate was accepted: %v", err)
	}
	_ = first.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("EOF did not stop")
	}
	if departures.Load() != 1 {
		t.Fatal("incorrect departure count")
	}
	if err := w.runBBS(context.Background(), func() error {
		if len(w.bbsSessions) != 0 {
			t.Error("session remains after EOF")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestBBSPreAckFailureNeverSendsGameBytes(t *testing.T) {
	w, _, departures := setupBBSWorld(t)
	c, done := startBBSHandler(t, testHandshake(), time.Now().Add(time.Minute))
	// Close before reading the acknowledgment. Provisioned session must be saved
	// and detached without a spawn/despawn announcement or leaked game output.
	_ = c.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-ack cleanup stuck")
	}
	_ = w.runBBS(context.Background(), func() error {
		if len(w.bbsSessions) != 0 {
			t.Error("pre-ack orphan")
		}
		return nil
	})
	if departures.Load() != 0 {
		t.Fatal("unentered player announced departure")
	}
}
func TestBBSStreamHandlesFragmentedANSIUTF8AndMultipleLines(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	stream := newBBSStream(s)
	go func() {
		for _, part := range []string{"look\r\nwho\r", "\x1b[", "A", "é"} {
			_, _ = c.Write([]byte(part))
		}
		_ = c.Close()
	}()
	var keys []string
	b := make([]byte, 128)
	for {
		n, err := stream.Read(b)
		if err != nil {
			break
		}
		keys = append(keys, string(b[:n]))
	}
	want := []string{"l", "o", "o", "k", "\r", "w", "h", "o", "\r", "\x1b[A", "é"}
	if strings.Join(keys, "|") != strings.Join(want, "|") {
		t.Fatalf("logical keys %q", keys)
	}
}
