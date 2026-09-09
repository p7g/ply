# ply

A minimal coding agent in Go. Its state is an append-only JSONL transcript; its
model tools are `bash` and `plan`. It prints ordinary terminal text, without a
TUI.

## Build and configure

Requires Go 1.25.6 or newer, Linux or macOS, and Bash. The only library dependency
is `github.com/pelletier/go-toml/v2` v2.4.3. HTTP, streaming, JSON, process
supervision, and locking use the standard library.

```sh
make build
export PATH="$PWD/bin:$PATH"
# Or: make install  (installs all executables in GOBIN / GOPATH/bin)

mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/ply"
${EDITOR:-vi} "${XDG_CONFIG_HOME:-$HOME/.config}/ply/config.toml"
```

Example **user** configuration; replace `YOUR_MODEL` and set its actual context
window. There is deliberately no built-in model table.

```toml
model = "YOUR_MODEL"
context_window = 200000
base_url = "https://api.openai.com/v1"
# api_key = "..."  # alternatively, export PLY_API_KEY
compact_at = 0.8
pager = true
detach = false

[provider]
retries = 5

[approve]
command = "ply-approve-chain ply-approve-allowlist ply-approve-ask"
model = ""                  # empty inherits the main model
context_window = 0          # zero inherits the main context window

[bash]
shell = "/bin/bash"
default_timeout = 120
max_output_lines = 200       # model-facing command output

[output]
max_lines = 200              # terminal rendering only
color = "auto"
show_thinking = false
show_usage = true
```

Set `PLY_API_KEY` for an authenticated Responses endpoint, or set `api_key` in
trusted configuration. `--api-key` follows the same precedence as other options,
but environment/configuration avoids exposing the key in process arguments.
`OPENAI_API_KEY` is no longer read; migrate existing setups to `PLY_API_KEY`. Then:

```sh
ply -m 'Explain this repository' work.jsonl
ply -m 'Implement the change and run the relevant tests' work.jsonl
ply work.jsonl                         # review current context
```

