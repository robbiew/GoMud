package users

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoMudEngine/GoMud/internal/configs"
	"github.com/GoMudEngine/GoMud/internal/connections"
	"github.com/GoMudEngine/GoMud/internal/mobs"
	"github.com/GoMudEngine/GoMud/internal/mudlog"
	"github.com/GoMudEngine/GoMud/internal/util"

	//
	"gopkg.in/yaml.v2"
)

var (
	userManager   *ActiveUsers = newUserManager()
	userManagerMu sync.RWMutex
)

type ActiveUsers struct {
	Users               map[int]*UserRecord                 // userId to UserRecord
	Usernames           map[string]int                      // username to userId
	Connections         map[connections.ConnectionId]int    // connectionId to userId
	UserConnections     map[int]connections.ConnectionId    // userId to connectionId
	LinkDeadConnections map[connections.ConnectionId]uint64 // connectionId to turn they became link-dead
}

func newUserManager() *ActiveUsers {
	return &ActiveUsers{
		Users:               make(map[int]*UserRecord),
		Usernames:           make(map[string]int),
		Connections:         make(map[connections.ConnectionId]int),
		UserConnections:     make(map[int]connections.ConnectionId),
		LinkDeadConnections: make(map[connections.ConnectionId]uint64),
	}
}

func RemoveLinkDeadUser(userId int) {
	userManagerMu.Lock()
	defer userManagerMu.Unlock()

	if u := userManager.Users[userId]; u != nil {
		u.Character.SetAdjective(`zombie`, false)
		u.isLinkDead = false
	}
	if connId, ok := userManager.UserConnections[userId]; ok {
		delete(userManager.LinkDeadConnections, connId)
	}
}

func IsLinkDeadConnection(connectionId connections.ConnectionId) bool {
	userManagerMu.RLock()
	defer userManagerMu.RUnlock()

	_, ok := userManager.LinkDeadConnections[connectionId]
	return ok
}

func isLinkDeadConnectionLocked(connectionId connections.ConnectionId) bool {
	_, ok := userManager.LinkDeadConnections[connectionId]
	return ok
}

func RemoveLinkDeadConnection(connectionId connections.ConnectionId) {
	userManagerMu.Lock()
	defer userManagerMu.Unlock()

	delete(userManager.LinkDeadConnections, connectionId)
}

func removeLinkDeadConnectionLocked(connectionId connections.ConnectionId) {
	delete(userManager.LinkDeadConnections, connectionId)
}

// Returns a slice of userId's
// These userId's are link-dead players that have reached expiration
func GetExpiredLinkDeadUsers(expirationTurn uint64) []int {
	userManagerMu.RLock()
	defer userManagerMu.RUnlock()

	expiredUsers := make([]int, 0)

	for connectionId, linkDeadTurn := range userManager.LinkDeadConnections {
		if linkDeadTurn < expirationTurn {
			expiredUsers = append(expiredUsers, userManager.Connections[connectionId])
		}
	}

	return expiredUsers
}

func GetConnectionId(userId int) connections.ConnectionId {
	userManagerMu.RLock()
	defer userManagerMu.RUnlock()

	if user, ok := userManager.Users[userId]; ok {
		return user.connectionId
	}
	return 0
}

func GetConnectionIds(userIds []int) []connections.ConnectionId {
	userManagerMu.RLock()
	defer userManagerMu.RUnlock()

	connectionIds := make([]connections.ConnectionId, 0, len(userIds))
	for _, userId := range userIds {
		if user, ok := userManager.Users[userId]; ok {
			connectionIds = append(connectionIds, user.connectionId)
		}
	}

	return connectionIds
}

func GetAllActiveUsers() []*UserRecord {
	userManagerMu.RLock()
	defer userManagerMu.RUnlock()

	ret := []*UserRecord{}
	for _, userPtr := range userManager.Users {
		if !userPtr.isLinkDead {
			ret = append(ret, userPtr)
		}
	}

	return ret
}

func GetOnlineUserIds() []int {
	userManagerMu.RLock()
	defer userManagerMu.RUnlock()

	onlineList := make([]int, 0, len(userManager.Users))
	for _, user := range userManager.Users {
		onlineList = append(onlineList, user.UserId)
	}
	return onlineList
}

