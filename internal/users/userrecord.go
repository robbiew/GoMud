package users

import (
	"crypto/md5"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/GoMudEngine/GoMud/internal/audio"
	"github.com/GoMudEngine/GoMud/internal/characters"
	"github.com/GoMudEngine/GoMud/internal/configs"
	"github.com/GoMudEngine/GoMud/internal/connections"
	"github.com/GoMudEngine/GoMud/internal/events"
	"github.com/GoMudEngine/GoMud/internal/prompt"
	"github.com/GoMudEngine/GoMud/internal/skills"
	"github.com/GoMudEngine/GoMud/internal/stats"
	"github.com/GoMudEngine/GoMud/internal/util"
	"golang.org/x/crypto/bcrypt"
	//
)

var (
	// immutable roles
	RoleGuest string = "guest"
	RoleUser  string = "user"
	RoleMod   string = "mod"
	RoleAdmin string = "admin"
)

type UserRecord struct {
	BBS            *BBSIdentity          `yaml:"bbs,omitempty"`
	UserId         int                   `yaml:"userid"`
	Role           string                `yaml:"role"`                  // user, mod, or admin
	Permissions    []string              `yaml:"permissions,omitempty"` // Discrete permissions for mod role
	Username       string                `yaml:"username"`
	Password       string                `yaml:"password"`
	Joined         time.Time             `yaml:"joined"`
	Macros         map[string]string     `yaml:"macros,omitempty"`  // Up to 10 macros, just string commands.
	Aliases        map[string]string     `yaml:"aliases,omitempty"` // string=>string remapping of commands
	Character      *characters.Character `yaml:"character,omitempty"`
	ConfigOptions  map[string]any        `yaml:"configoptions,omitempty"`
	Muted          bool                  `yaml:"muted,omitempty"`        // Cannot SEND custom communications to anyone but admin/mods
	ScreenReader   bool                  `yaml:"screenreader,omitempty"` // Are they using a screen reader? (We should remove excess symbols)
	IsAI           bool                  `yaml:"isai,omitempty"`         // Flagged as an AI/test account
	EmailAddress   string                `yaml:"emailaddress,omitempty"` // Email address (if provided)
	TipsComplete   map[string]bool       `yaml:"tipscomplete,omitempty"` // Tips the user has followed/completed so they can be quiet
	EventLog       UserLog               `yaml:"-"`                      // Do not retain in user file (for now)
	LastMusic      string                `yaml:"-"`                      // Keeps track of the last music that was played
	connectionId   uint64
	unsentText     string
	suggestText    string
	connectionTime time.Time
	lastInputRound uint64
	tempDataStore  map[string]any
	activePrompt   *prompt.Prompt
	isLinkDead     bool // are they a link-dead connection currently?
	inputBlocked   bool // Whether input is currently intentionally turned off (for a certain category of commands)
}

func NewUserRecord(userId int, connectionId uint64) *UserRecord {

	c := configs.GetGamePlayConfig()

	u := &UserRecord{
		connectionId:   connectionId,
		UserId:         userId,
		Role:           RoleUser,
		Username:       "",
		Password:       "",
		Macros:         make(map[string]string),
		Character:      characters.New(),
		ConfigOptions:  map[string]any{},
		Joined:         time.Now(),
		connectionTime: time.Now(),
		tempDataStore:  make(map[string]any),
		EventLog:       UserLog{},
	}

	if c.Death.PermaDeath {
		u.Character.ExtraLives = int(c.LivesStart)
	}

	return u
}

func (u *UserRecord) ClientSettings() connections.ClientSettings {
	return connections.GetClientSettings(u.connectionId)
}

func (u *UserRecord) HasPlaintextPassword() bool {
	if u.BBS != nil {
		return false
	}
	// bcrypt check
	if strings.HasPrefix(u.Password, "$2a$") || strings.HasPrefix(u.Password, "$2b$") {
		return false
	}
	// SHA256 check
	if len(u.Password) == 64 {
		return false
	}
	return true
}

