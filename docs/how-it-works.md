# How it works

[繁體中文](how-it-works.zh-TW.md)

The goal: photos found by AI (image embeddings and OCR) appear **in the native Synology Photos search results**, on the
web and in the mobile app, including over QuickConnect. The Synology Photos database stays untouched, no binary is
patched, and everything can be removed.

## Components

```
┌─ sse-indexer · photo owner's account · systemd ────────────────────────────────────┐
│ DSM WebAPI (read-only): Browse.Item.list / RecentlyAdded.list → Thumbnail.get       │
│        │ thumbnail                                                                   │
│        ├──▶ /v1/embeddings (EmbeddingGemma 2, image)          → 768-dim vector       │
│        └──▶ /v1/chat/completions (vision model)               → OCR text → vector   │
│        ▼                                                                             │
│   SQLite ~/sse/data/index.db                                                         │
└──────────────────────────────────────────────────────────────────────────────────────┘
        │ read-only
┌─ sse-engine · photo owner's account, nginx group · systemd ─────────────────────────┐
│ vectors in memory (int8 × 512, scored on all CPU cores) + SQLite (OCR text, tags)    │
│ /run/sse-engine/engine.sock: search word → query embedding (cached) → matching ids  │
└──────────────────────────────────────────────────────────────────────────────────────┘
        │ unix socket
┌─ sse-filter · nginx worker user · systemd ───────────────────────────────────────────┐
│ nginx ──scgi──▶ /run/sse/filter.sock ──▶ /run/synoscgi.sock (DSM)                    │
│   └─ backup ─────────────────────────▶ /run/synoscgi.sock (when the filter is down)   │
│ rewrites only the responses of SYNO.Foto.Search.Search                                │
└──────────────────────────────────────────────────────────────────────────────────────┘
```

Everything is one static Go binary, `/usr/local/sse/sse` (about 11 MB, no dependencies on the NAS; built for x86_64,
ARM64 and ARMv7). The services are subcommands of it:

| Component | Command | Runs as | Code |
|---|---|---|---|
| `sse-filter` | `sse filter` | nginx worker user | [internal/filter](../internal/filter) |
| `sse-indexer` | `sse index` | photo owner | [internal/indexer](../internal/indexer) |
| `sse-engine` | `sse serve` | photo owner, nginx group | [internal/server](../internal/server), [internal/search](../internal/search) |
| `sse` command | `sse status`, `sse apply`… | root for installing and maintenance; switches to the owner for index commands | [internal/admin](../internal/admin), [internal/engine](../internal/engine) |

Only the filter sits on DSM's critical path, so it is small and forwards anything it does not understand unchanged.
Everything that needs a model server or the DSM password runs under the photo owner's account, never as root.

### What changes on DSM

| Path | |
|---|---|
| `/usr/local/sse/` | The program (one binary, owned by root) |
| `/usr/local/bin/sse` | The command |
| `/usr/local/etc/sse/` | Install record, filter settings (generated from `config.yml`), emergency switch |
| `/var/log/sse/` | Filter and audit logs |
| `/etc/systemd/system/sse-*` | The services and the guard |
| `/etc/nginx/nginx.conf` | `upstream synoscgi` becomes "filter first, synoscgi as backup", between marker comments |
| `/etc/nginx/nginx.conf.sse-orig` | The file as it was before the change |

The user's data directory (`~/sse`: `config.yml`, the index, the indexer and engine logs) is kept on uninstall unless
`--purge` is given.

## Indexing

```
start: sign in → UserInfo.me → the database must belong to the same user (otherwise refuse, exit 2: no restart)
loop:
  full scan (every 6 h): page through Browse.Item.list
      new or changed thumbnail (cache_key) → queue; not seen → mark deleted
      ↳ if more than half the photos are missing in one scan, delete nothing (more likely an API problem)
  quick scan (every 10 min): RecentlyAdded.list until a whole page has no changes
  embedding: newest first, one photo per request, one request at a time
      someone searched in the last 15 s → wait: the query embedding shares the model server
  OCR: only after every photo is embedded (content search becomes usable first), one photo at a time
  OCR text → vector, one text at a time (texts with fewer than 4 letters are skipped)
  when idle: at most once a day, compact the database (VACUUM) if ≥ 25 % and ≥ 8 MB can be reclaimed
```

**No concurrency**: the indexer and the search service are separate processes, and both can call the embedding
server. They queue on a lock file next to the database, so at any moment there is at most one embedding request and
one OCR request in flight, each carrying a single image or text.

**Failures**

- A problem with one photo (thumbnail 404, unknown format, the model cannot decode it) is retried 3 times, then that
  photo is given up on. `sse retry-failed` queues those photos again.
- A problem with a service (unreachable, timeout, HTTP 5xx or 429, session expired) pauses the whole indexer, which
  retries with a growing delay. It is not counted against any photo. A cheap request to the same service tells the
  two apart: for example, llama.cpp answers 500 both when it is down and when it cannot decode one particular image.
- A thumbnail size that does not exist (some HEIC files have no `m`) falls back to another size.

Changing the embedding model or image size re-queues every photo's embedding. Changing the OCR model re-queues OCR.
Writes are conditional on the thumbnail's `cache_key`, so a photo that changed or was deleted while being processed is
not written. Only one goroutine writes to the database.

While the model server works on one photo, the indexer already fetches the next photo's thumbnail from DSM, so the
model server never waits for DSM.

**Progress**: the indexer writes its live state (phase, scan progress, pause reason, recent speed) to
`indexer-status.json` next to the database. `sse status` combines it with the totals in the database to show
percentages and an estimate of the time left.

## Search

### The engine decides which photos match

Synology Photos sorts search results by time, so the question is *which* photos match, not how to rank them
([internal/search](../internal/search/search.go)).