func GetByCharacterName(name string) *UserRecord {
	userManagerMu.RLock()
	defer userManagerMu.RUnlock()

	var closeMatch *UserRecord = nil

	name = strings.ToLower(name)
	for _, user := range userManager.Users {
		testName := strings.ToLower(user.Character.Name)
		if testName == name {
			return user
		}
		if strings.HasPrefix(testName, name) {
			closeMatch = user
		}
	}

	return closeMatch
}

func GetByUserId(userId int) *UserRecord {
	userManagerMu.RLock()
	defer userManagerMu.RUnlock()

	if user, ok := userManager.Users[userId]; ok {
		return user
	}

	return nil
}

func GetByConnectionId(connectionId connections.ConnectionId) *UserRecord {
	userManagerMu.RLock()
	defer userManagerMu.RUnlock()

	if userId, ok := userManager.Connections[connectionId]; ok {
		return userManager.Users[userId]
	}

	return nil
}

// CopyoverReconnectUser takes over an existing session for a user who is
// reconnecting after a copyover. The caller must have already validated a
// one-time reconnect token that proves the identity of the connecting client.
// Unlike LoginUser, this always succeeds for users that are currently tracked
// (zombie or not), because the old connection is a stale copyover socket that
// is no longer connected.
func CopyoverReconnectUser(user *UserRecord, connectionId connections.ConnectionId) (*UserRecord, string, error) {

	mudlog.Info("CopyoverReconnectUser()", "username", user.Username, "connectionId", connectionId)

	user.Character.SetAdjective(`zombie`, false)

	userManagerMu.Lock()
	if userId, ok := userManager.Usernames[user.Username]; ok {
		if existingUser, ok := userManager.Users[userId]; ok {
			user = existingUser
		}

		if oldConnId, ok := userManager.UserConnections[userId]; ok {
			delete(userManager.LinkDeadConnections, oldConnId)
			delete(userManager.Connections, oldConnId)
		}
	}

	user.connectionId = connectionId
	user.Character.SetAdjective(`zombie`, false)
	user.connectionTime = time.Now()
	user.SetLastInputRound(util.GetRoundCount())

	userManager.Users[user.UserId] = user
	userManager.Usernames[user.Username] = user.UserId
	userManager.Connections[user.connectionId] = user.UserId
	userManager.UserConnections[user.UserId] = user.connectionId
	userManagerMu.Unlock()

	for _, mobInstId := range user.Character.GetCharmIds() {
		if !mobs.MobInstanceExists(mobInstId) {
			user.Character.TrackCharmed(mobInstId, false)
		}
	}

	user.EventLog.Add(`conn`, `Reconnected`)

	return user, "Reconnecting...", nil
}

// First time creating a user.
func LoginUser(user *UserRecord, connectionId connections.ConnectionId) (*UserRecord, string, error) {

	mudlog.Info("LoginUser()", "username", user.Username, "connectionId", connectionId)

	user.Character.SetAdjective(`zombie`, false)

	userManagerMu.Lock()

	// If they're already logged in
	if userId, ok := userManager.Usernames[user.Username]; ok {

		// Do they have a connection tracked?
		if otherConnId, ok := userManager.UserConnections[userId]; ok {

			// Is it a link-dead connection? If so, lets make this new connection the owner
			if isLinkDeadConnectionLocked(otherConnId) {

				mudlog.Info("LoginUser()", "LinkDead", true)

				if linkDeadUser, ok := userManager.Users[user.UserId]; ok {
					user = linkDeadUser
				}

				removeLinkDeadConnectionLocked(otherConnId)

				user.connectionId = connectionId

				userManager.Users[user.UserId] = user
				userManager.Usernames[user.Username] = user.UserId
				userManager.Connections[user.connectionId] = user.UserId
				userManager.UserConnections[user.UserId] = user.connectionId

				userManagerMu.Unlock()

				for _, mobInstId := range user.Character.GetCharmIds() {
					if !mobs.MobInstanceExists(mobInstId) {
						user.Character.TrackCharmed(mobInstId, false)
					}
				}

				// Set their input round to current to track idle time fresh
				user.SetLastInputRound(util.GetRoundCount())

				user.EventLog.Add(`conn`, `Reconnected`)

				return user, "Reconnecting...", nil
			}

		}

		userManagerMu.Unlock()
		// Otherwise, someone else is logged in, can't double-login!
		return nil, "That user is already logged in.", errors.New("user is already logged in")
	}

	mudlog.Info("LoginUser()", "LinkDead", false)

	// Set their input round to current to track idle time fresh
	user.SetLastInputRound(util.GetRoundCount())

	user.connectionId = connectionId

	userManager.Users[user.UserId] = user
	userManager.Usernames[user.Username] = user.UserId
	userManager.Connections[user.connectionId] = user.UserId
	userManager.UserConnections[user.UserId] = user.connectionId

	userManagerMu.Unlock()

	mudlog.Info("LOGIN", "userId", user.UserId)

	user.EventLog.Add(`conn`, `Connected`)

	for _, mobInstId := range user.Character.GetCharmIds() {
		if !mobs.MobInstanceExists(mobInstId) {
			user.Character.TrackCharmed(mobInstId, false)
		}
	}

	return user, "", nil
}