func (u *UserRecord) PasswordMatches(input string) bool {
	if u.BBS != nil {
		return false
	}

	// Try bcrypt first (new format).
	if err := bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(input)); err == nil {
		return true
	}

	// Migration: check if stored password is old unsalted SHA256 format.
	if u.Password == util.Hash(input) {
		// Re-hash with bcrypt so subsequent logins use the secure path.
		if hash, err := bcrypt.GenerateFromPassword([]byte(input), bcrypt.DefaultCost); err == nil {
			u.Password = string(hash)
		}
		return true
	}

	// Special case for new setups before things get reset
	if u.HasPlaintextPassword() && u.Password == input {
		return true
	}

	// No plaintext fallback. No hash-of-hash bypass.
	return false
}

func (u *UserRecord) AddCommandAlias(input string, output string) (addedAlias string, deletedAlias string) {

	if u.Aliases == nil {
		u.Aliases = map[string]string{}
	}

	input = strings.ToLower(strings.TrimSpace(input))
	if input == `alias` {
		return
	}

	if output == `` {
		delete(u.Aliases, input)
		return ``, input
	}

	if len(input) >= 64 {
		input = input[0:64]
	}

	if len(output) >= 64 {
		output = output[0:64]
	}

	u.Aliases[input] = strings.TrimSpace(output)

	return input, ``
}

func (u *UserRecord) TryCommandAlias(input string) string {

	if u.Aliases == nil {
		u.Aliases = map[string]string{}
		return input
	}

	if alias, ok := u.Aliases[strings.ToLower(input)]; ok {
		return alias
	}

	return input
}

func (u *UserRecord) ShorthandId() string {
	return fmt.Sprintf(`@%d`, u.UserId)
}

func (u *UserRecord) SetLastInputRound(rdNum uint64) {
	u.lastInputRound = rdNum
}

func (u *UserRecord) GetLastInputRound() uint64 {
	return u.lastInputRound
}

func (u *UserRecord) HasShop() bool {
	return len(u.Character.Shop) > 0
}

// Grants experience to the user and notifies them
// Additionally accepts `source` as a short identifier of the XP source
// Example source: "combat", "quest progress", "trash cleanup", "exploration"
func (u *UserRecord) GrantXP(amt int, source string) {

	grantXP, xpScale := u.Character.GrantXP(amt)

	if xpScale != 100 {
		u.SendText(fmt.Sprintf(`You gained <ansi fg="yellow-bold">%d experience points</ansi> <ansi fg="yellow">(%d%% scale)</ansi>! <ansi fg="7">(%s)</ansi>`, grantXP, xpScale, source))

		u.EventLog.Add(`xp`, fmt.Sprintf(`Gained <ansi fg="yellow-bold">%d experience points</ansi> <ansi fg="yellow">(%d%% scale)</ansi>! <ansi fg="7">(%s)</ansi>`, grantXP, xpScale, source))

	} else {

		u.SendText(fmt.Sprintf(`You gained <ansi fg="yellow-bold">%d experience points</ansi>! <ansi fg="7">(%s)</ansi>`, grantXP, source))

		u.EventLog.Add(`xp`, fmt.Sprintf(`Gained <ansi fg="yellow-bold">%d experience points</ansi>! <ansi fg="7">(%s)</ansi>`, grantXP, source))
	}

	events.AddToQueue(events.GainExperience{
		UserId:     u.UserId,
		Experience: grantXP,
		Scale:      xpScale,
	})

	tpBefore := u.Character.TrainingPoints
	spBefore := u.Character.StatPoints

	if newLevel, statsDelta := u.Character.LevelUp(); newLevel {

		c := configs.GetGamePlayConfig()

		livesBefore := u.Character.ExtraLives

		levelUpEvent := events.LevelUp{
			UserId:         u.UserId,
			RoomId:         u.Character.RoomId,
			Username:       u.Username,
			CharacterName:  u.Character.Name,
			LevelsGained:   0,
			NewLevel:       u.Character.Level,
			StatsDelta:     stats.Statistics{},
			TrainingPoints: 0,
			StatPoints:     0,
			LivesGained:    0,
		}

		for newLevel {

			if c.Death.PermaDeath && c.LivesOnLevelUp > 0 {
				u.Character.ExtraLives += int(c.LivesOnLevelUp)
			}

			u.EventLog.Add(`xp`, fmt.Sprintf(`<ansi fg="username">%s</ansi> is now <ansi fg="magenta-bold">level %d</ansi>!`, u.Character.Name, u.Character.Level))

			levelUpEvent.LevelsGained += 1
			levelUpEvent.StatsDelta.Strength.Value += statsDelta.Strength.Value
			levelUpEvent.StatsDelta.Speed.Value += statsDelta.Speed.Value
			levelUpEvent.StatsDelta.Smarts.Value += statsDelta.Smarts.Value
			levelUpEvent.StatsDelta.Vitality.Value += statsDelta.Vitality.Value
			levelUpEvent.StatsDelta.Mysticism.Value += statsDelta.Mysticism.Value
			levelUpEvent.StatsDelta.Perception.Value += statsDelta.Perception.Value

			// Snapshot before the next LevelUp call so we can measure what was actually granted.
			tpBefore = u.Character.TrainingPoints
			spBefore = u.Character.StatPoints

			newLevel, statsDelta = u.Character.LevelUp()

			levelUpEvent.TrainingPoints += u.Character.TrainingPoints - tpBefore
			levelUpEvent.StatPoints += u.Character.StatPoints - spBefore
		}

		if u.Character.ExtraLives > int(c.LivesMax) {
			u.Character.ExtraLives = int(c.LivesMax)
		}

		levelUpEvent.LivesGained = u.Character.ExtraLives - livesBefore
		levelUpEvent.NewLevel = u.Character.Level

		events.AddToQueue(levelUpEvent)

		SaveUser(*u)
	}
}

