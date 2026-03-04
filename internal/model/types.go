package model

import "time"

type DeployFlags struct {
	Pull          bool   `json:"pull" yaml:"pull"`
	RemoveOrphans bool   `json:"remove_orphans" yaml:"remove_orphans"`
	Recreate      string `json:"recreate" yaml:"recreate"`
}

type Profile struct {
	Pull          *bool   `yaml:"pull"`
	RemoveOrphans *bool   `yaml:"remove_orphans"`
	Recreate      *string `yaml:"recreate"`
}

type Target struct {
	ID         string             `yaml:"-" json:"id"`
	Host       string             `yaml:"host" json:"host"`
	Port       int                `yaml:"port" json:"port"`
	User       string             `yaml:"user" json:"user"`
	DeployRoot string             `yaml:"deploy_root" json:"deploy_root"`
	Defaults   DeployFlags        `yaml:"defaults" json:"defaults"`
	Profiles   map[string]Profile `yaml:"profiles" json:"profiles,omitempty"`
}

type CCMStack struct {
	ID           string       `yaml:"-" json:"id"`
	TargetID     string       `yaml:"target" json:"target_id"`
	DeploySubdir string       `yaml:"deploy_subdir" json:"deploy_subdir"`
	Profile      string       `yaml:"profile" json:"profile,omitempty"`
	Restart      StackRestart `yaml:"restart" json:"restart,omitempty"`
	Target       *Target      `yaml:"-" json:"-"`
	Flags        DeployFlags  `yaml:"-" json:"flags"`
}

type StackRestart struct {
	Strategy   string                                `yaml:"strategy" json:"strategy,omitempty"`
	Containers map[string]ContainerRestartPreference `yaml:"containers" json:"containers,omitempty"`
}

type ContainerRestartPreference struct {
	Strategy string `yaml:"strategy" json:"strategy,omitempty"`
}

type RestartStrategy struct {
	Cron     string `yaml:"cron" json:"cron"`
	Timezone string `yaml:"timezone,omitempty" json:"timezone,omitempty"`
}

type RestartTrackingEntry struct {
	Key                 string    `json:"key"`
	Scope               string    `json:"scope"`
	StackID             string    `json:"stack_id"`
	TargetID            string    `json:"target_id"`
	ProjectName         string    `json:"project_name"`
	ContainerSelector   string    `json:"container_selector,omitempty"`
	Strategy            string    `json:"strategy"`
	Cron                string    `json:"cron"`
	Timezone            string    `json:"timezone"`
	LastEvaluatedMinute string    `json:"last_evaluated_minute,omitempty"`
	LastAttemptAt       time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt       time.Time `json:"last_success_at,omitempty"`
	LastResult          string    `json:"last_result,omitempty"`
	LastExitCode        int       `json:"last_exit_code,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	ScheduledRestarts   int       `json:"scheduled_restarts"`
}

type RestartDisplay struct {
	Enabled  bool   `json:"enabled"`
	Scope    string `json:"scope,omitempty"`
	Source   string `json:"source,omitempty"`
	Strategy string `json:"strategy,omitempty"`
	Cron     string `json:"cron,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	Note     string `json:"note,omitempty"`
}

type Container struct {
	ID             string            `json:"id"`
	ContainerID    string            `json:"container_id"`
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	Status         string            `json:"status"`
	RestartCount   int               `json:"restart_count"`
	Ports          []string          `json:"ports"`
	TargetID       string            `json:"target_id"`
	ComposeProject string            `json:"compose_project,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Uptime         string            `json:"uptime"`
	Restart        *RestartDisplay   `json:"restart,omitempty"`
	RawState       map[string]any    `json:"-"`
}

type ComposeProject struct {
	ID          string      `json:"id"`
	TargetID    string      `json:"target_id"`
	ProjectName string      `json:"project_name"`
	Status      string      `json:"status"`
	Containers  []Container `json:"containers"`
}

type InventoryRow struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	TargetID string `json:"target_id"`
	Status   string `json:"status"`
	Count    int    `json:"count,omitempty"`
}

type TargetInventory struct {
	TargetID   string           `json:"target_id"`
	Containers []Container      `json:"containers"`
	Projects   []ComposeProject `json:"projects"`
	At         time.Time        `json:"at"`
	Err        string           `json:"err,omitempty"`
}

type DeployRequest struct {
	CCMStack   string            `json:"ccm_stack"`
	Repo       string            `json:"repo"`
	SHA        string            `json:"sha"`
	ComposeYML string            `json:"compose_yml"`
	EnvFile    string            `json:"env_file,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Caddyfile  string            `json:"caddyfile,omitempty"`
	RunCompose *bool             `json:"run_compose,omitempty"`
}

type CommandResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}
