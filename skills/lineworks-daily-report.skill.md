---
name: lineworks-daily-report
description: >-
  LINE WORKS 日報（daily report）流程。當使用者在 LINE WORKS 以「日報 / daily / 回報」開頭傳訊息時，
  以 Odoo MCP 自動帶入、建立並提交當日日報。對應 internal/plugins/lineworks-workflow
  的 runDailyLogFlow。觸發詞：日報、daily、回報。
channel: lineworks
mcp_tools:
  - autofill_daily_log
  - create_daily_log
  - submit_daily_log
  - upload_field_photo
---

# LINE WORKS 日報

## 何時觸發

使用者透過 LINE WORKS Bot 傳來的「訊息」（message callback，content.type=text）以下列任一關鍵字**開頭**（不分大小寫）：

- `日報`
- `daily`
- `回報`

關鍵字後可接全形/半形冒號、空白或破折號，其餘文字作為日報補充備註（note，可空）。

範例輸入：

```
日報
日報 今天完成 3F 水電配管
回報：下午客戶驗收通過
```

## 身分解析（前置）

與 lineworks-todo 相同，先把 `source.userId` 對應到 Odoo `res.users` uid：

1. Directory `GET /users/{userId}` → `userExternalKey`（`odoo-emp-{hr_employee.id}`）。
2. 由 prefix 解出 `hr_employee.id`。
3. MCP `search_records` `res.users` where `employee_id = {empId}` → uid，並快取。

解析失敗回覆：
`無法對應你的 Odoo 帳號，請確認 LINE WORKS 目錄已綁定員工編號（externalKey），或聯絡系統管理員。`

## 流程

1. **autofill_daily_log** — 先讓 Odoo 依當日工時/排程自動帶入日報內容：

   ```json
   { "employee_id": <empId> }
   ```

2. **create_daily_log** — 建立當日日報（帶入第 1 步的內容；若使用者有附加 note，併入備註）。回傳日報 `id`；無 id 視為失敗。

3. **submit_daily_log** — 提交日報，並依結構化結果碼處理：

   - `already_submitted` → 告知今日日報已提交。
   - `timesheet_required` → 提示需先填工時。
   - `access_denied` → 提示無權限／聯絡管理員。
   - 其他錯誤 → 回覆 `處理時發生錯誤，請稍後再試。`

4. **upload_field_photo（選用）** — 若訊息夾帶照片，逐張以 base64 上傳：

   ```json
   { "daily_log_id": <logId>, "filename": "<name>", "data_base64": "<...>" }
   ```

## 回覆通道

- 1:1（`source.channelId` 空）→ `POST /bots/{botId}/users/{userId}/messages`
- 群組（`source.channelId` 非空）→ `POST /bots/{botId}/channels/{channelId}/messages`

純文字，單則 <= 2000 字。

## 邊界

- 非關鍵字開頭：flowNone，不處理。
- postback（top-level `data`）：`action=daily_log` 等於觸發本流程；未知 data 記 log 後忽略。
- 任一 MCP 步驟失敗：回覆 `處理時發生錯誤，請稍後再試。`，log 細節。
