package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoMudEngine/GoMud/internal/badinputtracker"
	"github.com/GoMudEngine/GoMud/internal/configs"
	"github.com/GoMudEngine/GoMud/internal/connections"
	"github.com/GoMudEngine/GoMud/internal/events"
	"github.com/GoMudEngine/GoMud/internal/mobcommands"
	"github.com/GoMudEngine/GoMud/internal/mobs"
	"github.com/GoMudEngine/GoMud/internal/mudlog"
	"github.com/GoMudEngine/GoMud/internal/prompt"
	"github.com/GoMudEngine/GoMud/internal/rooms"
	"github.com/GoMudEngine/GoMud/internal/scripting"
	"github.com/GoMudEngine/GoMud/internal/suggestions"
	"github.com/GoMudEngine/GoMud/internal/templates"
	"github.com/GoMudEngine/GoMud/internal/term"
	"github.com/GoMudEngine/GoMud/internal/usercommands"
	"github.com/GoMudEngine/GoMud/internal/users"
	"github.com/GoMudEngine/GoMud/internal/util"
	"github.com/GoMudEngine/GoMud/internal/web"
)

type WorldInput struct {
	FromId    int
	InputText string
	ReadyTurn uint64
}

func (wi WorldInput) Id() int {
	return wi.FromId
}

type World struct {
	bbsTasks           chan func()
	bbsDone            chan struct{}
	bbsSessions        map[connections.ConnectionId]*bbsSession
	worldInput         chan WorldInput
	ignoreInput        map[int]uint64 // userid->turn set to ignore
	enterWorldUserId   chan [2]int
	leaveWorldUserId   chan int
	logoutConnectionId chan connections.ConnectionId
	linkDeadFlag       chan [2]int
	//
	eventRequeue          []events.Event
	userInputEventTracker map[int]struct{}
	mobInputEventTracker  map[int]struct{}
}

func NewWorld(osSignalChan chan os.Signal) *World {

	w := &World{
		bbsTasks:           make(chan func(), 128),
		bbsDone:            make(chan struct{}),
		bbsSessions:        make(map[connections.ConnectionId]*bbsSession),
		worldInput:         make(chan WorldInput),
		ignoreInput:        make(map[int]uint64),
		enterWorldUserId:   make(chan [2]int),
		leaveWorldUserId:   make(chan int),
		logoutConnectionId: make(chan connections.ConnectionId),
		linkDeadFlag:       make(chan [2]int),
		//
		eventRequeue:          []events.Event{},
		userInputEventTracker: map[int]struct{}{},
		mobInputEventTracker:  map[int]struct{}{},
	}

	// System commands
	events.RegisterListener(events.System{}, w.HandleSystemEvents)
	events.RegisterListener(events.Input{}, w.HandleInputEvents)

	connections.SetShutdownChan(osSignalChan)

	return w
}