func SetLinkDeadUser(userId int) {
	userManagerMu.Lock()
	u, ok := userManager.Users[userId]
	userManagerMu.Unlock()

	if !ok {
		return
	}

	u.Character.RemoveBuff(0)
	u.Character.SetAdjective(`zombie`, true)

	// Special case for `newbieguide` module exported function check
	// If module was loaded, this function should exist.
	if fn, ok := GetExportedFunction(`PlayerGuideMobId`); ok {
		guideMobId := 0
		if guideMobIdFn, ok := fn.(func() int); ok {
			guideMobId = guideMobIdFn()
		}

		// Prevent guide mob dupes
		for _, miid := range u.Character.CharmedMobs {
			if m := mobs.GetInstance(miid); m != nil {
				if m.MobId == mobs.MobId(guideMobId) && m.Character.Charmed != nil {
					m.Character.Charmed.RoundsRemaining = 0
				}
			}
		}
	}

	userManagerMu.Lock()
	if _, ok := userManager.LinkDeadConnections[u.connectionId]; !ok {
		u.isLinkDead = true
		userManager.LinkDeadConnections[u.connectionId] = util.GetTurnCount()
	}
	userManagerMu.Unlock()
}

func SaveAllUsers(isAutoSave ...bool) {
	userManagerMu.RLock()
	users := make([]*UserRecord, 0, len(userManager.Users))
	for _, u := range userManager.Users {
		users = append(users, u)
	}
	userManagerMu.RUnlock()

	for _, u := range users {
		if err := SaveUser(*u, isAutoSave...); err != nil {
			mudlog.Error("SaveAllUsers()", "error", err.Error())
		}
	}

}

func LogOutUserByConnectionId(connectionId connections.ConnectionId) error {

	u := GetByConnectionId(connectionId)

	userManagerMu.Lock()
	defer userManagerMu.Unlock()

	if _, ok := userManager.Connections[connectionId]; ok {

		// Make sure the user data is saved to a file.
		if u != nil {
			u.Character.Validate()
			if err := SaveUser(*u); err != nil {
				mudlog.Error("Logout save failed", "userId", u.UserId, "error", err)
			}

			delete(userManager.Users, u.UserId)
			delete(userManager.Usernames, u.Username)
			delete(userManager.Connections, u.connectionId)
			delete(userManager.UserConnections, u.UserId)
		} else {
			// Connection exists but user record is missing - clean up the connection entry
			delete(userManager.Connections, connectionId)
		}

		return nil
	}
	return errors.New("user not found for connection")
}

// First time creating a user.
var createUserMu sync.Mutex

func CreateUser(u *UserRecord) error {
	createUserMu.Lock()
	defer createUserMu.Unlock()

	if err := ValidateName(u.Username); err != nil {
		return errors.New("that username is not allowed: " + err.Error())
	}

	u.UserId = GetUniqueUserId()
	u.Role = RoleUser

	idx := GetUserIndex()
	if err := SaveUser(*u); err != nil {
		return err
	}
	if err := idx.AddUser(u.UserId, u.Username); err != nil {
		// The YAML record is durable; rebuild the derived index for a safe retry.
		if rebuildErr := idx.Rebuild(); rebuildErr != nil {
			return fmt.Errorf("index user: %v; rebuild: %w", err, rebuildErr)
		}
		return err
	}

	userManagerMu.Lock()
	userManager.Users[u.UserId] = u
	userManager.Usernames[u.Username] = u.UserId
	userManager.Connections[u.connectionId] = u.UserId
	userManager.UserConnections[u.UserId] = u.connectionId
	userManagerMu.Unlock()

	return nil
}