func (u *UserRecord) DidTip(tipName string, completed ...bool) bool {

	if u.TipsComplete == nil {
		u.TipsComplete = map[string]bool{}
	}

	if len(completed) > 0 {
		if completed[0] {
			u.TipsComplete[tipName] = completed[0]
		} else {
			delete(u.TipsComplete, tipName)
		}
		return completed[0]
	}

	return u.TipsComplete[tipName]
}

func (u *UserRecord) PlayMusic(musicFileOrId string) {

	v := 100
	if soundConfig := audio.GetFile(musicFileOrId); soundConfig.FilePath != `` {
		musicFileOrId = soundConfig.FilePath
		if soundConfig.Volume > 0 && soundConfig.Volume <= 100 {
			v = soundConfig.Volume
		}
	}

	events.AddToQueue(events.MSP{
		UserId:    u.UserId,
		SoundType: `MUSIC`,
		SoundFile: musicFileOrId,
		Volume:    v,
	})

}

func (u *UserRecord) PlaySound(soundId string, category string) {

	v := 100
	if soundConfig := audio.GetFile(soundId); soundConfig.FilePath != `` {
		soundId = soundConfig.FilePath
		if soundConfig.Volume > 0 && soundConfig.Volume <= 100 {
			v = soundConfig.Volume
		}
	}

	events.AddToQueue(events.MSP{
		UserId:    u.UserId,
		SoundType: `SOUND`,
		SoundFile: soundId,
		Volume:    v,
		Category:  category,
	})

}

func (u *UserRecord) Command(inputTxt string, waitSeconds ...float64) {

	readyTurn := util.GetTurnCount()
	if len(waitSeconds) > 0 {
		readyTurn += uint64(float64(configs.GetTimingConfig().SecondsToTurns(1)) * waitSeconds[0])
	}

	events.AddToQueue(events.Input{
		UserId:    u.UserId,
		InputText: inputTxt,
		ReadyTurn: readyTurn,
	})

}

func (u *UserRecord) BlockInput() {
	u.inputBlocked = true
}

func (u *UserRecord) UnblockInput() {
	u.inputBlocked = false
}

func (u *UserRecord) InputBlocked() bool {
	return u.inputBlocked
}

func (u *UserRecord) CommandFlagged(inputTxt string, flagData events.EventFlag, waitSeconds ...float64) {

	readyTurn := util.GetTurnCount()
	if len(waitSeconds) > 0 {
		readyTurn += uint64(float64(configs.GetTimingConfig().SecondsToTurns(1)) * waitSeconds[0])
	}

	if flagData&events.CmdBlockInput == events.CmdBlockInput {
		u.BlockInput()
	}

	events.AddToQueue(events.Input{
		UserId:    u.UserId,
		InputText: inputTxt,
		ReadyTurn: readyTurn,
		Flags:     flagData,
	})

}