func (w *World) HandleInputEvents(e events.Event) events.ListenerReturn {

	input, typeOk := e.(events.Input)
	if !typeOk {
		mudlog.Error("Event", "Expected Type", "Input", "Actual Type", e.Type())
		return events.Continue
	}

	var turnCt uint64 = util.GetTurnCount()

	//mudlog.Debug(`Event`, `type`, input.Type(), `UserId`, input.UserId, `MobInstanceId`, input.MobInstanceId, `WaitTurns`, input.WaitTurns, `InputText`, input.InputText)

	// If it's a mob
	if input.MobInstanceId > 0 {

		// If an event was already processed for this user this turn, skip
		// We put this first, so that any delayed command for a mob will block
		// the command pipeline for the mob until executed.
		if _, ok := w.mobInputEventTracker[input.MobInstanceId]; ok {
			return events.CancelAndRequeue
		}

		// 0 and below, process immediately and don't count towards limit
		if input.ReadyTurn <= 0 {
			w.processMobInput(input.MobInstanceId, input.InputText)
			return events.Continue
		}

		// This will cause any pending command to block all further pending commands
		// This is important, otherwise we issue a command with a delay, but other commands
		// Get executed while we wait.
		w.mobInputEventTracker[input.MobInstanceId] = struct{}{}

		if input.ReadyTurn > turnCt {
			return events.CancelAndRequeue
		}

		w.processMobInput(input.MobInstanceId, input.InputText)

		return events.Continue
	}

	// 0 and below, process immediately and don't count towards limit
	if input.ReadyTurn <= 0 {

		// If this command was potentially blocking input, unblock it now.
		if input.Flags.Has(events.CmdUnBlockInput) {

			if _, ok := w.ignoreInput[input.UserId]; ok {
				delete(w.ignoreInput, input.UserId)
				if user := users.GetByUserId(input.UserId); user != nil {
					user.UnblockInput()
				}
			}

		}

		w.processInput(input.UserId, input.InputText, input.Flags)

		return events.Continue
	}

	// If an event was already processed for this user this turn, skip
	if _, ok := w.userInputEventTracker[input.UserId]; ok {
		return events.CancelAndRequeue
	}

	// 0 means process immediately
	// however, process no further events from this user until next turn
	if input.ReadyTurn > turnCt {

		// If this is a multi-turn wait, block further input if flagged to do so
		if input.Flags.Has(events.CmdBlockInput) {

			if _, ok := w.ignoreInput[input.UserId]; !ok {
				w.ignoreInput[input.UserId] = turnCt
			}

			input.Flags.Remove(events.CmdBlockInput)
		}

		return events.CancelAndRequeue
	}

	//
	// Event ready to be processed
	//

	// If this command was potentially blocking input, unblock it now.
	if input.Flags.Has(events.CmdUnBlockInput) {

		if _, ok := w.ignoreInput[input.UserId]; ok {
			delete(w.ignoreInput, input.UserId)
			if user := users.GetByUserId(input.UserId); user != nil {
				user.UnblockInput()
			}
		}

	}

	w.processInput(input.UserId, input.InputText, events.EventFlag(input.Flags))

	w.userInputEventTracker[input.UserId] = struct{}{}

	return events.Continue
}

// Checks whether their level is too high for a guide
func (w *World) HandleSystemEvents(e events.Event) events.ListenerReturn {

	sys, typeOk := e.(events.System)
	if !typeOk {
		mudlog.Error("Event", "Expected Type", "System", "Actual Type", e.Type())
		return events.Continue
	}

	if sys.Command == `reload` {

		events.AddToQueue(events.Broadcast{
			Text: `Reloading flat files...`,
		})

		loadAllDataFiles(true)

		events.AddToQueue(events.Broadcast{
			Text:            `Done.` + term.CRLFStr,
			SkipLineRefresh: true,
		})

	} else if sys.Command == `kick` {
		w.Kick(sys.Data.(int), sys.Description)
	} else if sys.Command == `leaveworld` {

		if userInfo := users.GetByUserId(sys.Data.(int)); userInfo != nil {
			events.AddToQueue(events.PlayerDespawn{
				UserId:        userInfo.UserId,
				RoomId:        userInfo.Character.RoomId,
				Username:      userInfo.Username,
				CharacterName: userInfo.Character.Name,
				TimeOnline:    userInfo.GetOnlineInfo().OnlineTimeStr,
			})
		}

	} else if sys.Command == `logoff` {

		if user := users.GetByUserId(sys.Data.(int)); user != nil {

			user.EventLog.Add(`conn`, `Logged off`)

			events.AddToQueue(events.PlayerDespawn{
				UserId:        user.UserId,
				RoomId:        user.Character.RoomId,
				Username:      user.Username,
				CharacterName: user.Character.Name,
				TimeOnline:    user.GetOnlineInfo().OnlineTimeStr,
			})

		}

	}

	return events.Continue
}

// Send input to the world.
// Just sends via a channel. Will block until read.
func (w *World) SendInput(i WorldInput) {
	w.worldInput <- i
}

func (w *World) SendEnterWorld(userId int, roomId int) {
	w.enterWorldUserId <- [2]int{userId, roomId}
}

func (w *World) SendLeaveWorld(userId int) {
	if !serverAlive.Load() {
		return
	}
	w.leaveWorldUserId <- userId
}

func (w *World) SendLogoutConnectionId(connId connections.ConnectionId) {
	if !serverAlive.Load() {
		return
	}
	w.logoutConnectionId <- connId
}

func (w *World) SendSetLinkDead(userId int, on bool) {
	if !serverAlive.Load() {
		return
	}
	if on {
		w.linkDeadFlag <- [2]int{userId, 1}
	} else {
		w.linkDeadFlag <- [2]int{userId, 0}
	}
}

func (w *World) logOutUserByConnectionId(connectionId connections.ConnectionId) {

	if err := users.LogOutUserByConnectionId(connectionId); err != nil {
		mudlog.Error("Log Out Error", "connectionId", connectionId, "error", err)
	}
}

