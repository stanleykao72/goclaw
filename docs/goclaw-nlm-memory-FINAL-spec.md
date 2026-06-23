# goclaw × NotebookLM 記憶系統 — 最終設計 (Phase 2/3 權威 spec)

> 狀態:設計定稿、前置全確認。Phase 0(nlm 基座)+ Phase 1(notebook_recall)已上線驗證。
> 本文定義 Phase 2(ingestion)+ Phase 3(per-scope 隔離)的完整規則。

## 1. 核心模型:4 層 notebook(混合)

| 層 scope_kind | scope_id | 誰**讀** | 誰**寫** | 用途 |
|---------------|----------|---------|---------|------|
| `shared` | `""`(每 tenant 1 個) | 所有 agent + user | `remember_shared` 工具(策展) | 公司公共知識 |
| `user` | `<lineworksUserId>` | 該 user(跨 agent) | 該 user 的 **DM 對話**(自動) | 個人記憶 |
| `agent` | `<agentKey>` | 該 agent(跨 user) | `remember_agent` 工具(策展) | agent 領域知識 |
| `group` | `<chatId>` | 該群成員 | 該群**對話**(自動) | 群組記憶 |

**隱私鐵律**:原始 DM 私密內容**只**進 `user-<id>`,**不**自動進 `agent`/`shared`(否則跨 user recall 會洩漏)。`agent`/`shared` 只收策展工具寫入的非私密知識。

## 2. 儲存:Strategy A — Drive Doc per scope

```
每 scope = 1 個 Google Doc(Drive)  ──加為 --drive source──>  1 個 NotebookLM notebook
對話 append 進 Doc → nlm source sync 刷新 notebook
```
- Doc 永遠 1 個/scope(不爆 source 上限)、可無限長、人可查看、durable、離 VPS。
- 建/append Doc 用 **Google Docs API**(create + batchUpdate insertText),auth 重用 **rclone gdrive 的 OAuth refresh token**(帳號 = stanleykao72 = nlm 帳號;drive scope 涵蓋 Docs;**免新 OAuth 同意**)。
- rclone 用於 Drive 檔案操作備援;Doc 內容編輯走 Docs API。

## 3. Pointer table(goclaw DB,唯一持久狀態)

```sql
nlm_notebooks(
  tenant_id uuid, scope_kind text, scope_id text,
  notebook_id text, drive_doc_id text, created_at timestamptz,
  UNIQUE(tenant_id, scope_kind, scope_id)   -- 防重複建立 / race
)
```

## 4. 建立規則:lazy first-write

```
ingest 收到某 scope 內容:
  pointer 命中 → 用既有 notebook_id + drive_doc_id
  未命中 → 建 Google Doc → 建 notebook → nlm source add --drive <docId> --type doc
          → INSERT pointer ON CONFLICT DO NOTHING(race-safe)
recall 時 scope 無 pointer → 跳過該層(不建空 notebook)
shared/agent 層 → 由策展工具首次寫入時建立
```

## 5. Ingestion 路由

| 來源 | 捕捉機制 | → 寫入 |
|------|---------|--------|
| **群組訊息** | **複用既有 `channel_pending_messages`**(已 live,11+ 筆) | `group-<chatId>` Doc |
| **DM 訊息** | inbound handler / EventSessionCompleted(新) | `user-<userId>` Doc |
| **Agent 知識** | `remember_agent` 工具 | `agent-<agentKey>` Doc |
| **共用知識** | `remember_shared` 工具 | `shared` Doc |

- Webhook 必須快回 200 → 捕捉只入 buffer,Drive/nlm 全在背景 worker(仿 `channelmemory.Worker` ticker drain)。
- 群組:nlm worker 當 `channel_pending_messages` 唯一消費者(channelmemory 目前 disabled),用自己的 high-water 讀取,不刪除。
- 批次:每 drain window 把該 scope 累積訊息 append 進 Doc 一次 → `nlm source sync`。high-water 防重複 ingest。

## 6. Recall:跨層 union(notebook_recall 升級)

```
user U / agent A / (DM 或 group G):
  notebook set = [shared] + [user-U] + [agent-A]  (+ [group-G] 若在群)
  → nlm cross query 一次查全部 → 合併 grounded 答案
```
- schema 仍只有 `question`;notebook set 由**注入身分**伺服端解析(LLM 不能指定)→ 隔離保持。
- scope 無對應 notebook 就排除該層;全空 → fail-soft「尚無記憶」。

## 7. 新增策展工具

- `remember_shared(content)` → 寫 `shared` Doc(+建 notebook if first)。所有 agent 可呼叫。
- `remember_agent(content)` → 寫 `agent-<callerAgentKey>` Doc。agentKey 從 ctx 身分取(非 LLM 參數)。

## 8. 前置(全確認)

- ✅ 帳號:rclone gdrive == nlm == stanleykao72@gmail.com
- ✅ Drive 寫入:rclone 5TiB;OAuth refresh token 可重用呼叫 Docs API(免新同意)
- ✅ 群組捕捉:channel_pending_messages 已 live
- ✅ Phase 0(nlm 基座)+ Phase 1(notebook_recall recall)已驗證

## 9. 實作分期