func (u *UserRecord) AddBuff(buffId int, source string) {

	events.AddToQueue(events.Buff{
		UserId: u.UserId,
		BuffId: buffId,
		Source: source,
	})

}

func (u *UserRecord) SendText(txt string) {

	events.AddToQueue(events.Message{
		UserId: u.UserId,
		Text:   txt + "\n",
	})

}

func (u *UserRecord) SendWebClientCommand(txt string) {

	events.AddToQueue(events.WebClientCommand{
		ConnectionId: u.connectionId,
		Text:         txt,
	})

}

func (u *UserRecord) SetTempData(key string, value any) {

	if u.tempDataStore == nil {
		u.tempDataStore = make(map[string]any)
	}

	if value == nil {
		delete(u.tempDataStore, key)
		return
	}
	u.tempDataStore[key] = value
}

func (u *UserRecord) GetTempData(key string) any {

	if u.tempDataStore == nil {
		u.tempDataStore = make(map[string]any)
	}

	if value, ok := u.tempDataStore[key]; ok {
		return value
	}
	return nil
}

// HasRolePermission returns true when the user has the given permission.
// Admins always pass. Mods are checked against their Permissions slice using
// prefix-match semantics: a granted key of "room" also satisfies "room.edit",
// "room.edit.exits", etc. The optional simpleMatch parameter additionally
// allows the inverse direction (requested "room" matches granted "room.edit").
func (u *UserRecord) HasRolePermission(permissionId string, simpleMatch ...bool) bool {
	if u.Role == RoleAdmin {
		return true
	}

	if u.Role != RoleMod {
		return false
	}

	return u.hasPermission(permissionId, len(simpleMatch) > 0 && simpleMatch[0])
}

// HasPermission is a convenience wrapper around HasRolePermission without the
// simpleMatch parameter — used by web middleware where prefix-only matching is
// always desired.
func (u *UserRecord) HasPermission(permissionId string) bool {
	return u.HasRolePermission(permissionId)
}

// hasPermission is the internal implementation. It checks u.Permissions for a
// match using prefix semantics:
//   - An exact match always passes.
//   - A granted key that is a prefix of the requested key passes
//     (e.g. granted "room" satisfies requested "room.edit.exits").
//   - When simpleMatch is true, the inverse also passes: a granted key that
//     starts with the requested key (e.g. requested "room" matches granted
//     "room.edit").
func (u *UserRecord) hasPermission(permissionId string, simpleMatch bool) bool {
	permissionIdLen := len(permissionId)
	for _, granted := range u.Permissions {
		if permissionId == granted {
			return true
		}
		grantedLen := len(granted)

		if simpleMatch {
			// requested key is shorter than granted — check if granted starts with requested
			if permissionIdLen < grantedLen {
				if granted[:permissionIdLen] == permissionId {
					return true
				}
			}
		}

		// granted key is a prefix of the requested key
		if permissionIdLen >= grantedLen && permissionId[:grantedLen] == granted {
			// ensure it's a proper prefix boundary (dot or exact)
			if permissionIdLen == grantedLen || permissionId[grantedLen] == '.' {
				return true
			}
		}
	}
	return false
}

func (u *UserRecord) SetConfigOption(key string, value any) {
	if u.ConfigOptions == nil {
		u.ConfigOptions = make(map[string]any)
	}
	if value == nil {
		delete(u.ConfigOptions, key)
		return
	}
	u.ConfigOptions[key] = value
}

func (u *UserRecord) GetConfigOption(key string) any {
	if u.ConfigOptions == nil {
		u.ConfigOptions = make(map[string]any)
	}
	if value, ok := u.ConfigOptions[key]; ok {
		return value
	}
	return nil
}

func (u *UserRecord) GetConnectTime() time.Time {
	return u.connectionTime
}

func (u *UserRecord) RoundTick() {

}

// The purpose of SetUnsentText(), GetUnsentText() is to
// Capture what the user is typing so that when we redraw the
// "prompt" or status bar, we can redraw what they were in the middle
// of typing.
// I don't like the idea of capturing it every time they hit a key though
// There is probably a better way.
func (u *UserRecord) SetUnsentText(t string, suggest string) {

	u.unsentText = t
	u.suggestText = suggest
}

func (u *UserRecord) GetUnsentText() (unsent string, suggestion string) {

	return u.unsentText, u.suggestText
}