func (w *World) enterWorld(userId int, roomId int) {

	if userInfo := users.GetByUserId(userId); userInfo != nil {
		events.AddToQueue(events.PlayerSpawn{
			UserId:        userInfo.UserId,
			ConnectionId:  userInfo.ConnectionId(),
			RoomId:        userInfo.Character.RoomId,
			Username:      userInfo.Username,
			CharacterName: userInfo.Character.Name,
		})
	}

	w.UpdateStats()

	// Put htme in the room
	rooms.MoveToRoom(userId, roomId, true)
}

/*
users can be:
Disconnected	+ OutWorld (no presence)	No record in connections.netConnections or users.LinkDeadConnections	| user object in room
Connected		+ OutWorld (logging in) 	Has record in connections.netConnections 							| user object in room
Connected		+ InWorld  (non-link-dead)	No record in users.LinkDeadConnections								| no link-dead flag		| user object in room
Disconnected	+ InWorld  (link-dead)			Has record in users.LinkDeadConnections 								| has link-dead flag		| user object in room
*/

func (w *World) GetAutoComplete(userId int, inputText string) []string {
	return suggestions.GetAutoComplete(userId, inputText)
}

const (
	// Used in GameTickWorker()
	// Used in MaintenanceWorker()
	roomMaintenancePeriod = time.Second * 3  // Every 3 seconds run room maintenance.
	serverStatsLogPeriod  = time.Second * 60 // Every 60 seconds log server stats.
	ansiAliasReloadPeriod = time.Second * 4  // Every 4 seconds reload ansi aliases.
)

func (w *World) MainWorker(shutdown chan bool, wg *sync.WaitGroup) {
	defer close(w.bbsDone)

	wg.Add(1)

	mudlog.Info("MainWorker", "state", "Started")
	defer func() {
		mudlog.Warn("MainWorker", "state", "Stopped")
		wg.Done()
	}()

	c := configs.GetConfig()

	roomUpdateTimer := time.NewTimer(roomMaintenancePeriod)
	ansiAliasTimer := time.NewTimer(ansiAliasReloadPeriod)
	eventLoopTimer := time.NewTimer(time.Millisecond)
	turnTimer := time.NewTimer(time.Duration(c.Timing.TurnMs) * time.Millisecond)
	statsTimer := time.NewTimer(time.Duration(10) * time.Second)

loop:
	for {

		// The reason for
		// util.LockGame() / util.UnlockGame()
		// In each of these cases is to lock down the
		// logic for when other processes need to query data
		// such as the webserver

		select {
		case task := <-w.bbsTasks:
			util.LockMud()
			task()
			util.UnlockMud()

		case <-shutdown:

			mudlog.Warn(`MainWorker`, `action`, `shutdown received`)

			util.LockMud()
			if err := rooms.SaveAllRooms(); err != nil {
				mudlog.Error("rooms.SaveAllRooms()", "error", err.Error())
			}
			users.SaveAllUsers() // Save all user data too.
			util.UnlockMud()

			break loop
		case <-statsTimer.C:

			// TODO: Move this to events
			util.LockMud()

			w.UpdateStats()
			// save the round counter.
			util.SaveRoundCount(c.FilePaths.DataFiles.String() + `/` + util.RoundCountFilename)

			util.UnlockMud()

			statsTimer.Reset(time.Duration(10) * time.Second)

		case <-roomUpdateTimer.C:

			// TODO: Move this to events
			util.LockMud()
			scripting.PruneRoomVMs(rooms.RoomMaintenance()...)
			scripting.PruneRoomVMs(rooms.EphemeralRoomMaintenance()...)
			util.UnlockMud()

			roomUpdateTimer.Reset(roomMaintenancePeriod)

		case <-ansiAliasTimer.C:

			// TODO: Move this to events
			util.LockMud()
			templates.LoadAliases()
			util.UnlockMud()

			ansiAliasTimer.Reset(ansiAliasReloadPeriod)

		case <-eventLoopTimer.C:

			eventLoopTimer.Reset(time.Millisecond)

			util.LockMud()
			w.EventLoop()
			util.UnlockMud()

		case <-turnTimer.C:

			util.LockMud()
			turnTimer.Reset(time.Duration(c.Timing.TurnMs) * time.Millisecond)

			turnCt := util.IncrementTurnCount()

			events.AddToQueue(events.NewTurn{TurnNumber: turnCt, TimeNow: time.Now()})

			// After a full round of turns, we can do a round tick.
			if turnCt%uint64(c.Timing.TurnsPerRound()) == 0 {

				roundNumber := util.IncrementRoundCount()

				events.AddToQueue(events.NewRound{RoundNumber: roundNumber, TimeNow: time.Now()})
			}

			util.UnlockMud()

		case enterWorldUserId := <-w.enterWorldUserId: // [2]int

			util.LockMud()
			w.enterWorld(enterWorldUserId[0], enterWorldUserId[1])
			util.UnlockMud()

		case leaveWorldUserId := <-w.leaveWorldUserId: // int

			util.LockMud()
			if userInfo := users.GetByUserId(leaveWorldUserId); userInfo != nil {
				events.AddToQueue(events.PlayerDespawn{
					UserId:        userInfo.UserId,
					RoomId:        userInfo.Character.RoomId,
					Username:      userInfo.Username,
					CharacterName: userInfo.Character.Name,
					TimeOnline:    userInfo.GetOnlineInfo().OnlineTimeStr,
				})
			}
			util.UnlockMud()

		case logoutConnectionId := <-w.logoutConnectionId: //  connections.ConnectionId

			util.LockMud()
			w.logOutUserByConnectionId(logoutConnectionId)
			util.UnlockMud()

		case linkDeadFlag := <-w.linkDeadFlag: //  [2]int
			if linkDeadFlag[1] == 1 {

				util.LockMud()
				users.SetLinkDeadUser(linkDeadFlag[0])
				events.AddToQueue(events.PlayerChanged{
					UserId: linkDeadFlag[0],
				})
				util.UnlockMud()

			}
		}
		c = configs.GetConfig()
	}

}

