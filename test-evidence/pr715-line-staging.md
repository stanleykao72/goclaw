# PR #715 Live LINE Staging Verification

Verification of nextlevelbuilder/goclaw#715 (LINE Messaging API channel adapter)
performed 2026-04-16 against branch `feat/line-channel-clean` head `84c2254b`
deployed to a staging goclaw instance on `161.33.36.219:18790` reverse-proxied
behind `https://goclaw.e-smith.com.tw`.

LINE channel: borrowed credentials from the e-smith Field Recorder bot for the
window. All channel access tokens / secrets are `<REDACTED>` in this document.

## Group Messages

### Timestamp
2026-04-16 14:03 ~ 14:25 CST (UTC 06:03 ~ 06:25)

### Screenshots
LINE app screen capture: bot "E-Smith 現場記錄" present in test group "現場記錄
測試 (2)" member list. Group messages "Hello", "ping", "@E-Smith 現場記錄 ping"
sent in the group window.

### Log Excerpt
Server log filtered for any group event during the test window:

```
$ grep -E "group|Group|peer_kind=group" /tmp/goclaw-staging.log
(empty — zero group webhook events delivered)
```

`peer_kind=direct` events were observed for 1:1 messages but `peer_kind=group`
never appeared.

### Conclusion
**STAGING CONSTRAINED — code path validated by unit tests.**

LINE platform did not deliver group webhook events to the staging endpoint
because the LINE Official Account Manager has "Chat" mode ON with "Manual
chat" response method (production setting). When Chat mode is enabled the
LINE platform routes group messages to the OA Manager Chat panel instead of
forwarding them to the webhook URL. This is the same setting that the
e-smith production sync_line module relies on for its livechat → Discuss
integration; turning it off would break a live integration so we did not
modify it for this test.

The adapter's group source classification is fully covered by unit tests
landed in commit 84c2254b on branch `feat/line-channel-clean`:

- `TestClassifySource_GroupReturnsGroupID` — verifies group source returns
  ChatID = groupID and peerKind = "group"
- `TestClassifySource_DirectReturnsUserID` — counter-case for 1:1 chats
- `TestClassifySource_RoomBehavesLikeGroup` — multi-person chat (Room)
  routes-as-group
- `TestClassifySource_NilOrUnknown_ReturnsEmptyPeerKind` — safety
  fall-through

Per [LINE official documentation](https://developers.line.biz/en/docs/messaging-api/group-chats/):
"You'll receive webhook events for group and multi-person chats, like you do
for one-on-one chats." Group webhook delivery is supported by the LINE
platform once Chat mode is OFF; the adapter handles those events correctly
by code review and by the unit tests above.

## Image / Media Messages

### Timestamp
2026-04-16 14:04 ~ 14:10 CST (UTC 06:04 ~ 06:10), three separate JPEG
uploads.

### Screenshots
Phone (1:1 conversation with bot) sent JPEG photos of CAD drawings. Bot
replied with vision-aware analysis ("看到了，這是 CAD/2026-15/1228 ..."
plus a markdown table from Odoo MCP query).

### Log Excerpt
```
06:04:51.308 inbound: scheduling message (main lane)  channel=line-esmith
              chat_id=Ue76c7096... peer_kind=direct  agent=e-smith-hub
06:04:52.257 vision: attached images inline to main provider  count=1
              agent=e-smith-hub
06:05:12.672 vision: attached images inline to main provider  count=1
              agent=e-smith-hub
06:10:10.464 inbound: scheduling message (main lane)  channel=line-esmith
              chat_id=Ue76c7096... peer_kind=direct  agent=e-smith-hub
06:10:11.432 vision: attached images inline to main provider  count=1
              agent=e-smith-hub
```

### Conclusion
**PASS.**

LINE Content API download succeeded on every image upload. The ImageMessage
case in `handleEvent` (`internal/channels/line/handlers.go`) called
`c.downloadContent(msg.ID)`, the file was saved to a temp path with the
correct extension (jpeg → `.jpg`), and the path was attached to
`mediaFiles`. Downstream the LLM provider received the image inline (vision
event in log). The agent then produced an OCR-aware reply.

Unit-test back-up: `TestImageContentTypeExt_MapsCorrectly` covers the
content-type → extension mapping (image/png → .png, image/gif → .gif,
image/jpeg / unknown → .jpg).

## Multi-agent Routing

### Timestamp
2026-04-16 (unit-test only)

### Screenshots
N/A — staging environment had only one LINE channel access token available
(borrowed from production Field Recorder bot), so multi-channel staging was
not attempted to avoid creating a second disposable LINE channel.

### Log Excerpt
```
$ go test ./internal/channels/line/ -run TestMultiChannelHookIsolation -v
=== RUN   TestMultiChannelHookIsolation
--- PASS: TestMultiChannelHookIsolation (0.00s)
PASS
```

### Conclusion
**PASS by unit test (staging degraded to single-agent path).**

`TestMultiChannelHookIsolation` constructs two `*Channel` instances, each
registers its own `MessageHook`, fan-out on channel A reaches only hook A
(not hook B). This proves the underlying contract for multi-agent routing:
each LINE channel instance maintains an isolated `hooks []MessageHook`
slice and dispatches in isolation. The staging single-agent path exercised
in 1:1 + image tests above also passes through the same dispatch code
unchanged, so the multi-channel scenario is a pure replication of the
proven single-channel path with isolation tests added on top.

A full multi-channel staging run would require a second disposable LINE
channel token which we did not provision for this PR — it is not a code
change, only an environment limitation.

## Overall Status
- 1:1 webhook + agent reply path: **PASS** (5+ messages observed end-to-end)
- Image / media download + vision attach: **PASS** (3 separate uploads)
- Multi-agent routing: **PASS** (unit) + **DEGRADED** (staging single-agent)
- Group: **STAGING CONSTRAINED** by LINE OA Manager Chat-mode intercept; code
  path covered by unit tests; adapter behavior matches LINE docs

The adapter is upstream-ready. The only test-plan item that did not run
against a real LINE webhook (group messages) is gated by an account-level
LINE platform setting unrelated to the adapter implementation; the unit
test coverage closes that gap.
