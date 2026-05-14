# sp-mcp

MCP server for checking and restarting allowlisted `supervisorctl` programs across local or SSH hosts.

The server intentionally does not expose arbitrary shell execution. Hosts and restartable programs must be declared in `config.json`.

## Build

```bash
go build ./cmd/sp-mcp
```

## Run

Stdio mode:

```bash
./sp-mcp -config config.example.json
```

HTTP mode for supervisor:

```bash
./sp-mcp -config /var/www/slp/sp-mcp/config.json -listen 127.0.0.1:18082
```

The server supports MCP-style JSON-RPC over stdio by default. With `-listen`, it exposes:

- `GET /healthz`
- `POST /mcp`

## Tools

- `list_supervisor_targets`: list configured hosts, targets, and allowed programs.
- `supervisor_status`: run `supervisorctl status <program>` for selected targets.
- `restart_supervisor_processes`: run `supervisorctl restart <program>` for selected targets.

Example restart arguments:

```json
{
  "targets": ["api-a", "api-b"],
  "programs": ["slp-api"]
}
```

Omit `programs` to restart every allowlisted program for the selected targets.
Targets can also set `include_running_programs: true` to allow every program currently reported as `RUNNING` by `supervisorctl status` on that host.

## Configuration

```json
{
  "max_bytes_per_response": 65536,
  "command_timeout_seconds": 30,
  "max_parallel": 4,
  "hosts": [
    {
      "id": "test-a",
      "type": "ssh",
      "ssh_target": "deploy@test-a.example.com",
      "ssh_options": ["-o", "BatchMode=yes", "-o", "ConnectTimeout=5"],
      "supervisorctl": "supervisorctl"
    }
  ],
  "targets": [
    {
      "id": "api-a",
      "host": "test-a",
      "name": "API on test-a",
      "programs": ["slp-api", "slp-worker"]
    },
    {
      "id": "test-a-running",
      "host": "test-a",
      "name": "All running supervisor programs on test-a",
      "include_running_programs": true
    }
  ]
}
```

`type` can be `ssh` or `local`. Remote commands are executed as:

```text
ssh <ssh_options...> <ssh_target> 'supervisorctl' 'restart|status' '<program>'
```

## Jenkins Deploy

A typical deployment installs the binary and config under:

```text
/var/www/slp/sp-mcp
```

and installs:

```text
/etc/supervisor/conf.d/sp-mcp.conf
```

The Jenkins job expects passwordless sudo for a narrow set of install and supervisor commands. Install and validate:

```bash
sudo install -m 0440 deploy/sudoers/sp-mcp-jenkins /etc/sudoers.d/sp-mcp-jenkins
sudo visudo -cf /etc/sudoers.d/sp-mcp-jenkins
```

If Jenkins reports `sudo: a password is required`, this sudoers file is missing on the Jenkins host or its username does not match the actual Jenkins process user. Check the user with `id -un` in the job log and adjust the first field in `deploy/sudoers/sp-mcp-jenkins` when needed.

The supervisor process runs as `jenkins` so SSH-based restarts use the Jenkins user's key and `HOME=/var/lib/jenkins`.
