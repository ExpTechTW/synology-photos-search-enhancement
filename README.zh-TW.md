# Synology Photos Search Enhancement

[English](README.md)

在 Synology Photos **原本的搜尋框**裡，用**照片內容**和**照片裡的文字**找照片。網頁版、手機 App、經 QuickConnect 都可以用。

- **內容**：搜「鳥」會找到老鷹和企鵝，不用先貼標籤（圖片 embedding，EmbeddingGemma 2）
- **照片裡的文字**：發票、招牌、截圖、文件（用任何能看圖的 LLM 做 OCR）
- **自訂標籤**：`sse tag add 旅行 1234 1235`，之後搜「旅行」

AI 找到的照片會**加進**原生搜尋結果，原本找得到的照片全部保留、順序不變。外掛不改 Synology Photos 的資料庫和照片，
也不修改任何程式，只改 nginx 的一行設定，可以完整解除安裝。任何一部分出問題時，nginx 會直接連回 DSM，Photos 照常運作。

```
Photos（網頁 / App）──▶ nginx ──▶ sse-filter ──▶ DSM（synoscgi）
                          └──── backup ─────────▶ DSM（filter 沒在跑時）
                                     │ 只處理搜尋請求
                                     ▼
                                sse-engine ◀── 索引 ── sse-indexer ──▶ 你的 embedding / OCR 服務
```

運作原理：[docs/how-it-works.zh-TW.md](docs/how-it-works.zh-TW.md)。

## 需要

- 有 **DSM 7** 和 **Synology Photos** 的 Synology NAS。開發環境是 DSM 7.3.2、Photos 1.9.0（x86_64）；
  ARM64 機種應該可以用，但沒有測試過。
- 能 SSH 登入的管理員帳號（`sudo`）。
- 一個 **embedding 服務**，跑支援圖片的 EmbeddingGemma 2，例如在有 GPU 的電腦上跑 llama.cpp。
  見 [docs/models.zh-TW.md](docs/models.zh-TW.md)。
- 選用，讀照片裡的文字：任何 **OpenAI 相容、能看圖的聊天模型**（Ollama、LM Studio、vLLM、LiteLLM、OpenAI…）。
- 照片要在擁有者的**個人空間**，而且這個帳號**不能開兩步驟驗證**（索引器用密碼登入）。

> **隱私**：照片的縮圖會送到你設定的 embedding 與 OCR 服務。不希望照片離開自己的網路，就把服務架在自己的機器上。

外掛不會對模型服務併發：任何時候最多只有一個 embedding 請求和一個 OCR 請求在處理，每個請求只有一張圖或一段文字，
上一個完成才送下一個。照片很多的話，第一次建立索引要好幾個小時；索引做到哪，搜尋就用到哪。

## 安裝

用管理員帳號 SSH 登入 NAS，執行：

```sh
curl -fsSL https://raw.githubusercontent.com/ExpTechTW/synology-photos-search-enhancement/main/install.sh | sudo sh
```

只想看看 NAS 能不能通過檢查？這樣不會做任何變更，也不會留下任何檔案：

```sh
curl -fsSL https://raw.githubusercontent.com/ExpTechTW/synology-photos-search-enhancement/main/install.sh | sudo sh -s -- --dry-run
```

安裝程式會：

