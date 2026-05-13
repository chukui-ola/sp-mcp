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
	err   error
}

type fakeCall struct {
	host string
	name string
	args []string
}

func (r *fakeRunner) Run(host Host, _ time.Duration, name string, args []string) (string, error) {
	r.calls = append(r.calls, fakeCall{host: host.ID, name: name, args: append([]string(nil), args...)})
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
	target := Target{ID: "api-a", Programs: []string{"api"}}
	if _, err := selectPrograms(target, []string{"worker"}); err == nil {
		t.Fatalf("selectPrograms() error = nil, want rejection")
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