The provider sends stateless streaming requests to `BASE_URL/responses`, with
`store: false`, replayed items, and encrypted reasoning included. It supports
OpenResponses-compatible endpoints; it does not translate Chat Completions API
responses. See the [OpenResponses reference](https://www.openresponses.org/reference).
No real API key is needed for tests or recorded-response replay.

## Input and output

```sh
ply -m 'First paragraph' -m 'Second paragraph' work.jsonl
ply -F request.txt work.jsonl
printf 'Explain the build\n' | ply work.jsonl
ply -e work.jsonl                      # $EDITOR, with commented transcript tail
ply -H -m 'Continue' work.jsonl         # show context first
ply -HH work.jsonl                     # show all history, including pre-compaction
ply --tail 20 work.jsonl
ply -f work.jsonl                      # read-only follow, no writer lock
ply --show-thinking work.jsonl
ply --no-pager work.jsonl
ply --allow-empty work.jsonl           # react without adding a user message
ply --no-tools -q -m 'Summarize this project' /tmp/summary.jsonl
```

Without message flags, nonempty piped stdin becomes a message; an empty pipe
renders the transcript. Follow and transcript actions do not consume stdin.
`-q` prints only assistant prose. `$PAGER` defaults to `less -FRX`; live output
is never paged. `NO_COLOR` disables color, including `--output-color=always`.
While awaiting model output, tty stderr shows `thinking...` with elapsed seconds.
Command execution uses `running command...`; compaction uses `compacting...`.
Activity clears before prose or approval prompts and is suppressed in quiet,
redirected-stderr, and subagent output. A command is displayed immediately before
its approval and result, even when a response contains several commands.

`--show-thinking` displays provider-supplied reasoning summaries, including streamed
summaries when available. Providers without summaries show none. Encrypted
reasoning is preserved for replay but never displayed.

At turn completion, stderr reports the latest provider-reported input token count,
context-window size, and percentage. This is a reported snapshot, not an exact
live context count. Disable it with `--no-output-show-usage`; quiet and subagent
modes suppress it. Every completed response, including tool rounds, records usage.

`output.max_lines` limits only terminal command output. `bash.max_output_lines`
independently limits model-facing results; both default to 200. This changes the
old meaning of `output.max_lines`: migrate model-output tuning to
`bash.max_output_lines`. Full command logs remain available at the paths shown
in truncation notices. History can display more from retained logs; if a log is
missing, it falls back to the stored result.
Whitespace-only assistant messages are hidden, and outer blank lines are normalized
without changing the stored provider items.
Exit codes are 0 for completion/detachment, 1 for errors, 2 for invalid CLI
syntax, and 130 for an interrupted model round or foreground command.

## Plan, compact, and execute

```sh
ply --plan -m 'Plan the migration' work.jsonl
ply --plan -m 'Revise the rollout steps' work.jsonl
ply --show-plan work.jsonl
ply -m 'Implement the plan' work.jsonl

# Preserve the plan but start execution in fresh context:
ply --clear work.jsonl
ply -m 'Implement the plan' work.jsonl

# Or copy the plan to a different transcript:
ply --plan-from work.jsonl -m 'Implement' implementation.jsonl
ply --compact implementation.jsonl
```

Compaction happens automatically above `compact_at`, either a fraction of
`context_window` or an absolute token count. It preserves the latest plan
verbatim and asks the model to preserve the outstanding request. Neither
compaction nor clear rewrites the file. Planning instructions are turn-scoped
messages; switching modes does not change the stored system prompt or carry old
mode instructions into later turns. Active mode guidance survives compaction.
The model saves plans through the `plan` tool; ply displays the latest saved plan
once after a planning turn. Plan updates do not require shell approval.

## Approval and autonomous runs

Every bash call is approved before execution. The approver receives
`PLY_COMMAND`, `PLY_COMMAND_TRUNCATED`, `PLY_JUSTIFICATION`, `PLY_USER_MSG`,
`PLY_CWD`, `PLY_TRANSCRIPT`, `PLY_BACKGROUND`, `PLY_TIMEOUT`,
`PLY_APPROVAL_DEPTH`, `PLY_PLAN_MODE`, and `PLY_CONFIG_DIR`.
`PLY_PLAN_MODE=1` means inspection-only planning; nested approval requests carry
the originating planning mode, and a planning parent also restricts its children. It returns 0 to approve, 1 to deny,
or 2 to abstain. The optional `PLY_COMMAND_RENDERED=1` hint tells the bundled
ask approver that the command is already visible in the terminal, so it only
prints its confirmation prompt. Denial text on stdout goes back to the model, which is instructed to adapt or
explain the blocker. If all approvers abstain, their explanations are returned
and execution is denied. A later definitive decision supplies the final reason.
The prompt is `Allow? [y/N]`, with a subagent label for nested requests.

Bundled programs:

- `ply-approve-yolo`: approve everything.
- `ply-approve-ask`: ask on `/dev/tty`; deny if no terminal is available.
- `ply-approve-allowlist`: apply the first matching rule; otherwise abstain.
- `ply-approve-auto`: invoke `ply --no-tools -q` on a temporary transcript and
  accept a strict JSON object with `decision` (`APPROVE`, `DENY`, or `UNSURE`)
  and a nonempty `reason`. Denials and abstentions return that reason. Malformed
  responses and provider failures abstain with an explanation.
- `ply-approve-chain A B C`: execute programs in order until one does not
  abstain. Each argument names an executable; use a wrapper for arguments.

Allowlist rules are `allow REGEX` or `deny REGEX`, one per line. Empty lines and
`#` comments are ignored. Expressions use Go's regexp syntax and are unanchored
unless you add anchors. Rules match the **whole shell command string**, not
individual shell tokens. Prefer fully anchored rules when allowing commands.
Project `.ply/allowlist` takes precedence over the user-level `allowlist`.

```text
# Deny before broader allow rules.
deny ^rm\b
allow ^pwd$
allow ^git status --short$
```

```sh
# Interactive:
ply --approve-command='ply-approve-chain ply-approve-allowlist ply-approve-ask' \
  -m 'Fix the test' work.jsonl

# Autonomous; the default wait loop collects background results:
ply --approve-command=ply-approve-yolo -m 'Implement and test the change' work.jsonl

# Policy, model, then human fallback:
ply --approve-command='ply-approve-chain ply-approve-allowlist ply-approve-auto ply-approve-ask' \
  -m 'Implement the change' work.jsonl
```

The auto approver receives labeled prose explaining the command, user request,
working directory, justification, timeout, background execution, subagent depth,
and planning status. It permits task-relevant reads outside the working directory
and distinguishes them from broad filesystem searches, secret collection, and
unauthorized writes or transmissions. In plan mode it permits inspection and
denies implementation mutations. Truncated commands cause it to abstain.

`approve.model` and `approve.context_window` select an independent approval model;
empty/zero values inherit the effective main settings. The approval subprocess
receives resolved model, context window, endpoint, retry count, and credentials
through its environment, including overrides supplied as flags.

Approval is a policy hook, **not an OS sandbox**. Approved commands execute with
your account's permissions. The transcript and full output logs may contain
secrets; ply does not redact command output. API keys may come from
configuration or the environment; their values are redacted in `--show-config`
and omitted from `ply.config` snapshots.

## Background tasks and subagents

The model starts long commands with `background: true`. A separate supervisor
owns the deadline, log, and completion record, so tasks survive the parent
exiting. Timeouts and foreground interruption kill the command's process group.
Commands must not daemonize or escape that group. Each invocation only collects
tasks that belong to its own transcript, even when `.ply/` is shared.

```sh
ply --detach -m 'Start the long build' work.jsonl
ply --tasks work.jsonl
ply --kill=TASK_ID work.jsonl
ply --kill work.jsonl                  # all unfinished tasks in this transcript
ply --allow-empty work.jsonl           # collect results and continue

# Ask the model to fan out through ordinary background bash calls:
ply -m 'Use subagents to inspect the parser and renderer, then combine their findings' work.jsonl
# A child invocation looks like:
# ply --subagent -m 'Inspect the parser' parser-review.jsonl
```

Subagent stdout is JSONL (`hello`, `approval_request`, `result`, `error`); stdin
accepts matching `approve` replies and `steer` messages. The parent proxies
approvals through its own approver, incrementing depth at each hop, including
while waiting on foreground work. Completion includes the child's final result
and transcript path. Subagent mode waits for its tasks regardless of `detach`.
Closing the parent denies subsequent child approvals with `no parent attached`.
There is no reattachment or `--steer TASK` command in v1.

Ctrl+C during streaming records the received assistant text with `ply.partial`
and an interrupt marker. During a foreground command it records interrupted
output. While waiting it detaches and exits successfully, leaving tasks running.
During approval, Ctrl+C stops approval without executing the command. Expected
interruption exits with status 130 without printing Go’s `context canceled` error.

```sh
# Steering: Ctrl+C, then:
ply -m 'Actually, focus on the parser first' work.jsonl

# Queue turns:
ply -m 'Make the change' work.jsonl && ply -m 'Review it' work.jsonl

# Suspend with Ctrl+Z, then:
# fg; ply -m 'Next request' work.jsonl
```

## Configuration and prompt sources

Precedence: flags → `PLY_*` environment → project TOML → user TOML → defaults.
Project config is `.ply/config.toml`, found walking upward from the transcript's
working directory. `--config FILE` substitutes an explicit project config.
For example, `output.max_lines` maps to `PLY_OUTPUT_MAX_LINES` and
`--output-max-lines`. Boolean options have `--no-` inverses.

```sh
ply --show-config work.jsonl           # values and provenance; no API request
PLY_OUTPUT_MAX_LINES=50 ply -m 'Run tests' work.jsonl
ply --no-detach --no-show-thinking -m 'Continue' work.jsonl
```

Untrusted project configuration cannot set `api_key`, `model`, `base_url`, `provider.*`,
`approve.*`, or `bash.shell`; ignored keys produce warnings. Opt in for one run
with `--trust-project`, or add `trust = ["/absolute/path/to/repo"]` at the top
level of your **user** config. This also applies to an explicit `--config` file.
Other unknown or wrongly typed keys are errors.

The system prompt consists of built-in guidance, `ply-* --ply-prompt` hints,
ancestor `AGENTS.md` files (nearest last), and optional `system_file`. It is
recorded once and reread only with `--refresh-system`. Built-in system, mode, compaction, and approval prompts live in
`internal/prompts/*.txt` and are embedded in the binaries; runtime template files
are unnecessary. Discovery does not run
bundled approvers and gives each companion a two-second deadline.

## Companions

`ply-skill` discovers `.ply/skills/*/SKILL.md` and `.agents/skills/*/SKILL.md`
up the directory tree, plus user skills in `~/.config/ply/skills` (respecting
`XDG_CONFIG_HOME`) and `~/.agents/skills`. A nearer directory wins; within the
same directory, `.ply` wins over `.agents`. Project skills override user skills.
At user scope, the ply config directory wins over `~/.agents/skills`.
A `description:` frontmatter line supplies the prompt hint.

```sh
ply-skill list
ply-skill show NAME
```

To add a project skill, create or copy a directory containing `SKILL.md` into
`.ply/skills/NAME/` (or `.agents/skills/NAME/` to share it with other agents).
Use the user locations above for personal skills. A minimal skill is:

```markdown
---
name: review
description: Review a change and report actionable findings.
---
Read the change, inspect its callers, and verify important assumptions.
```

Run `ply-skill list` and `ply-skill show review` to check discovery. Existing
conversations cache their system prompt, so use `--refresh-system` after adding
skills or changing their descriptions. The companion reads the current skill
body when `show` is invoked. There is no installer or scaffold command.

`ply-mcp` reads `[servers.NAME]` entries from user and ancestor project
`mcp.toml` files. A nearer server definition wins. Discovery only reads config;
connections happen when an approved command invokes list/schema/call.

```toml
# .ply/mcp.toml (examples; install your chosen server separately)
[servers.local]
command = "my-mcp-server"
args = ["--stdio"]

[servers.remote]
url = "https://your-server.example/mcp"
[servers.remote.headers]
Authorization = "Bearer ${MCP_TOKEN}"
```

```sh
ply-mcp list
ply-mcp list local
ply-mcp schema local tool_name
ply-mcp call local tool_name '{"argument":"value"}'
```

The companion supports newline-delimited stdio and Streamable HTTP JSON/SSE,
initialization, session headers, tool pagination, schemas, and calls. Environment
variables expand in server `env` values and HTTP header values. It advertises
no sampling or elicitation capabilities. Each command has a two-minute deadline.
It does not implement OAuth login, legacy HTTP+SSE, or resumable SSE sessions;
provide authentication through configured headers or server environment. See
[MCP transports](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports).

## Transcript, recovery, and tests

Each JSONL item has a zero-based `seq` and UTC `ts`. Writes use `O_APPEND`, one
write per item, `fsync`, and an exclusive nonblocking `flock`. Readers do not
lock. A torn final line is ignored only by follow mode; a writer refuses it
rather than silently rewriting history. Back up and repair a damaged file
explicitly. A subsequent turn supplies interrupted results for unresolved tool
calls; it never blindly reruns a command that might already have had effects.

```sh
jq 'select(.type=="ply.approval")' work.jsonl
jq -r 'select(.type=="ply.usage") | .total_input' work.jsonl | tail -1

# Recover from an unwanted change:
ply -m 'Revert that change' work.jsonl
# Or --clear, or use a new transcript. There is no history-rewriting undo.

make test
make vet

# Replay recorded model responses through the actual harness:
ply --provider=replay:recorded.jsonl -m 'Repeat the scenario' replayed.jsonl
```

Replay mode consumes response groups separated by `ply.usage`, including saved
compaction summaries. It still executes tools and applies approval, so use a
scratch working directory and a suitable approver. Tests use temporary files,
local mock servers, and real subprocesses: no model access or external service
is required. Integration tests build their own copies of the executables. The interactive
approval regression uses Python 3’s standard-library `pty` module when Python 3
is installed; no additional Python packages are needed.

Code is organized in `internal/ply` around transcript replay, configuration,
provider transport, process supervision, approval, and rendering. Independent
companions live in `internal/companion`; `cmd/` contains their entry points.
Generated binaries, `.ply/tasks/`, and `.ply/out/` are gitignored. Task and output
files remain on disk for inspection; ply does not garbage-collect them.
