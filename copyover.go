package main

import (
	"fmt"
	"os"

	"github.com/GoMudEngine/GoMud/internal/configs"
	"github.com/GoMudEngine/GoMud/internal/copyover"
	"github.com/GoMudEngine/GoMud/internal/mudlog"
	"github.com/GoMudEngine/GoMud/internal/plugins"
	"github.com/GoMudEngine/GoMud/internal/rooms"
	"github.com/GoMudEngine/GoMud/internal/users"
)

func triggerCopyover() error {
	if shuttingDown.Load() {
		return fmt.Errorf("server is shutting down")
	}
	select {
	case worldManager.bbsTasks <- func() {
		if err := performCopyover(); err != nil {
			mudlog.Error("copyover failed", "error", err)
		}
	}:
		return nil
	default:
		return fmt.Errorf("world request queue is full")
	}
}

func performCopyover() (result error) {
	if shuttingDown.Load() {
		return fmt.Errorf("server is shutting down")
	}
	stopped := false
	defer func() {
		if result != nil && !shuttingDown.Load() {
			serverAlive.Store(true)
			if stopped {
				if err := startBBSListener(configs.GetConfig().Network.BBS); err != nil {
					mudlog.Error("restart BBS after failed copyover", "error", err)
				}
			}
		}
	}()
	binaryPath, err := os.Executable()
	if err != nil {
		return err
	}

	serverAlive.Store(false)
	if listener := bbsListener.Load(); listener != nil {
		stopped = true
		listener.StopAccepting()
	}
	worldManager.disconnectAllBBS()

	rooms.SaveAllRooms()
	users.SaveAllUsers()
	plugins.Save()

	return copyover.Execute(binaryPath, os.Args[1:])
}