| 子階段 | 內容 |
|--------|------|
| **2.1 Drive Doc 庫** | `internal/gdrive` or `internal/nlmdoc`:用 rclone token 呼叫 Docs API(create doc、append text、resolve doc id)+ rclone fallback |
| **2.2 Pointer + resolver** | `nlm_notebooks` table + 共用 `resolveNotebookSet(ctx)`(recall 用)/ `resolveScopeNotebook(ctx, scopeKind)`(ingest/curate 用)+ lazy-create |
| **2.3 Ingest worker** | `internal/nlmingest`:DM(handler/event)+ 群組(channel_pending_messages)→ buffer → ticker drain → Doc append → nlm source sync;high-water;fail-soft;opt-in flag |
| **2.4 Recall 升級** | notebook_recall 改 `nlm cross query` over resolved set |
| **2.5 策展工具** | remember_shared / remember_agent |
| **3 隔離驗證** | per-scope 隔離 unit test(A 不能命中 B)、群組隱私、conversation_id 續查 |

## 10. 風險

- nlm 基座脆弱(CDP 單帳號,~20-30 分 auth,10 分 cron keepalive;掛了 ingest+recall 雙停 → 需健康檢查)
- 隔離 100% 靠伺服端 scope 解析(scope 來自 SenderVerified 身分,絕不可來自訊息內容 / LLM 參數)
- Docs API rate/quota;cross query 延遲(多 notebook)
- PII:DM 只進 user 層;策展工具寫入前可選 redaction

## 11. Agent 記憶設定:單一 `other_config.memory` 欄位

對外設定收斂為**單一**公開欄位 `other_config.memory`,取代原本兩個各自獨立的
`other_config.memory_mode` + `other_config.memory_backend`。內部 gating 完全不變
——仍以 `store.MemoryMode {notebook,vault,both}` + `store.MemoryBackend {db,vault}`
注入 ctx,所有 gate site(notebook_recall / memory_search-get-expand / AutoInjector /
memory_interceptor / nlmingest sweep / vault file-vs-db routing)維持原樣。改的只有
mode+backend 的**來源**。

### 5 個公開值 → 內部 (mode, backend) 分解表

| `other_config.memory` | 內部 mode | 內部 backend | 說明 |
|-----------------------|-----------|--------------|------|
| `notebook` | `notebook` | `db` | 只開 NotebookLM 子系統;local 關閉時 backend 無作用,用 `db` 當惰性預設 |
| `vault` | `vault` | `vault` | 只開 MEMORY.md vault,寫入 Obsidian 檔 |
| `db` | `vault` | `db` | 只開 vault 子系統,但持久化走原生 Postgres |
| `both-vault` | `both` | `vault` | 全子系統 active,backend 走 vault 檔 |
| `both-db` | `both` | `db` | **DEFAULT** == 既有預設行為(notebook + vault 子系統 + 原生 db) |

預設值 `both-db`:任何沒設 `memory`、設了無效值、或完全空 `other_config` 的 agent,
都解析為 `mode=both, backend=db`,與收斂前的預設行為 bit-for-bit 相同。

### 解析優先序(`store.ParseMemory(*AgentData)` 單一來源,絕不報錯)

1. `other_config.memory` 有設且在白名單 → 依上表分解。
2. 否則 **fallback 到 legacy** `other_config.memory_mode` / `memory_backend`(未遷移
   的舊資料、或仍在設舊 key 的人都照常運作):mode = legacy memory_mode(預設 `both`),
   backend = legacy memory_backend(預設 `db`)。
3. 否則預設 `mode=both, backend=db`(== `both-db`)。

`memory` 設了但無效(garbage / 型別錯)→ **fall through 到第 2 步**(在同一個 bag 上跑
legacy),不會直接跳到預設,避免遷移過程中同時帶新舊 key 的 row 倒退。

`store.ParseMemoryMode()` 與 `store.ParseMemoryBackend()` 改為**衍生**自
`ParseMemory()`(分別取 `.Mode` / `.Backend`),所有既有 caller / ctx 注入點完全不動。

### Legacy / derived 註記

`memory_mode` / `memory_backend` 為 **legacy**,僅作為 fallback 來源保留;新設定一律用
單一 `memory`。資料遷移 `migrations/000083_merge_memory_config`(JSONB data migration,
非 schema 變更)把帶有任一 legacy key 的 row 收斂成 `memory`(UP),並可反向展開回兩個
舊 key(DOWN)。UP/DOWN 的 CASE 對應**鏡像**上方分解表。

> 注意 notebook 的不對稱:UP 把 `(notebook, 任何 backend)` 都折成 `notebook`(mode wins),
> 所以 DOWN 展開 notebook row 時 backend 一律回 `db`——即使原本帶 `memory_backend=vault`
> (例如 live esmith-general)。因為 mode=notebook 時 backend 本就惰性,此折損可接受。
>
> 遷移後預期結果(僅驗證目標,不動 live DB):esmith-general
> `{memory_mode:notebook, memory_backend:vault}` → `{memory:notebook}`;e-smith-hub
> `{memory_backend:vault}`(無 mode → both)→ `{memory:both-vault}`;空 `other_config`
> 不動 → 讀取時預設 `both-db`。
