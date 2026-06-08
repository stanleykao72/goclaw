package lineworks

import "fmt"

// This file holds the localization layer for the LINE WORKS channel's fixed
// command replies. Agent (non-command) replies are NOT touched here — they flow
// through the agent pipeline untranslated. Only the deterministic strings the
// channel itself emits (help/status text, reset/stop acks, the writer gate, and
// every Tier 2 admin literal) are routed through localize().
//
// Supported languages: "en" (canonical / fallback), "zh_TW" (Traditional
// Chinese), "vi_VN" (Vietnamese). Native review of zh_TW / vi_VN is a followup.

// Stable string keys. Every fixed reply the channel emits has exactly one key.
// Keys are referenced from commands.go and commands_admin.go via localize(...).
// Parameterized strings carry %s/%d placeholders applied through fmt.Sprintf.
const (
	// --- Tier 1: commands.go ---
	keyHelp                = "help"                  // full /help body
	keyStatusLine          = "status.line"           // "Bot status: %s\nChannel: %s\nBot name(s): %s"
	keyStatusRunning       = "status.running"        // "running"
	keyStatusStopped       = "status.stopped"        // "stopped"
	keyStatusNamesUnresolved = "status.names_unresolved" // "(unresolved — group mention gating disabled)"
	keyResetDone           = "reset.done"            // /reset + /new ack
	keyResetWriterGate     = "reset.writer_gate"     // only file writers may reset
	keyStopOne             = "stop.one"              // /stop ack
	keyStopAll             = "stop.all"              // /stopall ack

	// --- Tier 2: commands_admin.go (writers) ---
	keyWriterGroupOnly        = "writer.group_only"         // "This command only works in group chats."
	keyWriterUnavailable      = "writer.unavailable"        // "File writer management is not available."
	keyWriterUnavailableAgent = "writer.unavailable_agent"  // "...not available (no agent)."
	keyWriterOnlyWriters      = "writer.only_writers"       // "Only existing file writers can manage the writer list."
	keyWriterNoneYet          = "writer.none_yet"           // "No file writers configured yet. Use /addwriter to add the first one."
	keyWriterUsage            = "writer.usage"              // "Usage: /%swriter <userId>"
	keyWriterAddFailed        = "writer.add_failed"         // "Failed to add writer. Please try again."
	keyWriterAdded            = "writer.added"              // "Added %s as a file writer."
	keyWriterRemoveLast       = "writer.remove_last"        // "Cannot remove the last file writer."
	keyWriterRemoveFailed     = "writer.remove_failed"      // "Failed to remove writer. Please try again."
	keyWriterRemoved          = "writer.removed"            // "Removed %s from file writers."
	keyWriterListFailed       = "writer.list_failed"        // "Failed to list writers. Please try again."
	keyWriterListEmpty        = "writer.list_empty"         // "No file writers configured for this group. Use /addwriter to add one."
	keyWriterListHeader       = "writer.list_header"        // "File writers for this group (%d):\n"
	keyWriterListRow          = "writer.list_row"           // "%d. %s (ID: %s)\n"

	// --- Tier 2: commands_admin.go (tasks) ---
	keyTeamUnavailable        = "team.unavailable"          // "Team features are not available."
	keyTeamUnavailableAgent   = "team.unavailable_agent"    // "Team features are not available (no agent)."
	keyTeamLookupFailed       = "team.lookup_failed"        // "Failed to look up team. Please try again."
	keyTeamNotInTeam          = "team.not_in_team"          // "This agent is not part of any team."
	keyTaskListFailed         = "task.list_failed"          // "Failed to list tasks. Please try again."
	keyTaskNoneForTeam        = "task.none_for_team"        // "No tasks for team %q."
	keyTaskListHeaderTrunc    = "task.list_header_trunc"    // "Tasks for team %q (showing %d of %d):\n\n"
	keyTaskListHeader         = "task.list_header"          // "Tasks for team %q (%d):\n\n"
	keyTaskListFooter         = "task.list_footer"          // "\nUse /task_detail <id> to view a task."
	keyTaskDetailUsage        = "task.detail_usage"         // "Usage: /task_detail <task_id>"
	keyTaskNotFound           = "task.not_found"            // "Task %q not found. Use /tasks to see available tasks."

	// --- Tier 2: commands_admin.go (subagents) ---
	keySubUnavailable         = "subagent.unavailable"      // "Subagent task tracking is not available."
	keySubUnavailableAgent    = "subagent.unavailable_agent" // "Subagent tasks are not available (no agent configured)."
	keySubListFailed          = "subagent.list_failed"      // "Failed to list subagent tasks. Please try again."
	keySubNone                = "subagent.none"             // "No subagent tasks found."
	keySubListHeaderTrunc     = "subagent.list_header_trunc" // "Subagent tasks (showing %d of %d):\n\n"
	keySubListHeader          = "subagent.list_header"      // "Subagent tasks (%d):\n\n"
	keySubListFooter          = "subagent.list_footer"      // "\nUse /subagent <id> to view a task."
	keySubDetailUsage         = "subagent.detail_usage"     // "Usage: /subagent <task_id>"
	keySubInvalidID           = "subagent.invalid_id"       // "Invalid task ID %q. Use /subagents to list tasks."
	keySubLoadFailed          = "subagent.load_failed"      // "Failed to load subagent task. Please try again."
	keySubNotFound            = "subagent.not_found"        // "Task %q not found. Use /subagents to see available tasks."
)

