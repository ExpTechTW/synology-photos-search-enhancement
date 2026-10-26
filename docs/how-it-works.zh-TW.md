# 運作原理

[English](how-it-works.md)

目標：讓 AI（圖片 embedding 與 OCR）找到的照片直接出現在 **Synology Photos 原生的搜尋結果**裡，網頁版、手機 App、
經 QuickConnect 都一樣。不動 Synology Photos 的資料庫、不修改任何程式，而且可以完整移除。

## 元件

```
┌─ sse-indexer ·照片擁有者的帳號 · systemd ──────────────────────────────────────────┐
│ DSM WebAPI（只讀）：Browse.Item.list / RecentlyAdded.list → Thumbnail.get            │
│        │ 縮圖                                                                        │
│        ├──▶ /v1/embeddings（EmbeddingGemma 2，圖片）       → 768 維向量              │
│        └──▶ /v1/chat/completions（能看圖的模型）           → OCR 文字 → 向量         │
│        ▼                                                                             │
│   SQLite ~/sse/data/index.db                                                         │
└──────────────────────────────────────────────────────────────────────────────────────┘
        │ 唯讀
┌─ sse-engine ·照片擁有者的帳號、nginx 群組 · systemd ──────────────────────────────┐
│ 向量常駐記憶體（int8 × 512，比對時用上所有 CPU 核心）＋ SQLite（OCR 文字、標籤）     │
│ /run/sse-engine/engine.sock：搜尋字 → 查詢 embedding（快取）→ 命中的照片 id         │
└──────────────────────────────────────────────────────────────────────────────────────┘
        │ unix socket
┌─ sse-filter · nginx worker 身分 · systemd ─────────────────────────────────────────┐
│ nginx ──scgi──▶ /run/sse/filter.sock ──▶ /run/synoscgi.sock（DSM）                   │
│   └─ backup ─────────────────────────▶ /run/synoscgi.sock（filter 沒在跑時）          │
│ 只改寫 SYNO.Foto.Search.Search 的回應                                                 │
└──────────────────────────────────────────────────────────────────────────────────────┘
```

全部是一個靜態連結的 Go 執行檔 `/usr/local/sse/sse`（約 11 MB，NAS 上不需要任何其他東西；提供 x86_64、ARM64、ARMv7 版本），
各個服務都是它的子指令：

| 元件 | 指令 | 執行身分 | 程式 |
|---|---|---|---|
| `sse-filter` | `sse filter` | nginx worker | [internal/filter](../internal/filter) |
| `sse-indexer` | `sse index` | 照片擁有者 | [internal/indexer](../internal/indexer) |
| `sse-engine` | `sse serve` | 照片擁有者、nginx 群組 | [internal/server](../internal/server)、[internal/search](../internal/search) |
| `sse` 指令 | `sse status`、`sse apply`… | 安裝與維運用 root；索引相關指令切換成擁有者 | [internal/admin](../internal/admin)、[internal/engine](../internal/engine) |

只有 filter 在 DSM 的關鍵路徑上，所以它要小、看不懂的東西一律原樣轉發。要連模型服務、要用 DSM 密碼的部分
都以照片擁有者的身分執行，不用 root。

### 對 DSM 的變更

| 路徑 | |
|---|---|
| `/usr/local/sse/` | 程式（一個執行檔，root 擁有） |
| `/usr/local/bin/sse` | 指令 |
| `/usr/local/etc/sse/` | 安裝紀錄、filter 設定（由 `config.yml` 產生）、緊急開關 |
| `/var/log/sse/` | filter 與操作紀錄的 log |
| `/etc/systemd/system/sse-*` | 服務與 guard |
| `/etc/nginx/nginx.conf` | `upstream synoscgi` 改成「filter 為主、synoscgi 為 backup」，前後有標記註解 |
| `/etc/nginx/nginx.conf.sse-orig` | 修改前的設定 |

使用者的資料目錄（`~/sse`：`config.yml`、索引、索引器與搜尋服務的 log）解除安裝時保留，加 `--purge` 才刪。

## 建立索引