1. 從 GitHub 下載最新版，並驗證 SHA-256。
2. **檢查** DSM、Synology Photos、nginx、systemd。有問題就把所有問題列出來，什麼都不改。
3. 下載 [uv](https://github.com/astral-sh/uv) 和獨立的 Python 3.12 到 `/usr/local/sse`，不碰系統的 Python。
4. **詢問** DSM 帳號、embedding 服務、OCR 模型，寫成 `~/sse/config.yml`（權限 600）。
5. 用這份設定**測試**：DSM 登入、縮圖、圖片與文字 embedding、OCR。
6. 安裝服務、讓 nginx 經過 filter，並確認流量真的有經過。任何一步失敗都會**自動回復**到安裝前的狀態。

之後索引器會在背景建立索引，用 `sudo sse status` 看進度。

選項放在 `sh -s --` 後面，例如 `curl -fsSL … | sudo sh -s -- --user alice`：

| | |
|---|---|
| `--channel pre-release` | 跟著 pre-release（`main` 分支的每次更新），不是穩定版 |
| `--version 版本` | 安裝指定版本，例如 `v2026.10.26+build.1` |
| `--user alice` | 照片屬於其他帳號（預設：執行 `sudo` 的帳號） |
| `--data-dir 目錄` | 放 `config.yml`、索引、log 的地方（預設：`~alice/sse`，需要開啟家目錄服務） |
| `--from-config 檔案` | 直接用這份 `config.yml`，不問問題（搭配 `--yes` 可以不需互動安裝） |
| `--dry-run` | 只檢查 |

不想直接把下載的腳本交給 shell 執行？先下載 [install.sh](install.sh) 看過，再執行 `sudo sh install.sh`。
在這個 repo 的原始碼目錄裡執行 `sudo ./install.sh`，安裝的是這份原始碼，不是 release（開發用）。

## 設定

所有設定都在**一個檔案** `~/sse/config.yml`。每個選項的說明見範本
[sse/data/config.example.yml](sse/data/config.example.yml)。改完之後：

```sh
sudo sse apply            # 檢查格式，用新設定重新啟動服務
sudo sse apply --check    # 先測試 DSM 登入與模型服務再套用
```

| 設定 | 用途 |
|---|---|
| `synology.account`、`password` | 照片擁有者的 DSM 帳號 |
| `embedding.base_url`、`api_key`、`model` | EmbeddingGemma 2 服務（[docs/models.zh-TW.md](docs/models.zh-TW.md)） |
| `embedding.image_size` | 送去 embedding 的縮圖：`m`（預設，較快）或 `xl` |
| `ocr.enabled`、`model` | 要不要 OCR、用哪個能看圖的模型 |
| `ocr.base_url`、`api_key` | 空白就跟 embedding 用同一個服務 |
| `ocr.extra_body` | 額外的請求欄位，例如會思考的模型用 `{reasoning_effort: none}` |
| `search.min_score`、`text_min_score` | 調高：結果較少、較準 |
| `filter.mode` | `engine`（加入 AI 結果）或 `passthrough`（完全不改，排查問題用） |
| `log.keep_days` | 只留這幾天內的 log（預設 3） |

換 embedding 模型或 `embedding.image_size` 會重做每張照片的 embedding；換 OCR 模型會重做 OCR。

**調整門檻**：`sse search 貓 --explain` 會列出每張照片的分數和被選進來的原因。
看到不相關的照片就調高 `min_score`，找不到該有的照片就調低。

## 指令

| 指令 | |
|---|---|
| `sudo sse status` | 服務狀態、filter 統計、索引進度（不加 `sudo`：只顯示索引進度） |
| `sudo sse doctor` | 診斷並給建議 |
| `sudo sse logs [indexer\|engine\|filter\|sse] [-n 50] [-f]` | 看 log |
| `sse search 字 [--explain]` | 試搜尋、看分數 |
| `sse tag add\|rm\|list 標籤 [id…]` | 自訂標籤（`sse search` 會列出照片 id） |
| `sse retry-failed` | 重試失敗 3 次而放棄的照片 |
| `sudo sse apply [--check]` | 套用 `config.yml` 的修改 |
| `sudo sse disable` / `enable` | 緊急開關：停止 / 恢復加入結果（5 秒內生效） |
| `sudo sse bypass on` / `off` | nginx 直連 DSM / 恢復經過 filter |
| `sudo sse reapply` | DSM 更新後重新套用 nginx 設定 |
| `sudo sse capture on\|show\|off` | 錄下搜尋請求供排查（不含 cookie、session） |
| `sudo sse update [--check]` | 更新到最新版 / 只檢查有沒有新版 |
| `sudo sse uninstall [--purge]` | 解除安裝 |

不加 `sudo` 的指令要用照片擁有者的帳號執行；加了 `sudo` 會自動切換成擁有者，索引檔案不會變成 root 擁有。

## 更新

```sh
sudo sse update --check    # 有沒有新版？
sudo sse update            # 更新；config.yml 和索引都會保留
```

版本以日期命名：`vYYYY.MM.DD+build.N`。有兩個更新管道：

| 管道 | |
|---|---|
| `release`（預設） | 穩定版 |
| `pre-release` | 合併進 `main` 的每次更新，測試用 |

用 `sudo sse update --channel pre-release`（或 `release`）切換，之後的更新都沿用。`sse update` 不會降版：從
pre-release 回到比較舊的 release 要加 `--force`，或用 `--version 版本` 指定。

更新時新版會先裝在舊版旁邊，全部正常才切換過去。新版沒通過檢查（服務要能啟動、DSM 要能回應）就自動換回舊版。

`sse` 指令壞掉時，也可以不透過它更新：

```sh
curl -fsSL https://raw.githubusercontent.com/ExpTechTW/synology-photos-search-enhancement/main/install.sh | sudo sh -s -- update
```

## DSM 與 Photos 更新

- **DSM 更新**：DSM 可能會重新產生 `nginx.conf`。外掛會先暫停（DSM 照常運作，只是沒有外掛），
  等你確認一切正常後執行 `sudo sse reapply`。需要的時候 `sudo sse doctor` 會提醒。
- **Synology Photos 更新**：回應格式可能改變，filter 會自動停止加入結果。確認搜尋正常後執行 `sudo sse apply` 恢復。

平常 DSM 重新產生 `nginx.conf` 時（例如改了網頁服務的設定），guard 會自動重新套用。

## 解除安裝

```sh
sudo sse uninstall            # 會詢問要不要連 ~/sse 一起刪（預設保留）
sudo sse uninstall --purge    # 不詢問，直接連 ~/sse 一起刪
```

或不透過 `sse` 指令：

```sh
curl -fsSL https://raw.githubusercontent.com/ExpTechTW/synology-photos-search-enhancement/main/install.sh | sudo sh -s -- uninstall
```

保留 `~/sse`（索引資料庫與 `config.yml`）的話，之後重新安裝會接著用，不必重建索引。加 `--yes` 時除非同時加 `--purge`，
否則一律保留。

會停止並移除服務、還原 `nginx.conf`，刪除 `/usr/local/sse`、`/usr/local/etc/sse`、`/var/log/sse`、`/usr/local/bin/sse`。
不會動 Synology Photos 和你的照片。

### 手動還原

`sse` 指令本身不能用時：

```sh
# 1. nginx 直連 DSM：在 /etc/nginx/nginx.conf 裡，把 "# >>> synology-search-enhancement" 到
#    "# <<< synology-search-enhancement <<<"（含這兩行）整段換成：
#        server unix:/run/synoscgi.sock;
#    （DSM 之後重新產生過 nginx.conf 的話，這段已經不在了，可以跳過）
sudo vi /etc/nginx/nginx.conf
sudo nginx -t && sudo nginx -s reload

# 2. 停止並移除服務（DSM 7 是 systemd 219，沒有 "disable --now"）
UNITS="sse-nginx-guard.path sse-nginx-guard.timer sse-filter.service sse-engine.service sse-indexer.service"
sudo systemctl stop $UNITS
sudo systemctl disable $UNITS
sudo rm -f /etc/systemd/system/sse-*
sudo systemctl daemon-reload

# 3. 移除檔案
sudo rm -rf /usr/local/sse /usr/local/etc/sse /usr/local/bin/sse /var/log/sse /etc/nginx/nginx.conf.sse-orig
```

`/etc/nginx/nginx.conf.sse-orig` 是修改前的設定。只有 DSM 之後沒有重新產生過 `nginx.conf` 時，才能直接把它蓋回去。

## 排查問題

- 先執行 `sudo sse doctor`，再看 `sudo sse logs indexer` / `engine` / `filter`。
- 搜尋結果怪怪的：`sudo sse disable` 5 秒內就恢復成原生搜尋，可以慢慢查。
- 懷疑影響到 DSM 本身：`sudo sse bypass on` 讓 filter 完全不在路徑上。
- 索引暫停：`sudo sse status` 會顯示原因（模型服務連不上、DSM 登入失敗…），之後會自己重試。

## 限制

- 只索引擁有者的個人空間，一台 NAS 一個帳號；共享空間與其他使用者的照片不會被索引。
- 不支援開了兩步驟驗證的帳號。
- 「相似」是用分數門檻判斷，難免會漏掉一些、多出一些；用 `sse search 字 --explain` 調整。

## 開發

```sh
sh tests/run.sh          # 全部（第一次會下載 Python 3.8 / 3.12，並編譯跟 DSM 7.3 同版的 nginx 1.23.1）
sh tests/run.sh quick    # 不需要 nginx 的部分
```

測試用假的 DSM、假的模型服務、假的 `systemctl`，搭配真的 nginx 與 DSM 的 Python 3.8。
`tests/test_install_sandbox.py` 會從安裝到解除安裝實際跑一遍 `install.sh`，包含用假的 GitHub release 測試
`curl | sh` 安裝與更新。

推到 `main` 並通過測試會發佈 pre-release，推到 `release` 會發佈 release
（[.github/workflows/ci-release.yml](.github/workflows/ci-release.yml)，打包用
[scripts/package.sh](scripts/package.sh)）。

## 授權

[GPL-3.0-or-later](LICENSE)。

本專案與 Synology Inc. 無關，也未經其認可。Synology 與 Synology Photos 是 Synology Inc. 的商標。
