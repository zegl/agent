package agent

type AgentConfiguration struct {
	ConfigPath                string
	BootstrapScript           string
	BuildPath                 string
	HooksPath                 string
	PluginsPath               string
	GitCloneFlags             string
	GitCleanFlags             string
	SSHKeyscan                bool
	CommandEval               bool
	PluginsEnabled            bool
	RunInPty                  bool
	TimestampLines            bool
	DisconnectAfterJob        bool
	DisconnectAfterJobTimeout int
	Shell                     string
}