// langEN / langZH / langVI are the canonical language codes.
const (
	langEN = "en"
	langZH = "zh_TW"
	langVI = "vi_VN"
)

// helpEN / helpZH / helpVI are the full /help bodies, kept as named values so
// the multi-line content does not clutter the map literal.
const helpEN = "Available commands:\n" +
	"/new — Reset conversation history\n" +
	"/reset — Reset conversation history\n" +
	"/stop — Stop the current running task\n" +
	"/stopall — Stop all running tasks\n" +
	"/help — Show this help message\n" +
	"/status — Show bot status\n" +
	"\nAdmin commands:\n" +
	"/addwriter <userId> — Add a file writer (group)\n" +
	"/removewriter <userId> — Remove a file writer (group)\n" +
	"/writers — List file writers (group)\n" +
	"/tasks — List team tasks\n" +
	"/task_detail <id> — Show a team task\n" +
	"/subagents — List subagent tasks\n" +
	"/subagent <id> — Show a subagent task\n" +
	"\nJust send a message to chat with the AI."

const helpZH = "可用指令:\n" +
	"/new — 重設對話紀錄\n" +
	"/reset — 重設對話紀錄\n" +
	"/stop — 停止目前執行中的任務\n" +
	"/stopall — 停止所有執行中的任務\n" +
	"/help — 顯示此說明訊息\n" +
	"/status — 顯示機器人狀態\n" +
	"\n管理指令:\n" +
	"/addwriter <userId> — 新增檔案寫入者(群組)\n" +
	"/removewriter <userId> — 移除檔案寫入者(群組)\n" +
	"/writers — 列出檔案寫入者(群組)\n" +
	"/tasks — 列出團隊任務\n" +
	"/task_detail <id> — 顯示團隊任務\n" +
	"/subagents — 列出子代理任務\n" +
	"/subagent <id> — 顯示子代理任務\n" +
	"\n直接傳送訊息即可與 AI 對話。"

const helpVI = "Các lệnh khả dụng:\n" +
	"/new — Đặt lại lịch sử hội thoại\n" +
	"/reset — Đặt lại lịch sử hội thoại\n" +
	"/stop — Dừng tác vụ đang chạy hiện tại\n" +
	"/stopall — Dừng tất cả tác vụ đang chạy\n" +
	"/help — Hiển thị thông báo trợ giúp này\n" +
	"/status — Hiển thị trạng thái bot\n" +
	"\nLệnh quản trị:\n" +
	"/addwriter <userId> — Thêm người ghi tệp (nhóm)\n" +
	"/removewriter <userId> — Xóa người ghi tệp (nhóm)\n" +
	"/writers — Liệt kê người ghi tệp (nhóm)\n" +
	"/tasks — Liệt kê tác vụ nhóm\n" +
	"/task_detail <id> — Hiển thị tác vụ nhóm\n" +
	"/subagents — Liệt kê tác vụ subagent\n" +
	"/subagent <id> — Hiển thị tác vụ subagent\n" +
	"\nChỉ cần gửi tin nhắn để trò chuyện với AI."

