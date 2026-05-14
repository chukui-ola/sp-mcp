package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

type Config struct {
	MaxBytesPerResponse int      `json:"max_bytes_per_response"`
	CommandTimeoutSec   int      `json:"command_timeout_seconds"`
	MaxParallel         int      `json:"max_parallel"`
	Hosts               []Host   `json:"hosts"`
	Targets             []Target `json:"targets"`
	hostsByID           map[string]Host
	targetsByID         map[string]Target
}

type Host struct {
	ID            string   `json:"id"`
	Type          string   `json:"type"`
	SSHTarget     string   `json:"ssh_target"`
	SSHOptions    []string `json:"ssh_options"`
	Supervisorctl string   `json:"supervisorctl"`
}

type Target struct {
	ID                     string   `json:"id"`
	Host                   string   `json:"host"`
	Name                   string   `json:"name"`
	Programs               []string `json:"programs"`
	IncludeRunningPrograms bool     `json:"include_running_programs"`
}

type Server struct {
	cfg    Config
	runner commandRunner
}

type commandRunner interface {
	Run(host Host, timeout time.Duration, name string, args []string) (string, error)
}

type execRunner struct{}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type operationResult struct {
	Target   string `json:"target"`
	Host     string `json:"host"`
	Program  string `json:"program,omitempty"`
	Action   string `json:"action"`
	OK       bool   `json:"ok"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
	Duration string `json:"duration"`
}

func main() {
	configPath := flag.String("config", "config.json", "path to JSON config")
	listenAddr := flag.String("listen", "", "HTTP listen address; empty means stdio mode")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	s := Server{cfg: cfg, runner: execRunner{}}
	if *listenAddr != "" {
		if err := s.serveHTTP(*listenAddr); err != nil {
			fmt.Fprintf(os.Stderr, "serve http: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := s.serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.MaxBytesPerResponse <= 0 {
		cfg.MaxBytesPerResponse = 64 * 1024
	}
	if cfg.CommandTimeoutSec <= 0 {
		cfg.CommandTimeoutSec = 30
	}
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 4
	}
	cfg.hostsByID = make(map[string]Host, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		if h.ID == "" {
			return Config{}, errors.New("host id is required")
		}
		if h.Type != "local" && h.Type != "ssh" {
			return Config{}, fmt.Errorf("host %q has unsupported type %q", h.ID, h.Type)
		}
		if h.Type == "ssh" && h.SSHTarget == "" {
			return Config{}, fmt.Errorf("host %q requires ssh_target", h.ID)
		}
		if h.Supervisorctl == "" {
			h.Supervisorctl = "supervisorctl"
		}
		cfg.hostsByID[h.ID] = h
	}
	cfg.targetsByID = make(map[string]Target, len(cfg.Targets))
	for _, t := range cfg.Targets {
		if t.ID == "" || t.Host == "" {
			return Config{}, errors.New("target id and host are required")
		}
		if _, ok := cfg.hostsByID[t.Host]; !ok {
			return Config{}, fmt.Errorf("target %q references unknown host %q", t.ID, t.Host)
		}
		if len(t.Programs) == 0 && !t.IncludeRunningPrograms {
			return Config{}, fmt.Errorf("target %q requires at least one program or include_running_programs", t.ID)
		}
		seen := map[string]bool{}
		for _, program := range t.Programs {
			if strings.TrimSpace(program) == "" {
				return Config{}, fmt.Errorf("target %q has empty program", t.ID)
			}
			if seen[program] {
				return Config{}, fmt.Errorf("target %q has duplicate program %q", t.ID, program)
			}
			seen[program] = true
		}
		cfg.targetsByID[t.ID] = t
	}
	return cfg, nil
}

func (s Server) serve(r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(w)
	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		if err := enc.Encode(s.handle(req)); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (s Server) serveHTTP(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		defer r.Body.Close()
		var req request
		if err := json.NewDecoder(io.LimitReader(r.Body, 8*1024*1024)).Decode(&req); err != nil {
			writeHTTPResponse(w, response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeHTTPResponse(w, s.handle(req))
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "sp-mcp listening on %s\n", addr)
	return server.ListenAndServe()
}

func writeHTTPResponse(w http.ResponseWriter, res response) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (s Server) handle(req request) response {
	res := response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		res.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "sp-mcp", "version": "0.1.0"},
		}
	case "tools/list":
		res.Result = map[string]any{"tools": s.tools()}
	case "tools/call":
		var params callToolParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			res.Error = &rpcError{Code: -32602, Message: err.Error()}
			return res
		}
		result, err := s.callTool(params.Name, params.Arguments)
		if err != nil {
			res.Result = toolResult{
				IsError: true,
				Content: []toolContent{{
					Type: "text",
					Text: err.Error(),
				}},
			}
			return res
		}
		res.Result = result
	default:
		res.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	return res
}

func (s Server) tools() []map[string]any {
	return []map[string]any{
		{
			"name":        "list_supervisor_targets",
			"description": "List configured remote/local supervisor targets and allowed programs.",
			"inputSchema": schema("object", map[string]any{}, []string{}),
		},
		{
			"name":        "supervisor_status",
			"description": "Get supervisor status for configured targets. Programs are restricted to each target allowlist.",
			"inputSchema": schema("object", map[string]any{
				"targets":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1},
				"programs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1},
			}, []string{}),
		},
		{
			"name":        "restart_supervisor_processes",
			"description": "Restart allowlisted supervisor programs across one or more configured targets.",
			"inputSchema": schema("object", map[string]any{
				"targets": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1},
				"programs": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"minItems":    1,
					"description": "Optional subset of programs to restart on each target. Omit to restart every allowlisted program for the selected targets.",
				},
			}, []string{"targets"}),
		},
	}
}

func schema(t string, properties map[string]any, required []string) map[string]any {
	return map[string]any{
		"type":                 t,
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

func (s Server) callTool(name string, args json.RawMessage) (toolResult, error) {
	switch name {
	case "list_supervisor_targets":
		out, err := s.listTargets()
		return s.textResult(out), err
	case "supervisor_status":
		var in struct {
			Targets  []string `json:"targets"`
			Programs []string `json:"programs"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return toolResult{}, err
		}
		out, err := s.supervisorStatus(in.Targets, in.Programs)
		return s.textResult(out), err
	case "restart_supervisor_processes":
		var in struct {
			Targets  []string `json:"targets"`
			Programs []string `json:"programs"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return toolResult{}, err
		}
		out, err := s.restartSupervisorProcesses(in.Targets, in.Programs)
		return s.textResult(out), err
	default:
		return toolResult{}, fmt.Errorf("unknown tool %q", name)
	}
}

func decodeArgs(data json.RawMessage, v any) error {
	if len(data) == 0 {
		data = []byte("{}")
	}
	return json.Unmarshal(data, v)
}

func (s Server) textResult(text string) toolResult {
	if len(text) > s.cfg.MaxBytesPerResponse {
		text = text[:s.cfg.MaxBytesPerResponse] + "\n[truncated]\n"
	}
	return toolResult{Content: []toolContent{{Type: "text", Text: text}}}
}

func (s Server) listTargets() (string, error) {
	type row struct {
		ID                     string   `json:"id"`
		Host                   string   `json:"host"`
		HostType               string   `json:"host_type"`
		Name                   string   `json:"name"`
		Programs               []string `json:"programs"`
		IncludeRunningPrograms bool     `json:"include_running_programs,omitempty"`
		DiscoveryError         string   `json:"discovery_error,omitempty"`
	}
	rows := make([]row, 0, len(s.cfg.Targets))
	for _, target := range s.cfg.Targets {
		host := s.cfg.hostsByID[target.Host]
		programs := append([]string(nil), target.Programs...)
		var discoveryError string
		if target.IncludeRunningPrograms {
			discovered, err := s.discoverRunningPrograms(host)
			if err != nil {
				discoveryError = err.Error()
			} else {
				programs = mergePrograms(programs, discovered)
			}
		}
		rows = append(rows, row{
			ID:                     target.ID,
			Host:                   target.Host,
			HostType:               host.Type,
			Name:                   target.Name,
			Programs:               programs,
			IncludeRunningPrograms: target.IncludeRunningPrograms,
			DiscoveryError:         discoveryError,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	data, _ := json.MarshalIndent(rows, "", "  ")
	return string(data), nil
}

func (s Server) supervisorStatus(targetIDs, programs []string) (string, error) {
	jobs, err := s.selectJobs(targetIDs, programs)
	if err != nil {
		return "", err
	}
	results := s.runJobs("status", jobs)
	return marshalResults(results)
}

func (s Server) restartSupervisorProcesses(targetIDs, programs []string) (string, error) {
	if len(targetIDs) == 0 {
		return "", errors.New("targets is required")
	}
	jobs, err := s.selectJobs(targetIDs, programs)
	if err != nil {
		return "", err
	}
	results := s.runJobs("restart", jobs)
	return marshalResults(results)
}

type job struct {
	target  Target
	host    Host
	program string
}

func (s Server) selectJobs(targetIDs, programs []string) ([]job, error) {
	targets, err := s.selectTargets(targetIDs)
	if err != nil {
		return nil, err
	}
	var jobs []job
	for _, target := range targets {
		host := s.cfg.hostsByID[target.Host]
		selected, err := s.selectPrograms(target, host, programs)
		if err != nil {
			return nil, err
		}
		for _, program := range selected {
			jobs = append(jobs, job{target: target, host: host, program: program})
		}
	}
	return jobs, nil
}

func (s Server) selectTargets(ids []string) ([]Target, error) {
	if len(ids) == 0 {
		targets := append([]Target(nil), s.cfg.Targets...)
		sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
		return targets, nil
	}
	var targets []Target
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		target, ok := s.cfg.targetsByID[id]
		if !ok {
			return nil, fmt.Errorf("unknown target %q", id)
		}
		targets = append(targets, target)
		seen[id] = true
	}
	return targets, nil
}

func (s Server) selectPrograms(target Target, host Host, requested []string) ([]string, error) {
	programs := append([]string(nil), target.Programs...)
	if target.IncludeRunningPrograms {
		discovered, err := s.discoverRunningPrograms(host)
		if err != nil {
			return nil, fmt.Errorf("discover running programs for target %q: %w", target.ID, err)
		}
		programs = mergePrograms(programs, discovered)
	}
	if len(requested) == 0 {
		return programs, nil
	}
	allowed := map[string]bool{}
	for _, program := range programs {
		allowed[program] = true
	}
	var selected []string
	seen := map[string]bool{}
	for _, program := range requested {
		if !allowed[program] {
			return nil, fmt.Errorf("program %q is not allowed on target %q", program, target.ID)
		}
		if !seen[program] {
			selected = append(selected, program)
			seen[program] = true
		}
	}
	return selected, nil
}

func (s Server) discoverRunningPrograms(host Host) ([]string, error) {
	timeout := time.Duration(s.cfg.CommandTimeoutSec) * time.Second
	out, err := s.runner.Run(host, timeout, host.Supervisorctl, []string{"status"})
	if err != nil {
		return nil, err
	}
	programs := parseRunningPrograms(out)
	if len(programs) == 0 {
		return nil, errors.New("no running supervisor programs found")
	}
	return programs, nil
}

func parseRunningPrograms(out string) []string {
	var programs []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "RUNNING" {
			continue
		}
		program := fields[0]
		if !seen[program] {
			programs = append(programs, program)
			seen[program] = true
		}
	}
	sort.Strings(programs)
	return programs
}

func mergePrograms(base, extra []string) []string {
	seen := map[string]bool{}
	var merged []string
	for _, program := range append(base, extra...) {
		if program == "" || seen[program] {
			continue
		}
		merged = append(merged, program)
		seen[program] = true
	}
	sort.Strings(merged)
	return merged
}

func (s Server) runJobs(action string, jobs []job) []operationResult {
	maxParallel := s.cfg.MaxParallel
	if maxParallel < 1 {
		maxParallel = 1
	}
	results := make([]operationResult, len(jobs))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = s.runSupervisorAction(action, j)
		}(i, j)
	}
	wg.Wait()
	return results
}

func (s Server) runSupervisorAction(action string, j job) operationResult {
	start := time.Now()
	timeout := time.Duration(s.cfg.CommandTimeoutSec) * time.Second
	out, err := s.runner.Run(j.host, timeout, j.host.Supervisorctl, []string{action, j.program})
	result := operationResult{
		Target:   j.target.ID,
		Host:     j.host.ID,
		Program:  j.program,
		Action:   action,
		OK:       err == nil,
		Output:   strings.TrimSpace(out),
		Duration: time.Since(start).Round(time.Millisecond).String(),
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

func marshalResults(results []operationResult) (string, error) {
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (execRunner) Run(host Host, timeout time.Duration, name string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var cmd *exec.Cmd
	if host.Type == "local" {
		cmd = exec.CommandContext(ctx, name, args...)
	} else {
		remote := shellJoin(append([]string{name}, args...))
		sshArgs := append([]string{}, host.SSHOptions...)
		sshArgs = append(sshArgs, host.SSHTarget, remote)
		cmd = exec.CommandContext(ctx, "ssh", sshArgs...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", errors.New("command timed out")
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return string(out), errors.New(msg)
	}
	return string(out), nil
}

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
