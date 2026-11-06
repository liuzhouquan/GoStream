# GoStream development guide

Read `docs/SDD.md`, `docs/DELIVERY_SCOPE.md`, `docs/TEST_STANDARD.md`, and the recent entries in
`PROGRESS.md` before choosing the next work package. Use the Go version declared
in `go.mod`; the local installation is `/usr/local/go/bin/go`. Ask about a missing
or incompatible runtime instead of silently falling back to another version.

## Track commands to completion

- Always surface `session_id`, `exit_code`, and command output from execution
  tools. Printing only `result.output` hides running processes and failures.
- A returned process `session_id` means the command must be resumed/polled until
  an exit code is available. Empty stdout does not mean completion or failure.
- Resume a pending orchestration cell with its cell-wait tool; resume a pending
  shell process with its process-session tool. Keep those identifiers distinct.
- Start dependent actions only after the preceding command exits successfully.
  Before retrying a push, PR creation, or merge, finish its existing process and
  inspect the remote state. Do not launch duplicate commands based on blank output.

## Keep validation proportional and observable

- Run relevant tests for the change, then the required checks. Repeat a completed
  suite only after further relevant changes, a failure, or an unresolved concern.
- Full Go race testing currently takes several minutes. A full-size A04 run takes
  roughly 20 minutes, including six acceptance fixtures and two profile fixtures.
  Documentation or timing-tool edits do not require another A04 benchmark.
- Preserve assertions, security settings, and acceptance gates. Do not lower
  production bcrypt cost or remove fault checks to make tests faster.
- Use `tools/task_timing.py` for long local commands. It records wall/CPU time,
  output silence, process state, active Go tests, and explicit completion/timeout.
  Supply `go test -json` to obtain individual test durations.
- For Actions, use its job/step timestamps and critical path. Parallel job times
  cannot be added to obtain the run's elapsed time. Collect a compact status once
  per interval, preserve the run ID, and report the current slow stage to the user.
- Store raw timing logs under ignored `data/diagnostics/`. Commit only sanitized
  aggregate evidence; raw command output can contain credentials or fixture data.

Commands and evidence are documented in `docs/DEVELOPMENT_TIMING.md`.