1. **Query embedding** (LRU cache of 2,048 entries), cut to the first 512 dimensions and normalised. EmbeddingGemma
   is trained so that its leading dimensions work on their own.
2. **Image vectors** are kept in memory as int8 × 512 (516 bytes per photo) and compared with every photo, split
   across all CPU cores (about 5 ms for 20,000 photos on a 4-core Celeron J4125).
3. **Hub correction**: some photos (certain screenshots, for example) are similar to almost every query and would
   appear in unrelated searches. Each photo's average score over a set of generic queries is measured, and the part
   above the overall average is subtracted.
4. **Threshold**: keep photos scoring ≥ `max(min_score, best − max_drop)`, at most `max_results`. A "mean + N standard
   deviations" rule does not work: a common subject (cats in more than 10 % of the photos) raises both the mean and the
   deviation, and then nothing is selected.
5. **Added on top**, without changing the image threshold:
   - text in photos, word for word: FTS5 trigram index for 3+ characters, `LIKE` for shorter words
   - text in photos, by meaning: the OCR text vectors, with their own threshold (`text_min_score`, `text_max_drop`)
   - your own tags

   These only add photos. Common words appear in the text of *some* photo, so letting them influence the image
   threshold would hide most of the real matches.

`sse search WORD --explain` shows the scores and which rule selected each photo.

### The filter merges the results into the native response

The filter rewrites only the four methods of `SYNO.Foto.Search.Search` (`count_item`, `list_item`,
`list_basic_timeline_info`, `get_search_timeline`). Every other request is streamed through without buffering.

```
1. Ask the engine: search word → candidate photo ids (cached 60 s)
2. With the searcher's own session, call Browse.Item.get(candidate ids)
     photos DSM does not return are not added (no permission, deleted);
     the returned owner_user_id must be the indexed user
3. With the searcher's own session, fetch the full native list_basic_timeline_info
4. Merge into one list: newest first (stable, so equal times keep the native order), no duplicates
5. Answer all four methods from that one list:
     count_item               → the count
     list_basic_timeline_info → the list (applying start_time / end_time)
     get_search_timeline      → the sections rebuilt per day / month, following Photos' own rules
     list_item                → [offset : offset + limit]; native items reused, the rest from Browse.Item.get
```

Counts, pages and the timeline all come from the same list, so they stay consistent however the app pages through
them. Steps 2–3 are cached per (session, search word) for 60 seconds, shared by the four requests of one search.

Permissions are DSM's: photos are fetched with the searcher's session, so other accounts never see the owner's photos.
The web page and the mobile app send different requests (the app sends a single `list_item` with 14 extra fields);
both are tested.

## Safety and failure handling

| Situation | Behaviour |
|---|---|
| Filter not running, crashed or deleted | nginx's backup upstream goes straight to synoscgi; DSM works as usual |
| A POST fails after reaching the filter | nginx does not retry it (no double upload or delete); that request gets 502 |
| Uploads and downloads | Streamed without buffering (200 MB tested at < 64 MB of memory) |
| Engine not answering or too slow | Native results only |
| Sub-request fails, response not understood | Native response unchanged |
| Another account searches | Browse.Item.get runs with their permissions, so nothing of the owner's is added |
| Not signed in, session expired | Sub-request fails, nothing is added |
| No photo data can be fetched for the matches | Native results, counted as "skipped"; never an empty list |
| Synology Photos updated | The filter detects the new version and stops adding results until `sse apply` |
| DSM regenerates `nginx.conf` | Back to stock = plugin paused, DSM fine; the guard re-applies when it is safe, and stops doing so after a DSM update until `sse reapply` |
| Emergency | `sse disable` (stops adding results within 5 s), `sse bypass on` (filter out of the path) |
| DSM or model server briefly unavailable | Indexer pauses and retries; photos are not marked failed |
| Database belongs to another account | Indexer refuses to run |
| Password and API keys | In `config.yml`, which must be mode 600; the plugin refuses to read it otherwise. The filter's generated settings contain no secrets |

## Storage and memory

- `~/sse/data/index.db`, SQLite in WAL mode: the indexer writes, the search service reads. Page size 64 KB
  (a vector row is about 1.5 KB).
- Per photo: about 1.5 KB for the image vector (768 × float16, the model's full output; cutting and quantising
  happens at search time, so changing that needs no model calls), plus the OCR text and its vector.
- Search service memory: about 516 bytes per photo per vector set.
- Logs rotate daily and are kept for `log.keep_days` days (default 3). The filter logs to `/var/log/sse/filter.log`;
  `sse` commands that change something log their full output to `/var/log/sse/sse.log`; the indexer and engine log to
  `~/sse/logs/`.

## Behaviour of Synology Photos the filter relies on

Checked against Photos 1.9.0 by calling the API directly on a NAS:

- Native search results are ordered by time only.
- `start_time` / `end_time` are applied only by `list_item` and `list_basic_timeline_info`; `count_item` and
  `get_search_timeline` ignore them. `sort_direction` and `item_type` are ignored by all four.
- `get_search_timeline` sections (dates in UTC): days (or months) are accumulated in order, and a section closes when it
  exceeds 100 photos. A single day over 100 photos forms its own sections, split evenly into `count // 100`; those
  sections list the whole day (`item_count` is the day's total) with `offset` as the section's start within the day.
  Before a large day, or at the end, a section that is not yet full is merged into the previous regular one. The
  filter's reconstruction matched the native response exactly for 30 keywords × day / month.
- `Browse.Item.get` succeeds even when some ids do not exist and returns only those that do. Versions 1 and 2 reject
  the extra fields the mobile app asks for (error 120); versions 3–7 accept them. The filter uses version 4, whose
  output matched the native `list_item` item for item.