// commandStrings maps lang → string-key → translated text. The "en" inner map
// is the authoritative key set: localize falls back to it, and the test suite
// asserts every other language defines exactly the same keys.
var commandStrings = map[string]map[string]string{
	langEN: {
		keyHelp:                  helpEN,
		keyStatusLine:            "Bot status: %s\nChannel: %s\nBot name(s): %s",
		keyStatusRunning:         "running",
		keyStatusStopped:         "stopped",
		keyStatusNamesUnresolved: "(unresolved — group mention gating disabled)",
		keyResetDone:             "Conversation history has been reset.",
		keyResetWriterGate:       "Only file writers can reset conversation history in this group.",
		keyStopOne:               "Stopping current task…",
		keyStopAll:               "Stopping all tasks…",

		keyWriterGroupOnly:        "This command only works in group chats.",
		keyWriterUnavailable:      "File writer management is not available.",
		keyWriterUnavailableAgent: "File writer management is not available (no agent).",
		keyWriterOnlyWriters:      "Only existing file writers can manage the writer list.",
		keyWriterNoneYet:          "No file writers configured yet. Use /addwriter to add the first one.",
		keyWriterUsage:            "Usage: /%swriter <userId>",
		keyWriterAddFailed:        "Failed to add writer. Please try again.",
		keyWriterAdded:            "Added %s as a file writer.",
		keyWriterRemoveLast:       "Cannot remove the last file writer.",
		keyWriterRemoveFailed:     "Failed to remove writer. Please try again.",
		keyWriterRemoved:          "Removed %s from file writers.",
		keyWriterListFailed:       "Failed to list writers. Please try again.",
		keyWriterListEmpty:        "No file writers configured for this group. Use /addwriter to add one.",
		keyWriterListHeader:       "File writers for this group (%d):\n",
		keyWriterListRow:          "%d. %s (ID: %s)\n",

		keyTeamUnavailable:      "Team features are not available.",
		keyTeamUnavailableAgent: "Team features are not available (no agent).",
		keyTeamLookupFailed:     "Failed to look up team. Please try again.",
		keyTeamNotInTeam:        "This agent is not part of any team.",
		keyTaskListFailed:       "Failed to list tasks. Please try again.",
		keyTaskNoneForTeam:      "No tasks for team %q.",
		keyTaskListHeaderTrunc:  "Tasks for team %q (showing %d of %d):\n\n",
		keyTaskListHeader:       "Tasks for team %q (%d):\n\n",
		keyTaskListFooter:       "\nUse /task_detail <id> to view a task.",
		keyTaskDetailUsage:      "Usage: /task_detail <task_id>",
		keyTaskNotFound:         "Task %q not found. Use /tasks to see available tasks.",

		keySubUnavailable:      "Subagent task tracking is not available.",
		keySubUnavailableAgent: "Subagent tasks are not available (no agent configured).",
		keySubListFailed:       "Failed to list subagent tasks. Please try again.",
		keySubNone:             "No subagent tasks found.",
		keySubListHeaderTrunc:  "Subagent tasks (showing %d of %d):\n\n",
		keySubListHeader:       "Subagent tasks (%d):\n\n",
		keySubListFooter:       "\nUse /subagent <id> to view a task.",
		keySubDetailUsage:      "Usage: /subagent <task_id>",
		keySubInvalidID:        "Invalid task ID %q. Use /subagents to list tasks.",
		keySubLoadFailed:       "Failed to load subagent task. Please try again.",
		keySubNotFound:         "Task %q not found. Use /subagents to see available tasks.",
	},
	langZH: {
		keyHelp:                  helpZH,
		keyStatusLine:            "機器人狀態:%s\n頻道:%s\n機器人名稱:%s",
		keyStatusRunning:         "執行中",
		keyStatusStopped:         "已停止",
		keyStatusNamesUnresolved: "(未解析 — 群組提及守門已停用)",
		keyResetDone:             "已重設對話紀錄。",
		keyResetWriterGate:       "只有檔案寫入者才能重設此群組的對話紀錄。",
		keyStopOne:               "正在停止目前的任務…",
		keyStopAll:               "正在停止所有任務…",

		keyWriterGroupOnly:        "此指令僅能在群組聊天中使用。",
		keyWriterUnavailable:      "檔案寫入者管理功能無法使用。",
		keyWriterUnavailableAgent: "檔案寫入者管理功能無法使用(無代理)。",
		keyWriterOnlyWriters:      "只有現有的檔案寫入者才能管理寫入者清單。",
		keyWriterNoneYet:          "尚未設定任何檔案寫入者。請使用 /addwriter 新增第一位。",
		keyWriterUsage:            "用法:/%swriter <userId>",
		keyWriterAddFailed:        "新增寫入者失敗。請再試一次。",
		keyWriterAdded:            "已將 %s 新增為檔案寫入者。",
		keyWriterRemoveLast:       "無法移除最後一位檔案寫入者。",
		keyWriterRemoveFailed:     "移除寫入者失敗。請再試一次。",
		keyWriterRemoved:          "已將 %s 從檔案寫入者中移除。",
		keyWriterListFailed:       "列出寫入者失敗。請再試一次。",
		keyWriterListEmpty:        "此群組尚未設定任何檔案寫入者。請使用 /addwriter 新增。",
		keyWriterListHeader:       "此群組的檔案寫入者(%d):\n",
		keyWriterListRow:          "%d. %s(ID:%s)\n",

		keyTeamUnavailable:      "團隊功能無法使用。",
		keyTeamUnavailableAgent: "團隊功能無法使用(無代理)。",
		keyTeamLookupFailed:     "查詢團隊失敗。請再試一次。",
		keyTeamNotInTeam:        "此代理不屬於任何團隊。",
		keyTaskListFailed:       "列出任務失敗。請再試一次。",
		keyTaskNoneForTeam:      "團隊 %q 沒有任務。",
		keyTaskListHeaderTrunc:  "團隊 %q 的任務(顯示 %d / %d 筆):\n\n",
		keyTaskListHeader:       "團隊 %q 的任務(%d):\n\n",
		keyTaskListFooter:       "\n使用 /task_detail <id> 檢視任務。",
		keyTaskDetailUsage:      "用法:/task_detail <task_id>",
		keyTaskNotFound:         "找不到任務 %q。請使用 /tasks 查看可用任務。",

		keySubUnavailable:      "子代理任務追蹤功能無法使用。",
		keySubUnavailableAgent: "子代理任務無法使用(未設定代理)。",
		keySubListFailed:       "列出子代理任務失敗。請再試一次。",
		keySubNone:             "找不到子代理任務。",
		keySubListHeaderTrunc:  "子代理任務(顯示 %d / %d 筆):\n\n",
		keySubListHeader:       "子代理任務(%d):\n\n",
		keySubListFooter:       "\n使用 /subagent <id> 檢視任務。",
		keySubDetailUsage:      "用法:/subagent <task_id>",
		keySubInvalidID:        "無效的任務 ID %q。請使用 /subagents 列出任務。",
		keySubLoadFailed:       "載入子代理任務失敗。請再試一次。",
		keySubNotFound:         "找不到任務 %q。請使用 /subagents 查看可用任務。",
	},
	langVI: {
		keyHelp:                  helpVI,
		keyStatusLine:            "Trạng thái bot: %s\nKênh: %s\nTên bot: %s",
		keyStatusRunning:         "đang chạy",
		keyStatusStopped:         "đã dừng",
		keyStatusNamesUnresolved: "(chưa xác định — đã tắt kiểm soát nhắc tên trong nhóm)",
		keyResetDone:             "Lịch sử hội thoại đã được đặt lại.",
		keyResetWriterGate:       "Chỉ người ghi tệp mới có thể đặt lại lịch sử hội thoại trong nhóm này.",
		keyStopOne:               "Đang dừng tác vụ hiện tại…",
		keyStopAll:               "Đang dừng tất cả tác vụ…",

		keyWriterGroupOnly:        "Lệnh này chỉ hoạt động trong các cuộc trò chuyện nhóm.",
		keyWriterUnavailable:      "Quản lý người ghi tệp không khả dụng.",
		keyWriterUnavailableAgent: "Quản lý người ghi tệp không khả dụng (không có agent).",
		keyWriterOnlyWriters:      "Chỉ những người ghi tệp hiện có mới có thể quản lý danh sách người ghi.",
		keyWriterNoneYet:          "Chưa có người ghi tệp nào được cấu hình. Dùng /addwriter để thêm người đầu tiên.",
		keyWriterUsage:            "Cách dùng: /%swriter <userId>",
		keyWriterAddFailed:        "Không thể thêm người ghi. Vui lòng thử lại.",
		keyWriterAdded:            "Đã thêm %s làm người ghi tệp.",
		keyWriterRemoveLast:       "Không thể xóa người ghi tệp cuối cùng.",
		keyWriterRemoveFailed:     "Không thể xóa người ghi. Vui lòng thử lại.",
		keyWriterRemoved:          "Đã xóa %s khỏi danh sách người ghi tệp.",
		keyWriterListFailed:       "Không thể liệt kê người ghi. Vui lòng thử lại.",
		keyWriterListEmpty:        "Chưa có người ghi tệp nào cho nhóm này. Dùng /addwriter để thêm.",
		keyWriterListHeader:       "Người ghi tệp của nhóm này (%d):\n",
		keyWriterListRow:          "%d. %s (ID: %s)\n",

		keyTeamUnavailable:      "Tính năng nhóm không khả dụng.",
		keyTeamUnavailableAgent: "Tính năng nhóm không khả dụng (không có agent).",
		keyTeamLookupFailed:     "Không thể tra cứu nhóm. Vui lòng thử lại.",
		keyTeamNotInTeam:        "Agent này không thuộc nhóm nào.",
		keyTaskListFailed:       "Không thể liệt kê tác vụ. Vui lòng thử lại.",
		keyTaskNoneForTeam:      "Không có tác vụ cho nhóm %q.",
		keyTaskListHeaderTrunc:  "Tác vụ cho nhóm %q (hiển thị %d / %d):\n\n",
		keyTaskListHeader:       "Tác vụ cho nhóm %q (%d):\n\n",
		keyTaskListFooter:       "\nDùng /task_detail <id> để xem tác vụ.",
		keyTaskDetailUsage:      "Cách dùng: /task_detail <task_id>",
		keyTaskNotFound:         "Không tìm thấy tác vụ %q. Dùng /tasks để xem các tác vụ khả dụng.",

		keySubUnavailable:      "Theo dõi tác vụ subagent không khả dụng.",
		keySubUnavailableAgent: "Tác vụ subagent không khả dụng (chưa cấu hình agent).",
		keySubListFailed:       "Không thể liệt kê tác vụ subagent. Vui lòng thử lại.",
		keySubNone:             "Không tìm thấy tác vụ subagent nào.",
		keySubListHeaderTrunc:  "Tác vụ subagent (hiển thị %d / %d):\n\n",
		keySubListHeader:       "Tác vụ subagent (%d):\n\n",
		keySubListFooter:       "\nDùng /subagent <id> để xem tác vụ.",
		keySubDetailUsage:      "Cách dùng: /subagent <task_id>",
		keySubInvalidID:        "ID tác vụ không hợp lệ %q. Dùng /subagents để liệt kê tác vụ.",
		keySubLoadFailed:       "Không thể tải tác vụ subagent. Vui lòng thử lại.",
		keySubNotFound:         "Không tìm thấy tác vụ %q. Dùng /subagents để xem các tác vụ khả dụng.",
	},
}

// localize resolves key in the given language, falling back to English, then to
// the raw key itself when no translation exists at all. fmt.Sprintf is applied
// only when args are supplied (so format strings with literal %-signs and no
// args are returned verbatim).
func localize(lang, key string, args ...any) string {
	s, ok := commandStrings[lang][key]
	if !ok {
		if s, ok = commandStrings[langEN][key]; !ok {
			s = key
		}
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

// normalizeLang maps an Odoo res.users.lang value to one of the supported
// command languages. Anything unrecognized (including empty) collapses to "en".
func normalizeLang(odooLang string) string {
	switch odooLang {
	case "en_US", langEN:
		return langEN
	case langZH:
		return langZH
	case langVI:
		return langVI
	default:
		return langEN
	}
}

// supportedLangs returns the language codes commandStrings defines, for tests
// that assert every language shares the canonical (English) key set.
func supportedLangs() []string {
	out := make([]string, 0, len(commandStrings))
	for l := range commandStrings {
		out = append(out, l)
	}
	return out
}
