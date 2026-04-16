# PR #714 CLI Smoke Test

Verification of nextlevelbuilder/goclaw#714 (`fix(cmd): add --user flag to
agent chat for user_id authentication`) performed 2026-04-16 against branch
`fix/cli-chat-user-id` head `8f78f19f` built on
`/home/ubuntu/goclaw-fork/` and run as `/tmp/goclaw-714` against the same
gateway used for PR #715 staging (`localhost:18790`).

## --user testuser success path

### Timestamp
2026-04-16 14:42 CST (UTC 06:42)

### Command
```bash
/tmp/goclaw-714 agent chat --name e-smith-hub --user testuser \
    -m "ping from PR714 smoke test scenario 1"
```

### Output
```
Connected to gateway at 127.0.0.1:18790
Hey! Pong 🏓

I just came online — fresh start, no memory yet. Who am I? Who are you?
```
exit code: 0

### Conclusion
**PASS.** Agent connected, responded; no `user_id is required` error.
`--user testuser` propagates as expected through the WebSocket connect
frame.

## No --user (backward-compat fail)

### Timestamp
2026-04-16 14:42 CST (UTC 06:42)

### Command
```bash
/tmp/goclaw-714 agent chat --name e-smith-hub -m "ping from scenario 2"
```

### Output
```
Connected to gateway at 127.0.0.1:18790
Error: agent error: user_id is required
```

### Conclusion
**PASS** with one observation. The expected error message surfaces and the
gateway rejects the request — confirming no silent default was injected.
Note: the binary today exits with status 0 even when stderr prints
"Error: ...". For scripting use the non-zero exit would be cleaner, but
that is independent of this PR's user_id-propagation scope.

## Interactive REPL with --user across multiple messages

### Timestamp
2026-04-16 14:43 CST (UTC 06:43)

### Command
```bash
printf "first message\nsecond message\nthird message\n" | \
    /tmp/goclaw-714 agent chat --name e-smith-hub --user testuser
```

### Output
```
Connected to gateway at 127.0.0.1:18790

GoClaw Interactive Chat (agent: e-smith-hub, model: openai-codex/gpt-4.1)
Session: agent:e-smith-hub:cli:direct:local
Type "exit" to quit, "/new" for new session

You: <agent reply 1: "Hey! Glad you're here. ...">
You: <agent reply 2: "Ha, okay — are you testing something ...">
You: <agent reply 3: "Still here, still nameless. ...">
You: exit code: 0
```

### Conclusion
**PASS.** REPL initialised, the per-session ID
`agent:e-smith-hub:cli:direct:local` remained stable across all three
messages, no `user_id is required` errors, and each round-trip received
an agent reply. user_id propagates correctly through the persistent WS
session.

## Overall Status
All three test plan items pass. PR #714 is ready for upstream review.