// Replace a characters information with another.
func (u *UserRecord) ReplaceCharacter(replacement *characters.Character) {
	u.Character = replacement
}

func (u *UserRecord) SetUsername(un string) error {

	if err := ValidateName(un); err != nil {
		return err
	}

	u.Username = un

	// If no character name, just set it to username for now.
	if u.Character.Name == "" {
		u.Character.Name = u.TempName()
	}

	return nil
}

func (u *UserRecord) TempName() string {
	hasher := md5.New()
	hasher.Write([]byte([]byte(u.Username)))
	hashInBytes := hasher.Sum(nil)
	number := new(big.Int).SetBytes(hashInBytes)

	mod := new(big.Int)
	mod.SetInt64(9087919)

	return fmt.Sprintf("nameless-%d", number.Mod(number, mod).Int64())
}

func (u *UserRecord) SetCharacterName(cn string) error {

	if err := ValidateName(cn); err != nil {
		return err
	}

	GetCharacterIndex().Add(cn, u.UserId)
	u.Character.Name = cn

	return nil
}

func (u *UserRecord) SetPassword(pw string) error {

	validation := configs.GetValidationConfig()

	if len(pw) < int(validation.PasswordSizeMin) || len(pw) > int(validation.PasswordSizeMax) {
		return fmt.Errorf("password must be between %d and %d characters long", validation.PasswordSizeMin, validation.PasswordSizeMax)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}
	u.Password = string(hash)
	return nil
}

func (u *UserRecord) ConnectionId() uint64 {
	return u.connectionId
}

// Prompt related functionality
func (u *UserRecord) StartPrompt(command string, rest string) (*prompt.Prompt, bool) {

	if u.activePrompt != nil {
		// If it's the same prompt, return the existing one
		if u.activePrompt.Command == command && u.activePrompt.Rest == rest {
			return u.activePrompt, false
		}
	}

	// If no prompt found or it seems like a new prompt, create a new one and replace the old
	u.activePrompt = prompt.New(command, rest)

	return u.activePrompt, true
}

func (u *UserRecord) GetPrompt() *prompt.Prompt {

	return u.activePrompt
}

func (u *UserRecord) ClearPrompt() {
	u.activePrompt = nil
}

func (u *UserRecord) GetOnlineInfo() OnlineInfo {
	c := configs.GetTimingConfig()
	afkRounds := uint64(c.SecondsToRounds(int(configs.GetNetworkConfig().AfkSeconds)))
	roundNow := util.GetRoundCount()

	connTime := u.GetConnectTime()

	oTime := time.Since(connTime)

	h := int(math.Floor(oTime.Hours()))
	m := int(math.Floor(oTime.Minutes())) - (h * 60)
	s := int(math.Floor(oTime.Seconds())) - (h * 60 * 60) - (m * 60)

	timeStr := ``
	if h > 0 {
		timeStr = fmt.Sprintf(`%dh%dm`, h, m)
	} else if m > 0 {
		timeStr = fmt.Sprintf(`%dm`, m)
	} else {
		timeStr = fmt.Sprintf(`%ds`, s)
	}

	isAfk := false
	if afkRounds > 0 && roundNow-u.GetLastInputRound() >= afkRounds {
		isAfk = true
	}

	connType := `telnet`
	if cd := connections.Get(u.connectionId); cd != nil {
		if cd.IsWebSocket() {
			connType = `websocket`
		} else if cd.IsSSH() {
			connType = `ssh`
		} else if cd.ConnType() == connections.ConnBBS {
			connType = `bbs`
		}
	}

	return OnlineInfo{
		u.Username,
		u.Character.Name,
		u.Character.Level,
		u.Character.AlignmentName(),
		skills.GetProfession(u.Character.GetAllSkillRanks()),
		int64(oTime.Seconds()),
		timeStr,
		isAfk,
		u.Role,
		connType,
	}
}

func (u *UserRecord) WimpyCheck() {
	if currentWimpy := u.GetConfigOption(`wimpy`); currentWimpy != nil {
		healthPct := int(math.Floor(float64(u.Character.Health) / float64(u.Character.HealthMax.Value) * 100))
		if healthPct < currentWimpy.(int) {
			u.Command(`flee`, -1)
		}
	}
}