```
啟動：登入 → UserInfo.me → 資料庫必須屬於同一個使用者（不同就拒絕，exit 2：不重啟）
迴圈：
  完整掃描（每 6 小時）：Browse.Item.list 全部翻完
      新照片、縮圖變了（cache_key）→ 排入；沒看到的 → 標記刪除
      ↳ 一次少了一半以上就不標記刪除（比較可能是 API 出問題）
  快速掃描（每 10 分鐘）：RecentlyAdded.list，直到一整頁都沒有變化
  embedding：新到舊，一個請求一張，一次一個請求
      最近 15 秒內有人搜尋就先等：查詢字的 embedding 跟索引共用同一個模型服務
  OCR：每張照片都做完 embedding 才開始（讓內容搜尋先能用），一次一張
  OCR 文字 → 向量，一次一段（字母或中文字少於 4 個的跳過）
  閒下來時：每天最多檢查一次，可回收空間達 25% 且至少 8 MB 就壓實（VACUUM）
```

**不併發**：索引器和搜尋服務是不同的 process，兩者都會呼叫 embedding 服務，用資料庫旁的鎖檔排隊。
所以任何時候最多只有一個 embedding 請求和一個 OCR 請求，每個請求只有一張圖或一段文字。

**失敗處理**

- 某張照片的問題（縮圖 404、格式不認得、模型解不開）重試 3 次後放棄這張。`sse retry-failed` 可以重新排入。
- 服務的問題（連不上、逾時、HTTP 5xx 或 429、session 過期）讓索引器整體暫停，等待時間逐漸拉長後重試，不算在任何照片上。
  用一個很便宜的請求確認服務本身是否正常來區分兩者：例如 llama.cpp 在掛掉時、和解不開某張圖時都回 500。
- 縮圖尺寸不存在（有些 HEIC 沒有 `m`）就改用其他尺寸。

換 embedding 模型或圖片尺寸會重新排入每張照片的 embedding；換 OCR 模型會重新排入 OCR。寫入時以縮圖的 `cache_key`
為條件，處理期間照片被改掉或刪掉就不寫入。只有一個 goroutine 寫資料庫。

模型服務處理一張照片的同時，索引器已經先向 DSM 取下一張的縮圖，模型服務不必等 DSM。

**進度**：索引器把即時狀態（階段、掃描進度、暫停原因、最近的處理速度）寫到資料庫旁的 `indexer-status.json`，
`sse status` 再加上資料庫裡的總數，算出百分比與預估剩餘時間。

## 搜尋

### engine：決定哪些照片算命中

Synology Photos 的搜尋結果依時間排序，所以要決定的是「哪些算命中」，不是排名（[internal/search](../internal/search/search.go)）。

1. **查詢 embedding**（LRU 快取 2,048 筆），取前 512 維再正規化。EmbeddingGemma 訓練時就讓前面的維度可以單獨使用。
2. **圖片向量**以 int8 × 512 常駐記憶體（每張 516 bytes），每次跟所有照片比對，分給所有 CPU 核心
   （四核 Celeron J4125 上 2 萬張約 5 ms）。
3. **hub 校正**：有些照片（例如某些截圖）跟什麼查詢都很像，會出現在各種不相關的搜尋裡。先算出每張照片對一組
   通用查詢的平均分數，比對時扣掉它高於整體平均的部分。
4. **門檻**：分數 ≥ `max(min_score, 最高分 − max_drop)` 的照片，最多 `max_results` 張。不用「平均 + N 個標準差」：
   常見的主題（例如貓佔了一成以上的照片）會把平均與標準差一起拉高，結果一張都選不到。
5. **另外加進來**（不影響圖片的門檻）：
   - 照片裡的文字逐字比對：3 個字以上用 FTS5 trigram 索引，更短的用 `LIKE`
   - 照片裡的文字的語意：OCR 文字的向量，有自己的門檻（`text_min_score`、`text_max_drop`）
   - 自訂標籤

   這些只會加照片。常見的字幾乎都會出現在某張照片的文字裡，如果讓它們影響圖片門檻，大部分真正的結果會被刷掉。

`sse search 字 --explain` 會列出分數，以及每張照片是被哪個規則選進來的。

### filter：把結果併進原生回應

filter 只改寫 `SYNO.Foto.Search.Search` 的四個 method（`count_item`、`list_item`、`list_basic_timeline_info`、
`get_search_timeline`），其他請求都邊收邊轉、不緩衝。

