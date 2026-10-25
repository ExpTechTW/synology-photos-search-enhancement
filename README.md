# Synology Photos Search Enhancement

[繁體中文](README.zh-TW.md)

Find photos by **what is in them** and by **the text in them**, right in Synology Photos' own search box,
on the web, in the mobile app, and over QuickConnect.

- **Content**: search "bird" and get eagles and penguins, with no tagging needed (image embeddings, EmbeddingGemma 2)
- **Text in photos**: receipts, street signs, screenshots, documents (OCR with any vision LLM)
- **Your own tags**: `sse tag add trip 1234 1235`, then search "trip"

What the AI finds is **added to** the native results. Everything Synology Photos already finds stays, in the same
order. The plugin does not modify the Synology Photos database or your photos, and it does not patch any program. It
changes one line in nginx, and it can be uninstalled completely. If any part of it stops working, nginx goes straight
back to DSM and Photos works as before.

```
Photos (web / app) ──▶ nginx ──▶ sse-filter ──▶ DSM (synoscgi)
                         └──── backup ─────────▶ DSM, when the filter is down
                                    │ search requests only
                                    ▼
                               sse-engine ◀── index ── sse-indexer ──▶ your embedding / OCR server
```

How it works in detail: [docs/how-it-works.md](docs/how-it-works.md).

## Requirements

- A Synology NAS with **DSM 7** and **Synology Photos**. Developed on DSM 7.3.2 with Photos 1.9.0 (x86_64).
  ARM64 models should work but have not been tested.
- SSH access with an administrator account (`sudo`).
- An **embedding server** running EmbeddingGemma 2 with image support, for example llama.cpp on a PC with a GPU.
  See [docs/models.md](docs/models.md).
- Optional, for text in photos: any **OpenAI-compatible vision chat model** (Ollama, LM Studio, vLLM, LiteLLM, OpenAI…).
- Photos must be in the owner's **Personal Space**, and that account must **not use 2-step verification**
  (the indexer signs in with the password).

> **Privacy**: thumbnails of your photos are sent to the embedding and OCR servers you configure. Run them on your own
> hardware if your photos must not leave your network.

The plugin never sends concurrent requests to your model servers. At any moment it has at most one embedding
request and one OCR request in flight, each carrying a single image or text, and it waits for each to finish before
sending the next. The first indexing run can take hours for a large library; search works on whatever is indexed so far.

## Install

SSH into the NAS with an administrator account and run:

```sh
curl -fsSL https://raw.githubusercontent.com/ExpTechTW/synology-photos-search-enhancement/main/install.sh | sudo sh
```

Only want to see whether your NAS passes the checks? This changes nothing and leaves nothing behind:

```sh
curl -fsSL https://raw.githubusercontent.com/ExpTechTW/synology-photos-search-enhancement/main/install.sh | sudo sh -s -- --dry-run
```

The installer:

