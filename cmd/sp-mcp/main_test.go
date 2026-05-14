package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeRunner struct {
	calls []fakeCall
	out   string
	err   error
}

type fakeCall struct {
	host string
	name string
	args []string
}

func (r *fakeRunner) Run(host Host, _ time.Duration, name string, args []string) (string, error) {
	r.calls = append(r.calls, fakeCall{host: host.ID, name: name, args: append([]string(nil), args...)})
	if r.out != "" {
		return r.out, r.err
	}
	if r.err != nil {
		return "", r.err
	}
	return "ok", nil
}

func TestLoadConfigBuildsIndexesAndDefaults(t *testing.T) {
	path := writeTempConfig(t, `{
		"hosts": [{"id": "a", "type": "ssh", "ssh_target": "deploy@example.com"}],
		"targets": [{"id": "api-a", "host": "a", "programs": ["api", "worker"]}]
	}`)

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.CommandTimeoutSec != 30 {
		t.Fatalf("CommandTimeoutSec = %d, want 30", cfg.CommandTimeoutSec)
	}
	if cfg.MaxParallel != 4 {
		t.Fatalf("MaxParallel = %d, want 4", cfg.MaxParallel)
	}
	if cfg.hostsByID["a"].Supervisorctl != "supervisorctl" {
		t.Fatalf("Supervisorctl default not set")
	}
	if _, ok := cfg.targetsByID["api-a"]; !ok {
		t.Fatalf("target index missing api-a")
	}
}

func TestSelectProgramsRejectsUnlistedProgram(t *testing.T) {
	s := Server{}
	target := Target{ID: "api-a", Programs: []string{"api"}}
	if _, err := s.selectPrograms(target, Host{}, []string{"worker"}); err == nil {
		t.Fatalf("selectPrograms() error = nil, want rejection")
	}
}

func TestParseRunningPrograms(t *testing.T) {
	out := `api                            RUNNING   pid 123, uptime 0:01:00
worker                         STOPPED   Not started
group:rpc.pay                  RUNNING   pid 456, uptime 0:02:00
api                            RUNNING   pid 789, uptime 0:03:00`

	got := parseRunningPrograms(out)
	want := []string{"api", "group:rpc.pay"}
	if len(got) != len(want) {
		t.Fatalf("parseRunningPrograms() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseRunningPrograms() = %#v, want %#v", got, want)
		}
	}
}

func TestDynamicRunningProgramsAreAllowed(t *testing.T) {
	cfg := testConfig()
	cfg.Targets = []Target{{ID: "all-a", Host: "a", IncludeRunningPrograms: true}}
	cfg.targetsByID = map[string]Target{"all-a": cfg.Targets[0]}
	runner := &fakeRunner{out: "api RUNNING pid 1\nworker STOPPED\nrpc.pay RUNNING pid 2\n"}
	s := Server{cfg: cfg, runner: runner}

	out, err := s.supervisorStatus([]string{"all-a"}, []string{"rpc.pay"})
	if err != nil {
		t.Fatalf("supervisorStatus() error = %v", err)
	}
	var results []operationResult
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(results) != 1 || results[0].Program != "rpc.pay" {
		t.Fatalf("results = %#v, want rpc.pay status", results)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("len(calls) = %d, want discover + status", len(runner.calls))
	}
	if runner.calls[0].args[0] != "status" || len(runner.calls[0].args) != 1 {
		t.Fatalf("discover args = %#v, want status", runner.calls[0].args)
	}
	if runner.calls[1].args[0] != "status" || runner.calls[1].args[1] != "rpc.pay" {
		t.Fatalf("status args = %#v, want status rpc.pay", runner.calls[1].args)
	}
}

func TestDynamicRunningProgramsUseStdoutWhenStatusExitsNonZero(t *testing.T) {
	cfg := testConfig()
	cfg.Targets = []Target{{ID: "all-a", Host: "a", IncludeRunningPrograms: true}}
	cfg.targetsByID = map[string]Target{"all-a": cfg.Targets[0]}
	runner := &fakeRunner{
		out: "api RUNNING pid 1\nworker STOPPED\n",
		err: errors.New("exit status 1"),
	}
	s := Server{cfg: cfg, runner: runner}

	selected, err := s.selectPrograms(cfg.Targets[0], cfg.Hosts[0], nil)
	if err != nil {
		t.Fatalf("selectPrograms() error = %v", err)
	}
	if len(selected) != 1 || selected[0] != "api" {
		t.Fatalf("selected = %#v, want api", selected)
	}
}

func TestRestartUsesOnlySelectedTargetsAndPrograms(t *testing.T) {
	cfg := testConfig()
	runner := &fakeRunner{}
	s := Server{cfg: cfg, runner: runner}

	out, err := s.restartSupervisorProcesses([]string{"api-a", "api-b"}, []string{"api"})
	if err != nil {
		t.Fatalf("restartSupervisorProcesses() error = %v", err)
	}
	var results []operationResult
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if len(runner.calls) != 2 {
		t.Fatalf("len(calls) = %d, want 2", len(runner.calls))
	}
	for _, call := range runner.calls {
		if call.name != "supervisorctl" {
			t.Fatalf("command = %q, want supervisorctl", call.name)
		}
		if len(call.args) != 2 || call.args[0] != "restart" || call.args[1] != "api" {
			t.Fatalf("args = %#v, want restart api", call.args)
		}
	}
}

func TestRestartReportsPerProgramFailures(t *testing.T) {
	cfg := testConfig()
	s := Server{cfg: cfg, runner: &fakeRunner{err: errors.New("restart failed")}}

	out, err := s.restartSupervisorProcesses([]string{"api-a"}, []string{"api"})
	if err != nil {
		t.Fatalf("restartSupervisorProcesses() error = %v", err)
	}
	var results []operationResult
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(results) != 1 || results[0].OK || results[0].Error != "restart failed" {
		t.Fatalf("results = %#v, want one failed result", results)
	}
}

func testConfig() Config {
	cfg := Config{
		MaxBytesPerResponse: 64 * 1024,
		CommandTimeoutSec:   30,
		MaxParallel:         2,
		Hosts: []Host{
			{ID: "a", Type: "ssh", SSHTarget: "deploy@a", Supervisorctl: "supervisorctl"},
			{ID: "b", Type: "ssh", SSHTarget: "deploy@b", Supervisorctl: "supervisorctl"},
		},
		Targets: []Target{
			{ID: "api-a", Host: "a", Programs: []string{"api", "worker"}},
			{ID: "api-b", Host: "b", Programs: []string{"api", "worker"}},
		},
	}
	cfg.hostsByID = map[string]Host{"a": cfg.Hosts[0], "b": cfg.Hosts[1]}
	cfg.targetsByID = map[string]Target{"api-a": cfg.Targets[0], "api-b": cfg.Targets[1]}
	return cfg
}

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}