```
1. 問 engine：搜尋字 → 候選照片 id（快取 60 秒）
2. 用搜尋者自己的 session 呼叫 Browse.Item.get(候選 id)
     DSM 沒回的不加入（沒權限、已刪除）；回來的 owner_user_id 必須是索引的那個使用者
3. 用搜尋者自己的 session 取完整的原生 list_basic_timeline_info
4. 合併成一份清單：新到舊（穩定排序，同時間保持原生順序），id 不重複
5. 四個 method 都從同一份清單回答：
     count_item               → 數量
     list_basic_timeline_info → 清單（套用 start_time / end_time）
     get_search_timeline      → 依天 / 月重建分段，規則跟 Photos 相同
     list_item                → [offset : offset + limit]；原生回應裡有的沿用，其餘用 Browse.Item.get 取
```

數量、分頁、時間軸都來自同一份清單，不論 App 怎麼翻頁都一致。2～3 的結果依（session、搜尋字）快取 60 秒，
一次搜尋的四個請求共用。

權限由 DSM 決定：照片是用搜尋者的 session 取的，所以其他帳號永遠看不到擁有者的照片。網頁版和手機 App 送的請求不同
（App 只送一個帶 14 種額外欄位的 `list_item`），兩種都有測試。

## 安全與失敗處理

| 情況 | 行為 |
|---|---|
| filter 沒在跑、當掉、被刪 | nginx 的 backup upstream 直連 synoscgi，DSM 照常 |
| POST 已送進 filter 才失敗 | nginx 不重送（避免同一個上傳或刪除做兩次），該請求回 502 |
| 上傳 / 下載 | 邊收邊轉不緩衝（200 MB 實測記憶體 < 64 MB） |
| engine 沒回應、太慢 | 只有原生結果 |
| 子請求失敗、回應格式看不懂 | 原樣轉發 |
| 其他帳號搜尋 | Browse.Item.get 以他的權限執行，拿不到擁有者的照片 → 不加入 |
| 未登入 / session 失效 | 子請求失敗 → 不加入 |
| 取不到要加入的照片資料 | 回傳原生結果並記成「略過」，不會回空清單 |
| Synology Photos 更新 | filter 偵測到版本變了就停止加入結果，`sse apply` 後恢復 |
| DSM 重新產生 `nginx.conf` | 回到原廠 = 外掛暫停、DSM 正常；guard 在安全條件下重新套用，DSM 更新過就暫停自動套用，等 `sse reapply` |
| 緊急狀況 | `sse disable`（5 秒內停止加入結果）、`sse bypass on`（filter 不在路徑上） |
| DSM 或模型服務暫時斷線 | 索引器整體暫停後重試，不會把照片標成失敗 |
| 資料庫屬於另一個帳號 | 索引器拒絕執行 |
| 密碼與 API key | 在 `config.yml` 裡，權限必須是 600，否則拒絕讀取；產生給 filter 的設定不含任何密碼 |

## 儲存與記憶體

- `~/sse/data/index.db`，SQLite 的 WAL 模式：索引器寫、搜尋服務讀。page size 64 KB（一列向量約 1.5 KB）。
- 每張照片：圖片向量約 1.5 KB（768 × float16，模型的完整輸出；截短與量化在搜尋時才做，改格式不必重跑模型），
  加上 OCR 文字與它的向量。
- 搜尋服務的記憶體：每組向量每張照片約 516 bytes。
- log 每天換檔，只留 `log.keep_days` 天內的（預設 3）。filter 寫 `/var/log/sse/filter.log`；有變更的 `sse` 指令把
  完整輸出記到 `/var/log/sse/sse.log`；索引器與搜尋服務寫到 `~/sse/logs/`。

## filter 依賴的 Synology Photos 行為

在 NAS 上直接呼叫 Photos 1.9.0 的 API 確認：

- 原生搜尋結果只依時間排序。
- `start_time` / `end_time` 只有 `list_item` 與 `list_basic_timeline_info` 會套用；`count_item` 與
  `get_search_timeline` 不理會。`sort_direction`、`item_type` 四個都不理會。
- `get_search_timeline` 的分段（日期以 UTC 計）：以天（或月）為一組依序累積，超過 100 張成一段。單一天超過 100 張時
  自己成段，平均拆成「張數 // 100」段，這些段的清單是整天（`item_count` 是整天的張數），`offset` 是該段在當天的起點。
  遇到大組或結尾時，還沒滿的段併入前一個一般段。filter 重建的結果用 30 個關鍵字 × 天 / 月與原生回應比對，完全相同。
- `Browse.Item.get` 含不存在的 id 時照樣成功，只回存在的。版本 1、2 不接受手機 App 要的額外欄位（錯誤碼 120），
  3～7 都接受。filter 用版本 4，回傳的資料與原生 `list_item` 逐張相同。