func LoadUserById(userId int) (*UserRecord, error) {

	idx := GetUserIndex()
	username, found := idx.FindByUserId(userId)
	if !found {
		return nil, errors.New("user doesn't exist")
	}

	return LoadUser(username)
}

func LoadUser(username string, skipValidation ...bool) (*UserRecord, error) {

	idx := GetUserIndex()
	userId, found := idx.FindByUsername(username)

	if !found {
		return nil, errors.New("user doesn't exist")
	}

	userFilePath := util.FilePath(string(configs.GetFilePathsConfig().DataFiles), `/`, `users`, `/`, strconv.Itoa(userId)+`.yaml`)

	userFileTxt, err := util.ReadFile(userFilePath)
	if err != nil {
		return nil, err
	}

	loadedUser := &UserRecord{}
	if err := yaml.Unmarshal([]byte(userFileTxt), loadedUser); err != nil {
		mudlog.Error("LoadUser", "error", err.Error())
	}

	if len(skipValidation) == 0 || !skipValidation[0] {
		if err := loadedUser.Character.Validate(true); err == nil {
			SaveUser(*loadedUser)
		}
	}

	if loadedUser.Joined.IsZero() {
		loadedUser.Joined = time.Now()
	}

	// Set their connection time to now
	loadedUser.connectionTime = time.Now()

	return loadedUser, nil
}

// Loads all user recvords and runs against a function.
// Stops searching if false is returned.
func SearchOfflineUsers(searchFunc func(u *UserRecord) bool) {

	basePath := util.FilePath(string(configs.GetFilePathsConfig().DataFiles), `/`, `users`)

	filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {

		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		if len(path) > 10 && path[len(path)-10:] == `.alts.yaml` {
			return nil
		}

		var uRecord UserRecord

		fpathLower := path[len(path)-5:] // Only need to compare the last 5 characters
		if fpathLower == `.yaml` {

			bytes, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			err = yaml.Unmarshal(bytes, &uRecord)
			if err != nil {
				return err
			}

			// If this is an online user, skip it
			userManagerMu.RLock()
			_, isOnline := userManager.Usernames[uRecord.Username]
			userManagerMu.RUnlock()
			if isOnline {
				return nil
			}

			if res := searchFunc(&uRecord); !res {
				return errors.New(`done searching`)
			}
		}
		return nil
	})

}

func ValidateName(name string) error {

	validation := configs.GetValidationConfig()

	if len(name) < int(validation.NameSizeMin) || len(name) > int(validation.NameSizeMax) {
		return fmt.Errorf("name must be between %d and %d characters long", validation.NameSizeMin, validation.NameSizeMax)
	}

	if validation.NameRejectRegex != `` {
		if !regexp.MustCompile(validation.NameRejectRegex.String()).MatchString(name) {
			return errors.New(validation.NameRejectReason.String())
		}
	}

	if bannedPattern, ok := configs.GetConfig().IsBannedName(name); ok {
		return errors.New(`that username matched the prohibited name pattern: "` + bannedPattern + `"`)
	}

	for _, mobName := range mobs.GetAllMobNames() {
		if strings.EqualFold(mobName, name) {
			return errors.New("that username is in use")
		}
	}

	if Exists(name) {
		return errors.New("that username is in use")
	}

	return nil
}

func ValidatePassword(pw string) error {

	validation := configs.GetValidationConfig()

	if len(pw) < int(validation.PasswordSizeMin) || len(pw) > int(validation.PasswordSizeMax) {
		return fmt.Errorf("password must be between %d and %d characters long", validation.PasswordSizeMin, validation.PasswordSizeMax)
	}

	return nil
}

// CharacterNameSearch returns the userId and username for the given character
// name. It consults the in-memory CharacterIndex, which is populated at startup
// with every active character name and, when the alt-characters module is
// loaded, with all stored alt names as well.
func CharacterNameSearch(nameToFind string) (foundUserId int, foundUserName string) {
	userId, found := GetCharacterIndex().Find(nameToFind)
	if !found {
		return 0, ``
	}
	foundUserName, _ = GetUserIndex().FindByUserId(userId)
	return userId, foundUserName
}

