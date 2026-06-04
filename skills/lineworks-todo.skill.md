---
name: lineworks-todo
description: >-
  LINE WORKS 待辦（todo）建立流程。當使用者在 LINE WORKS 以「待辦 / todo / 代辦」開頭傳訊息時，
  將其餘文字建立成 Odoo 的個人待辦（project.task）。對應 internal/plugins/lineworks-workflow
  的 runTodoFlow。觸發詞：待辦、todo、代辦。
channel: lineworks
mcp_tools:
  - create_my_todo
---

# LINE WORKS 待辦建立

## 何時觸發

使用者透過 LINE WORKS Bot 傳來的「訊息」（message callback，content.type=text）以下列任一關鍵字**開頭**（不分大小寫）：

- `待辦`
- `todo`
- `代辦`

關鍵字後可接全形/半形冒號、空白或破折號作為分隔，其餘文字即為待辦標題。

範例輸入：

```
待辦 巡檢 3F 配電盤
todo: 回覆業主估價單
待辦：採購水泥 50 包
```

## 身分解析（前置）

在執行任何 MCP 寫入前，必須先把 LINE WORKS 的 `source.userId`（resource ID）對應到 Odoo `res.users` uid：

1. LINE WORKS Directory `GET /users/{userId}` → 取 `userExternalKey`（格式 `odoo-emp-{hr_employee.id}`，prefix 由 `directory_external_key_prefix` 設定，預設 `odoo-emp-`）。
2. 由 prefix 解出 `hr_employee.id`。
3. Odoo MCP `search_records`：`res.users` where `employee_id = {empId}`，取 uid。
4. 結果以 (userId → uid) 在記憶體快取，TTL 預設 30 分鐘。

解析失敗時，回覆綁定提示，不要靜默丟棄：

```
無法對應你的 Odoo 帳號，請確認 LINE WORKS 目錄已綁定員工編號（externalKey），或聯絡系統管理員。
```

## 流程

1. 取出關鍵字後的標題。標題為空 → 回覆使用方式提示：
   `請在「待辦」後面加上要新增的待辦事項，例如：待辦 巡檢 3F 配電盤`
2. 若未設定預設專案（`TodoProjectID` / 環境變數 `LINEWORKS_WORKFLOW_TODO_PROJECT_ID`）→ 回覆：
   `尚未設定待辦預設專案（TodoProjectID），請聯絡系統管理員。`
3. 呼叫 MCP 工具 `create_my_todo`：

   ```json
   {
     "name": "<待辦標題>",
     "project_id": <TodoProjectID>
   }
   ```

4. 成功（回傳 `task_id` 非 0）→ 回覆建立成功與標題；`task_id == 0` → 回覆 `待辦建立失敗，請稍後再試。`

## 回覆通道

- 1:1（`source.channelId` 為空）→ 回給 user：`POST /bots/{botId}/users/{userId}/messages`
- 群組/房間（`source.channelId` 非空）→ 回給 channel：`POST /bots/{botId}/channels/{channelId}/messages`

訊息為純文字，單則 <= 2000 字。

## 邊界

- 非以上關鍵字開頭的訊息：本流程回傳 flowNone，不處理（與 agent 路徑共存）。
- 錯誤（MCP 失敗等）統一回覆：`處理時發生錯誤，請稍後再試。`，並於 log 記錄細節。
