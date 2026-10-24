package main

import (
	"context"
	"fmt"
	"time"

	"github.com/GoMudEngine/GoMud/internal/bbsprotocol"
	"github.com/GoMudEngine/GoMud/internal/configs"
	"github.com/GoMudEngine/GoMud/internal/connections"
	"github.com/GoMudEngine/GoMud/internal/events"
	"github.com/GoMudEngine/GoMud/internal/hooks"
	"github.com/GoMudEngine/GoMud/internal/inputhandlers"
	"github.com/GoMudEngine/GoMud/internal/mudlog"
	"github.com/GoMudEngine/GoMud/internal/plugins"
	"github.com/GoMudEngine/GoMud/internal/suggestions"
	"github.com/GoMudEngine/GoMud/internal/templates"
	"github.com/GoMudEngine/GoMud/internal/users"
)

// All bbsSession fields are accessed only on the world worker under LockMud.
type bbsSession struct {
	bbsID       string
	nodeNum     int
	userID      int
	expiry      time.Time
	entered     bool
	input       *connections.ClientInput
	shared      map[string]any
	suggestions suggestions.Suggestions
	lastInput   time.Time
}

func (w *World) runBBS(ctx context.Context, fn func() error) error {
	result := make(chan error, 1)
	task := func() {
		if err := ctx.Err(); err != nil {
			result <- err
			return
		}
		result <- fn()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.bbsDone:
		return errBBSUnavailable
	case w.bbsTasks <- task:
	}
	// Once queued, wait for completion: abandoning a successful attachment could
	// orphan an account. The world callback checks cancellation before mutation.
	select {
	case err := <-result:
		return err
	case <-w.bbsDone:
		return errBBSUnavailable
	}
}
func (w *World) attachBBS(cd *connections.ConnectionDetails, h bbsprotocol.Handshake, expiry time.Time) error {
	if !serverAlive.Load() || !time.Now().Before(expiry) {
		return errBBSUnavailable
	}
	u, err := users.LoginBBS(h.BBSID, h.Alias, cd.ConnectionId())
	if err != nil {
		return err
	}
	cs := connections.GetClientSettings(cd.ConnectionId())
	cs.Display.ScreenWidth, cs.Display.ScreenHeight = uint32(h.Columns), uint32(h.Rows)
	cs.DetectionComplete = true
	cs.MSPEnabled = false
	cs.SendTelnetGoAhead = false
	connections.OverwriteClientSettings(cd.ConnectionId(), cs)
	cd.AddInputHandler("AnsiHandler", inputhandlers.AnsiHandler)
	cd.AddInputHandler("SignalHandler", inputhandlers.SignalHandler)
	cd.AddInputHandler("CleanserInputHandler", inputhandlers.CleanserInputHandler)
	cd.AddInputHandler("TextPrefixHandler", inputhandlers.TextPrefixHandler)
	cd.AddInputHandler("EchoInputHandler", inputhandlers.EchoInputHandler)
	cd.AddInputHandler("HistoryInputHandler", inputhandlers.HistoryInputHandler)
	w.bbsSessions[cd.ConnectionId()] = &bbsSession{bbsID: h.BBSID, nodeNum: h.NodeNum, userID: u.UserId, expiry: expiry, input: &connections.ClientInput{ConnectionId: cd.ConnectionId()}, shared: map[string]any{}}
	return nil
}
func (w *World) enterBBS(id connections.ConnectionId) error {
	s := w.bbsSessions[id]
	u := users.GetByConnectionId(id)
	if s == nil || u == nil || u.UserId != s.userID || !time.Now().Before(s.expiry) {
		return errBBSUnavailable
	}
	cd := connections.Get(id)
	if cd == nil {
		return errBBSUnavailable
	}
	cd.SetState(connections.LoggedIn)
	plugins.OnNetConnect(cd)
	s.entered = true
	events.DoListeners(events.PlayerSpawn{UserId: u.UserId, ConnectionId: id, RoomId: u.Character.RoomId, Username: u.Username, CharacterName: u.Character.Name})
	return nil
}
func (w *World) inputBBS(id connections.ConnectionId, key []byte) error {
	s := w.bbsSessions[id]
	u := users.GetByConnectionId(id)
	cd := connections.Get(id)
	if s == nil || u == nil || u.UserId != s.userID || cd == nil {
		return errBBSUnavailable
	}
	if !time.Now().Before(s.expiry) {
		w.disconnectBBS(id)
		return errBBSUnavailable
	}
	if cd.InputDisabled() {
		return nil
	}
	ci := s.input
	ci.EnterPressed = false
	ci.TabPressed = false
	ci.BSPressed = false
	ci.DataIn = key
	next, _, err := cd.HandleInput(ci, s.shared)
	if err != nil {
		return err
	}
	if len(ci.Buffer) > 4096 {
		ci.Reset()
		u.SetUnsentText("", "")
		return fmt.Errorf("BBS command too long")
	}
	if !next {
		_, suggested := u.GetUnsentText()
		if ci.TabPressed {
			if s.suggestions.Count() == 0 {
				s.suggestions.Set(w.GetAutoComplete(u.UserId, string(ci.Buffer)))
			}
			if s.suggestions.Count() > 0 {
				suggested = s.suggestions.Next()
			}
		} else {
			suggested = ""
			s.suggestions.Clear()
		}
		u.SetUnsentText(string(ci.Buffer), suggested)
		if ci.TabPressed || ci.BSPressed {
			connections.SendTo([]byte(templates.AnsiParse(u.GetCommandPrompt())), id)
		}
		return nil
	}
	if ci.EnterPressed {
		minDelay := time.Duration(configs.GetConfig().Timing.TurnMs) * time.Millisecond
		if time.Since(s.lastInput) >= minDelay {
			_, suggested := u.GetUnsentText()
			events.AddToQueue(events.Input{UserId: u.UserId, InputText: string(ci.Buffer) + suggested})
			s.lastInput = time.Now()
		}
		ci.Reset()
		s.suggestions.Clear()
		u.SetUnsentText("", "")
	}
	return nil
}
func (w *World) disconnectBBS(id connections.ConnectionId) {
	s := w.bbsSessions[id]
	if s == nil {
		return
	}
	delete(w.bbsSessions, id) // EOF, expiry and stale ticker requests are idempotent.
	u := users.GetByConnectionId(id)
	if u == nil || u.UserId != s.userID {
		return
	}
	if s.entered {
		event := events.PlayerDespawn{UserId: u.UserId, RoomId: u.Character.RoomId, Username: u.Username, CharacterName: u.Character.Name, TimeOnline: u.GetOnlineInfo().OnlineTimeStr}
		events.DoListeners(event)
		// A script/plugin may cancel despawn. Forced BBS expiry still must save and
		// remove the session; preserve the final engine cleanup in that case.
		if users.GetByConnectionId(id) != nil {
			hooks.HandleLeave(event)
		}
	} else {
		if err := users.LogOutUserByConnectionId(id); err != nil {
			mudlog.Error("BBS logout", "connectionId", id, "error", err)
		}
	}
	_ = connections.Remove(id)
}
func (w *World) disconnectAllBBS() {
	for id := range w.bbsSessions {
		w.disconnectBBS(id)
	}
}
