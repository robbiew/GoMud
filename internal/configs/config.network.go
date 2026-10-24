package configs

type Network struct {
	BBS                  BBSNetwork        `yaml:"BBS"`
	MaxTelnetConnections ConfigInt         `yaml:"MaxTelnetConnections"` // Maximum number of telnet connections to accept
	TelnetPort           ConfigSliceString `yaml:"TelnetPort"`           // One or more Ports used to accept telnet connections
	LocalPort            ConfigInt         `yaml:"LocalPort"`            // Port used for admin connections, localhost only
	HttpPort             ConfigInt         `yaml:"HttpPort"`             // Port used for web requests
	HttpsPort            ConfigInt         `yaml:"HttpsPort"`            // Port used for web https requests
	HttpsRedirect        ConfigBool        `yaml:"HttpsRedirect"`        // If true, http traffic will be redirected to https
	SSHPort              ConfigInt         `yaml:"SSHPort"`              // Port used for SSH connections (0 to disable)
	MaxSSHConnections    ConfigInt         `yaml:"MaxSSHConnections"`    // Maximum number of SSH connections to accept
	AI                   AINetwork         `yaml:"AI"`                   // Dedicated AI-client port and limits (see AINetwork)
	AfkSeconds           ConfigInt         `yaml:"AfkSeconds"`           // How long until a player is marked as afk?
	MaxIdleSeconds       ConfigInt         `yaml:"MaxIdleSeconds"`       // How many seconds a player can go without a command in game before being kicked.
	TimeoutMods          ConfigBool        `yaml:"TimeoutMods"`          // Whether to kick admin/mods when idle too long.
	LinkDeadSeconds      ConfigInt         `yaml:"LinkDeadSeconds"`      // How many seconds a player will be link-dead allowing them to reconnect.
	LogoutRounds         ConfigInt         `yaml:"LogoutRounds"`         // How many rounds of uninterrupted meditation must be completed to log out.
}

// AINetwork groups the dedicated AI-client connection settings under Network.AI.
type AINetwork struct {
	Port             ConfigInt `yaml:"Port"`             // Dedicated telnet port for AI clients (0 = disabled)
	MaxConnections   ConfigInt `yaml:"MaxConnections"`   // Maximum number of concurrent AI connections
	CommandsPerRound ConfigInt `yaml:"CommandsPerRound"` // Max commands an AI connection may submit per round
}

func (n *Network) Validate() {

	// Ignore TelnetPort
	// Ignore LocalPort
	// Ignore TimeoutMods

	if n.MaxTelnetConnections < 1 {
		n.MaxTelnetConnections = 50 // default
	}

	if n.SSHPort < 0 {
		n.SSHPort = 0
	}

	if n.MaxSSHConnections < 1 {
		n.MaxSSHConnections = 50 // default
	}

	if n.AI.Port < 0 {
		n.AI.Port = 0
	}

	if n.AI.MaxConnections < 1 {
		n.AI.MaxConnections = 20 // default
	}

	if n.AI.CommandsPerRound < 1 {
		n.AI.CommandsPerRound = 2 // default
	}

	if n.HttpPort < 0 {
		n.HttpPort = 0 // default
	}

	if n.HttpsPort < 0 {
		n.HttpsPort = 0 // default
	}

	if n.AfkSeconds < 0 {
		n.AfkSeconds = 0
	}

	if n.MaxIdleSeconds < 0 {
		n.MaxIdleSeconds = 0
	}

	if n.LinkDeadSeconds < 0 {
		n.LinkDeadSeconds = 0
	}

	if n.LogoutRounds < 0 {
		n.LogoutRounds = 0 // default
	}

}

func GetNetworkConfig() Network {
	configDataLock.RLock()
	defer configDataLock.RUnlock()

	if !configData.validated {
		configData.Validate()
	}
	return configData.Network
}