1. Downloads the latest release from GitHub and verifies its SHA-256.
2. **Checks** DSM, Synology Photos, nginx and systemd. If anything is off, it lists every problem and changes nothing.
3. Downloads [uv](https://github.com/astral-sh/uv) and a private Python 3.12 into `/usr/local/sse`. The system Python
   is not touched.
4. **Asks** for the DSM account, the embedding server and the OCR model, and writes `~/sse/config.yml` (mode 600).
5. **Tests** that config: DSM sign-in, a thumbnail, image and text embedding, and OCR.
6. Installs the services, routes nginx through the filter and confirms that traffic really flows through it.
   If any step fails, everything is **rolled back** automatically.

Indexing then starts in the background. Check progress with `sudo sse status`.

Options go after `sh -s --`, for example `curl -fsSL … | sudo sh -s -- --user alice`:

| | |
|---|---|
| `--channel pre-release` | Follow pre-releases (every change on `main`) instead of stable releases |
| `--version TAG` | Install a specific version, e.g. `v2026.10.26+build.1` |
| `--user alice` | The photos belong to another account (default: the account running `sudo`) |
| `--data-dir DIR` | Where `config.yml`, the index and logs live (default: `~alice/sse`; the user home service must be on) |
| `--from-config FILE` | Use this `config.yml` instead of the questions (with `--yes` for a non-interactive install) |
| `--dry-run` | Checks only |

Prefer not to pipe into a shell? Download [install.sh](install.sh), read it, and run `sudo sh install.sh`. Running
`sudo ./install.sh` inside a clone of this repository installs that code instead of a release (for development).

## Configuration

Everything is in **one file**, `~/sse/config.yml`. The template with every option explained is
[sse/data/config.example.yml](sse/data/config.example.yml). After editing it:

```sh
sudo sse apply            # validate and restart the services with the new settings
sudo sse apply --check    # also test DSM sign-in and the model servers first
```

| Setting | What it does |
|---|---|
| `synology.account`, `password` | The DSM account that owns the photos |
| `embedding.base_url`, `api_key`, `model` | Your EmbeddingGemma 2 server ([docs/models.md](docs/models.md)) |
| `embedding.image_size` | Thumbnail sent for embedding: `m` (default, fast) or `xl` |
| `ocr.enabled`, `model` | OCR on/off and the vision model |
| `ocr.base_url`, `api_key` | Leave empty to use the embedding server |
| `ocr.extra_body` | Extra request fields, e.g. `{reasoning_effort: none}` for thinking models |
| `search.min_score`, `text_min_score` | Higher = fewer, more precise results |
| `filter.mode` | `engine` (add AI results) or `passthrough` (change nothing, for troubleshooting) |
| `log.keep_days` | Logs older than this are deleted (default 3) |

Changing the embedding model or `embedding.image_size` re-embeds every photo; changing the OCR model re-runs OCR.

**Tuning the thresholds**: `sse search cat --explain` shows each result's score and why it was included.
Raise `min_score` if you see unrelated photos, lower it if photos are missing.

## Commands

| Command | |
|---|---|
| `sudo sse status` | Services, filter statistics and indexing progress (without `sudo`: progress only) |
| `sudo sse doctor` | Diagnose and suggest what to do |
| `sudo sse logs [indexer\|engine\|filter\|sse] [-n 50] [-f]` | Show logs |
| `sse search WORD [--explain]` | Try a search and see the scores |
| `sse tag add\|rm\|list TAG [ID…]` | Your own tags (`sse search` shows photo ids) |
| `sse retry-failed` | Retry photos that failed 3 times |
| `sudo sse apply [--check]` | Apply `config.yml` changes |
| `sudo sse disable` / `enable` | Emergency switch: stop / resume adding results (takes effect within 5 seconds) |
| `sudo sse bypass on` / `off` | Point nginx straight at DSM / back through the filter |
| `sudo sse reapply` | Re-apply the nginx change after a DSM update |
| `sudo sse capture on\|show\|off` | Record search requests for troubleshooting (no cookies or session ids) |
| `sudo sse update [--check]` | Update to the latest version / only check for one |
| `sudo sse uninstall [--purge]` | Remove everything |

Commands without `sudo` must be run as the photo owner. With `sudo` they switch to the owner automatically, so the
index files never end up owned by root.

## Updating

```sh
sudo sse update --check    # is there a new version?
sudo sse update            # update; config.yml and the index are kept
```

Versions are named by date, `vYYYY.MM.DD+build.N`. There are two channels:

| Channel | |
|---|---|
| `release` (default) | Stable releases |
| `pre-release` | Every change merged into `main`, for testing |

Switch with `sudo sse update --channel pre-release` (or `release`); later updates stay on that channel. `sse update`
never downgrades: going back from a pre-release to an older release needs `--force`, or `--version TAG` for a
specific version.

An update installs the new version next to the old one and switches over only when everything works. If the new
version fails its checks (the services must start and DSM must answer), the old version is put back automatically.

The same works without the `sse` command, for example if it is broken:

```sh
curl -fsSL https://raw.githubusercontent.com/ExpTechTW/synology-photos-search-enhancement/main/install.sh | sudo sh -s -- update
```

## DSM and Photos updates

- **DSM update**: DSM may regenerate `nginx.conf`. The plugin then pauses (DSM works normally, without the plugin)
  until you check that everything is fine and run `sudo sse reapply`. `sudo sse doctor` tells you when this is needed.
- **Synology Photos update**: the response format may change, so the filter stops adding results automatically.
  Check that search works and run `sudo sse apply` to resume.

Between updates, a guard re-applies the nginx change whenever DSM regenerates `nginx.conf` (for example after
changing a web service setting).

## Uninstall

```sh
sudo sse uninstall            # asks whether to delete ~/sse as well (default: keep)
sudo sse uninstall --purge    # delete ~/sse without asking
```

or, without the `sse` command:

```sh
curl -fsSL https://raw.githubusercontent.com/ExpTechTW/synology-photos-search-enhancement/main/install.sh | sudo sh -s -- uninstall
```

Keeping `~/sse` (the index database and `config.yml`) means a later reinstall picks up where it left off, without
re-indexing. `--yes` never deletes it unless `--purge` is also given.

Search then goes back to what Synology Photos finds on its own (its object categories, people, places and tags).
Photos found through the text in them or by AI similarity no longer appear; for example, a word that only appears in
screenshots will find nothing. Reload any open Photos page and reopen the mobile app.

This stops and removes the services and restores `nginx.conf`. It also deletes `/usr/local/sse`,
`/usr/local/etc/sse`, `/var/log/sse` and `/usr/local/bin/sse`. Synology Photos and your photos are never touched.

### Manual restore

If the `sse` command itself no longer works:

```sh
# 1. Point nginx straight at DSM: in /etc/nginx/nginx.conf, replace everything from
#    "# >>> synology-search-enhancement" to "# <<< synology-search-enhancement <<<" (inclusive) with:
#        server unix:/run/synoscgi.sock;
#    (skip this if DSM has regenerated nginx.conf since; the block is then already gone)
sudo vi /etc/nginx/nginx.conf
sudo nginx -t && sudo nginx -s reload

# 2. Stop and remove the services (DSM 7 has systemd 219: no "disable --now")
UNITS="sse-nginx-guard.path sse-nginx-guard.timer sse-filter.service sse-engine.service sse-indexer.service"
sudo systemctl stop $UNITS
sudo systemctl disable $UNITS
sudo rm -f /etc/systemd/system/sse-*
sudo systemctl daemon-reload

# 3. Remove the files
sudo rm -rf /usr/local/sse /usr/local/etc/sse /usr/local/bin/sse /var/log/sse /etc/nginx/nginx.conf.sse-orig
```

`/etc/nginx/nginx.conf.sse-orig` is the file as it was before the change. Copy it back only if DSM has not
regenerated `nginx.conf` since.

## Troubleshooting

- Start with `sudo sse doctor`, then `sudo sse logs indexer` / `engine` / `filter`.
- Search behaves oddly: `sudo sse disable` gives you native search back within 5 seconds while you investigate.
- Anything looks wrong with DSM itself: `sudo sse bypass on` takes the filter out of the path entirely.
- Indexing paused: `sudo sse status` shows the reason (model server unreachable, DSM sign-in failed…); it retries on
  its own.

## Limitations

- Only the owner's Personal Space is indexed, one account per NAS. Shared Space and other users' photos are not.
- Accounts with 2-step verification are not supported.
- "Similar" is a score threshold, so expect some misses and some extras. Tune with `sse search WORD --explain`.

## Development

```sh
sh tests/run.sh          # everything (first run downloads Python 3.8 / 3.12 and builds nginx 1.23.1, as on DSM 7.3)
sh tests/run.sh quick    # without nginx
```

The tests use a fake DSM, a fake model server and a fake `systemctl`, together with a real nginx and DSM's
Python 3.8. `tests/test_install_sandbox.py` runs the real `install.sh` from install to uninstall, including `curl | sh`
installs and updates from a fake GitHub release.

Every push to `main` that passes the tests publishes a pre-release, and every push to `release` publishes a release
([.github/workflows/ci-release.yml](.github/workflows/ci-release.yml), packaged by
[scripts/package.sh](scripts/package.sh)).

## License

[GPL-3.0-or-later](LICENSE).

This project is not affiliated with or endorsed by Synology Inc. Synology and Synology Photos are trademarks of
Synology Inc.