// Should be goroutine/threadsafe
// Only reads from world channel
func (w *World) InputWorker(shutdown chan bool, wg *sync.WaitGroup) {
	wg.Add(1)

	mudlog.Info("InputWorker", "state", "Started")
	defer func() {
		mudlog.Warn("InputWorker", "state", "Stopped")
		wg.Done()
	}()

loop:
	for {
		select {
		case <-shutdown:
			mudlog.Warn(`InputWorker`, `action`, `shutdown received`)
			break loop
		case wi := <-w.worldInput:

			events.AddToQueue(events.Input{
				UserId:    wi.FromId,
				InputText: wi.InputText,
				ReadyTurn: util.GetTurnCount(),
			})

		}
	}
}

func (w *World) processInput(userId int, inputText string, flags events.EventFlag) {

	user := users.GetByUserId(userId)
	if user == nil { // Something went wrong. User not found.
		mudlog.Error("User not found", "userId", userId)
		return
	}

	var activeQuestion *prompt.Question = nil
	hadPrompt := false
	if cmdPrompt := user.GetPrompt(); cmdPrompt != nil {
		hadPrompt = true
		if activeQuestion = cmdPrompt.GetNextQuestion(); activeQuestion != nil {

			activeQuestion.Answer(string(inputText))
			inputText = ``

			// set the input buffer to invoke the command prompt it was relevant to
			if cmdPrompt.Command != `` {
				inputText = cmdPrompt.Command + " " + cmdPrompt.Rest
			}
		} else {
			// If a prompt was found, but no pending questions, clear it.
			user.ClearPrompt()
		}

	}

	command := ``
	remains := ``

	var err error
	handled := false

	inputText = strings.TrimSpace(inputText)

	if len(inputText) > 0 {

		// Update their last input
		// Must be actual text, blank space doesn't count.
		user.SetLastInputRound(util.GetRoundCount())

		// Check for macros
		if user.Macros != nil && len(inputText) == 2 {
			if macro, ok := user.Macros[inputText]; ok {
				handled = true
				readyTurn := util.GetTurnCount()
				for _, newCmd := range strings.Split(macro, `;`) {
					if newCmd == `` {
						continue
					}

					events.AddToQueue(events.Input{
						UserId:    userId,
						InputText: newCmd,
						ReadyTurn: readyTurn,
					})

					readyTurn++
				}
			}
		}

		if !handled {

			// Lets users use gossip/say shortcuts without a space
			if len(inputText) > 1 {
				if inputText[0] == '`' || inputText[0] == '.' {
					inputText = fmt.Sprintf(`%s %s`, string(inputText[0]), string(inputText[1:]))
				}
			}

			if index := strings.Index(inputText, " "); index != -1 {
				command, remains = strings.ToLower(inputText[0:index]), inputText[index+1:]
			} else {
				command = inputText
			}

			handled, err = usercommands.TryCommand(command, remains, userId, flags)
			if err != nil {
				mudlog.Warn("user-TryCommand", "command", command, "remains", remains, "error", err.Error())
			}
		}

	} else {
		connId := user.ConnectionId()
		connections.SendTo([]byte(templates.AnsiParse(user.GetCommandPrompt())), connId)
	}

	if !handled {
		if len(command) > 0 {

			badinputtracker.TrackBadCommand(command, remains)

			user.SendText(fmt.Sprintf(`<ansi fg="command">%s</ansi> not recognized. Type <ansi fg="command">help</ansi> for commands.`, command))
			user.Command(`emote @looks a little confused`)
		}
	}

	// If they had an input prompt, but now they don't, lets make sure to resend a status prompt
	if hadPrompt || (!hadPrompt && user.GetPrompt() != nil) {
		connId := user.ConnectionId()
		connections.SendTo([]byte(templates.AnsiParse(user.GetCommandPrompt())), connId)
	}
	// Removing this as possibly redundant.
	// Leaving in case I need to remember that I did it...
	//connId := user.ConnectionId()
	//connections.SendTo([]byte(templates.AnsiParse(user.GetCommandPrompt(true))), connId)

}