// UpdateOnlineUser applies updated exported fields to the in-memory user
// record if the user is currently online. Connection state and other
// runtime-only fields are preserved from the live record.
func UpdateOnlineUser(updated UserRecord) {
	userManagerMu.Lock()
	defer userManagerMu.Unlock()

	u, ok := userManager.Users[updated.UserId]
	if !ok {
		return
	}

	updated.connectionId = u.connectionId
	updated.unsentText = u.unsentText
	updated.suggestText = u.suggestText
	updated.connectionTime = u.connectionTime
	updated.lastInputRound = u.lastInputRound
	updated.tempDataStore = u.tempDataStore
	updated.activePrompt = u.activePrompt
	updated.isLinkDead = u.isLinkDead
	updated.inputBlocked = u.inputBlocked
	updated.EventLog = u.EventLog
	updated.LastMusic = u.LastMusic

	*u = updated
}

func SaveUser(u UserRecord, isAutoSave ...bool) error {

	fileWritten := false
	tmpSaved := false
	tmpCopied := false
	completed := false

	defer func() {
		mudlog.Info("SaveUser()", "username", u.Username, "wrote-file", fileWritten, "tmp-file", tmpSaved, "tmp-copied", tmpCopied, "completed", completed)
	}()

	data, err := yaml.Marshal(&u)
	if err != nil {
		return err
	}

	carefulSave := configs.GetFilePathsConfig().CarefulSaveFiles

	path := util.FilePath(string(configs.GetFilePathsConfig().DataFiles), `/`, `users`, `/`, strconv.Itoa(u.UserId)+`.yaml`)

	saveFilePath := path
	if carefulSave { // careful save first saves a {filename}.new file
		saveFilePath += `.new`
	}

	err = util.WriteFile(saveFilePath, data, 0600)
	if err != nil {
		return err
	}
	fileWritten = true
	if carefulSave {
		tmpSaved = true
	}

	if carefulSave {
		//
		// Once the file is written, rename it to remove the .new suffix and overwrite the old file
		//
		if err := os.Rename(saveFilePath, path); err != nil {
			return err
		}
		tmpCopied = true
	}

	completed = true

	return nil
}

func GetUniqueUserId() int {

	// if highestUserId is zero, loop through users and get real highest.

	highestUserId := 0

	idx := GetUserIndex()
	if idx.Exists() {

		highestUserId = idx.GetHighestUserId()

	} else {

		// Check all user id's of offline users
		SearchOfflineUsers(func(u *UserRecord) bool {

			if u.UserId > highestUserId {
				highestUserId = u.UserId
			}

			return true
		})

		// Check all user id's of online users
		for _, u := range GetAllActiveUsers() {
			if u.UserId > highestUserId {
				highestUserId = u.UserId
			}
		}

	}

	// Increment the highestUserId before returning a new one
	highestUserId += 1

	return highestUserId
}

// DeleteUser permanently removes a user record, their YAML file, and all index
// entries. It refuses to delete a user that is currently online.
func DeleteUser(userId int) error {
	u, err := LoadUserById(userId)
	if err != nil {
		return fmt.Errorf("user %d not found: %w", userId, err)
	}

	userManagerMu.Lock()
	defer userManagerMu.Unlock()

	if _, online := userManager.Users[userId]; online {
		return fmt.Errorf("user %d is currently online and cannot be deleted", userId)
	}

	filePath := util.FilePath(string(configs.GetFilePathsConfig().DataFiles), `/`, `users`, `/`, strconv.Itoa(userId)+`.yaml`)
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove user file: %w", err)
	}

	idx := GetUserIndex()
	_ = idx.RemoveByUsername(u.Username)

	GetCharacterIndex().Remove(u.Character.Name)

	delete(userManager.Users, userId)
	delete(userManager.Usernames, u.Username)
	if connId, ok := userManager.UserConnections[userId]; ok {
		delete(userManager.Connections, connId)
		delete(userManager.LinkDeadConnections, connId)
	}
	delete(userManager.UserConnections, userId)

	return nil
}

func Exists(name string) bool {

	for _, u := range GetAllActiveUsers() {
		if strings.ToLower(u.Username) == strings.ToLower(name) {
			return true
		}
	}

	idx := GetUserIndex()
	_, found := idx.FindByUsername(name)

	return found
}

func FindUserId(username string) int {
	idx := GetUserIndex()
	userid, _ := idx.FindByUsername(username)
	return userid
}
