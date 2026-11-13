package api

import (
	"time"
)

// Owned by the session builder. See SPEC.md "Sessions" and API.md "/v0/sessions".

// MachineStats are a machine's numbers (SPEC 8 "Machine numbers"), from
// the admission probe; -1 = unknown.
type MachineStats struct {
	MemoryUsedMB  int64     `json:"memory_used_mb"` // total less what the OS counts available
	MemoryTotalMB int64     `json:"memory_total_mb"`
	SwapUsedMB    int64     `json:"swap_used_mb"`
	SwapTotalMB   int64     `json:"swap_total_mb"`
	DiskFreeMB    int64     `json:"disk_free_mb"` // $ZM_HOME's volume
	DiskTotalMB   int64     `json:"disk_total_mb"`
	Sessions      int       `json:"sessions"`     // live managed sessions
	MaxSessions   int       `json:"max_sessions"` // admission's max_managed_sessions
	At            time.Time `json:"at"`           // when sampled
	// RemoteTasks: the machine takes tasks a project owner starts from
	// another machine (config remote_tasks; SPEC 8 "Machine choice").
	RemoteTasks bool `json:"remote_tasks,omitempty"`
	// ScriptRuns: it takes remote tasks and its zm runs scripts (SPEC 8
	// "Script runs"); machine choice sends a run only where this is set.
	ScriptRuns bool `json:"script_runs,omitempty"`
	// CommandRuns: it runs a script run's one command line (TaskRun.Command,
	// an approved command action, SPEC 16); a machine whose zm predates it
	// leaves it unset and an action naming it is refused when asked.
	CommandRuns bool `json:"command_runs,omitempty"`
	// TaskSlots: how many remote tasks it runs at once (config
	// remote_task_slots, default 1; SPEC 8 "Waiting line"), set only when it
	// takes them.
	TaskSlots int `json:"task_slots,omitempty"`
	// SandboxTasks: the machine runs a task driver for sandbox tasks;
	// SandboxSlots is how many it runs at once, set only with SandboxTasks.
	SandboxTasks bool `json:"sandbox_tasks,omitempty"`
	SandboxSlots int  `json:"sandbox_slots,omitempty"`
	// OSUser is the account zm runs as there, set only when it takes remote
	// tasks: the one user an authorize-key action may name (zm never uses
	// sudo).
	OSUser string `json:"os_user,omitempty"`
	// T3Enabled: the machine's owner set a t3 block, so its project's owner
	// may start Z3 there (POST /v0/team/machines/{machine}/t3). T3 is the
	// Z3 server zm started there, while it answers on loopback.
	T3Enabled bool       `json:"t3_enabled,omitempty"`
	T3        *MachineT3 `json:"t3,omitempty"`
	// Zm is the zm build the machine runs and its release channel state
	// (SPEC 2 "Release channel"); absent from a machine older than it.
	Zm *MachineZm `json:"zm,omitempty"`
	// Potluck is Zm under its new name, the one a machine sends from the
	// rename's step 2 on; an authority takes either. Build reads both.
	Potluck *MachineZm `json:"potluck,omitempty"`
}

// Build is the build the machine runs: Potluck, else Zm; nil from a machine
// older than either.
func (s MachineStats) Build() *MachineZm {
	if s.Potluck != nil {
		return s.Potluck
	}
	return s.Zm
}

// MachineT3 is a machine's running Z3 server (SPEC 8 "Z3 on a machine"):
// EnvironmentID joins the machine to a T3 client's saved environment.
type MachineT3 struct {
	EnvironmentID string `json:"environment_id"`
	Version       string `json:"version"`
	Port          int    `json:"port"`
}

// Machine is one row of GET /v0/machines.
type Machine struct {
	Machine string `json:"machine"`
	// Projects are the team projects (ULIDs) the machine heartbeats for
	// that the caller reads whole; absent for the answering machine.
	Projects []string `json:"projects,omitempty"`
	// Seen is the machine's last heartbeat; absent for the answering
	// machine (its numbers are its own, sampled at most every 30 s).
	Seen  *time.Time   `json:"seen,omitempty"`
	Stats MachineStats `json:"stats"`
}

// MachineList is GET /v0/machines.
type MachineList struct {
	Machines []Machine `json:"machines"`
	// Skipped are the authorities a replica could not merge (their machines
	// are missing from the list).
	Skipped []WorkSkipped `json:"skipped,omitempty"`
}
