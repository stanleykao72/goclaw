// Package lineworksworkflow is the LINE WORKS todo / daily-log workflow
// plugin for goclaw. It is the LINE WORKS counterpart to
// internal/plugins/esmith-km (which serves the consumer-LINE channel):
// it implements the lineworks channel's MessageHook so that bot callback
// events drive Odoo writeback through the Odoo MCP server.
//
// # What it does
//
//   - Todo flow: a "待辦" / "todo" style text message creates a project.task
//     assigned to the sending user via the Odoo MCP tool create_my_todo.
//   - Daily-log flow: a "日報" / "daily" style text message runs
//     autofill_daily_log → create_daily_log → (optional) upload_field_photo
//     → submit_daily_log. The flow is idempotent per calendar day: a second
//     attempt the same day reuses the existing draft / surfaces the
//     already_submitted result instead of creating a duplicate.
//
// # Identity resolution
//
// Callbacks carry only source.userId (a LINE WORKS resource ID). The plugin
// resolves it to an Odoo res.users id via resolve.go:
//
//  1. LINE WORKS Directory: GET /users/{userId} → userExternalKey, expected
//     to be "odoo-emp-{hr_employee.id}" (prefix configurable).
//  2. Parse the employee id out of the externalKey.
//  3. Odoo MCP search_records on res.users where employee_id = empId → uid.
//
// Resolved identities are cached in memory with a TTL. When no Odoo identity
// can be resolved the plugin replies with a configuration hint rather than
// silently dropping the message.
//
// # Dependency direction
//
// This package imports internal/channels/lineworks ONLY for the interface
// types (MessageHook, TextEvent, PostbackEvent, Sender, Lifecycle) and
// internal/lineworks for the Directory client. The dependency arrow is
// always plugin → channel/SDK, never the other way around. It does NOT
// import internal/channels/line and does NOT touch any consumer-LINE
// behaviour.
//
// # MCP transport
//
// All Odoo writes go through a JSON-RPC tools/call to the Odoo MCP HTTP
// endpoint (config MCPURL + MCPToken), mirroring esmith-km's mcpToolCall.
// The plugin never talks to the Odoo ORM directly.
package lineworksworkflow