func (w *World) processMobInput(mobInstanceId int, inputText string) {
	// No need to select the channel this way

	mob := mobs.GetInstance(mobInstanceId)
	if mob == nil { // Something went wrong. User not found.
		if !mobs.RecentlyDied(mobInstanceId) {
			mudlog.Error("Mob not found", "mobId", mobInstanceId, "where", "processMobInput()")
		}
		return
	}

	command := ""
	remains := ""

	handled := false
	var err error

	if len(inputText) > 0 {

		if index := strings.Index(inputText, " "); index != -1 {
			command, remains = strings.ToLower(inputText[0:index]), inputText[index+1:]
		} else {
			command = inputText
		}

		//mudlog.Info("World received mob input", "InputText", (inputText))

		handled, err = mobcommands.TryCommand(command, remains, mobInstanceId)
		if err != nil {
			mudlog.Warn("mob-TryCommand", "command", command, "remains", remains, "error", err.Error())
		}

	}

	if !handled {
		if len(command) > 0 {
			mob.Command(fmt.Sprintf(`emote looks a little confused (%s %s).`, command, remains))
		}
	}

}

func (w *World) UpdateStats() {
	s := web.GetStats()
	s.Reset()

	c := configs.GetNetworkConfig()

	for _, u := range users.GetAllActiveUsers() {
		info := u.GetOnlineInfo()
		s.OnlineUsers = append(s.OnlineUsers, info)
		switch info.ConnType {
		case "websocket":
			s.WebSocketConnections++
		case "ssh":
			s.SSHConnections++
		default:
			s.TelnetConnections++
		}
	}

	sort.Slice(s.OnlineUsers, func(i, j int) bool {
		if s.OnlineUsers[i].Role == users.RoleAdmin {
			return true
		}
		if s.OnlineUsers[j].Role == users.RoleAdmin {
			return false
		}
		return s.OnlineUsers[i].OnlineTime > s.OnlineUsers[j].OnlineTime
	})

	for _, t := range c.TelnetPort {
		p, _ := strconv.Atoi(t)
		if p > 0 {
			s.TelnetPorts = append(s.TelnetPorts, p)
		}
	}

	s.WebSocketPort = int(c.HttpPort)
	s.SSHPort = int(c.SSHPort)

	web.UpdateStats(s)
}

// Force disconnect a user (Makes them linkdead)
func (w *World) Kick(userId int, reason string) {

	user := users.GetByUserId(userId)
	if user == nil {
		return
	}

	users.SetLinkDeadUser(userId)
	user.EventLog.Add(`conn`, fmt.Sprintf(`Kicked (%s)`, reason))

	connections.Kick(user.ConnectionId(), reason)
}

// Should only handle sending messages out to users
func (w *World) EventLoop() {

	w.eventRequeue = w.eventRequeue[:0]

	events.ProcessEvents()

	for _, e := range w.eventRequeue {
		events.AddToQueue(e)
	}

	clear(w.userInputEventTracker)
	clear(w.mobInputEventTracker)
}
