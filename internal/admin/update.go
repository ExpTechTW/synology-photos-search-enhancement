package admin

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/app"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/state"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/version"
)

// ── 更新：從 GitHub release 下載 ─────────────────────────────────────────
//
// 版本名稱是 vYYYY.MM.DD+build.N。兩個管道：release（穩定版，預設）與 pre-release（main 分支的每次更新）。
// 下載的新版放在 /usr/local 底下的暫存目錄（DSM 的 /tmp 掛載成 noexec，執行不了），驗過 sha256 後
// 由新版自己執行 sse install：安裝流程的修正跟著新版一起生效，失敗時換回原本的程式（見 selfInstall）。

const repo = "ExpTechTW/synology-photos-search-enhancement"

// githubAPI 可以用 SSE_GITHUB_API 換掉（測試用的假 GitHub）。
var githubAPI = envOr("SSE_GITHUB_API", "https://api.github.com/repos/"+repo)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

var tagRe = regexp.MustCompile(`^v?(\d{4})\.(\d{2})\.(\d{2})\+build\.(\d+)$`)

// parseVersion：認不得的版本（例如從原始碼安裝的開發版 0.0.0+dev）比任何 release 舊。
func parseVersion(tag string) [4]int {
	var v [4]int
	if m := tagRe.FindStringSubmatch(strings.TrimSpace(tag)); m != nil {
		for i := range v {
			v[i], _ = strconv.Atoi(m[i+1])
		}
	}
	return v
}

// compareVersion：tag 比 installed 新 / 相同 / 舊。
func compareVersion(installed, tag string) string {
	a, b := parseVersion(installed), parseVersion(tag)
	for i := range a {
		switch {
		case b[i] > a[i]:
			return "newer"
		case b[i] < a[i]:
			return "older"
		}
	}
	return "same"
}

// assetName 是這台機器要下載的檔案，例如 sse-linux-amd64.tar.gz。
func assetName() string {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	return "sse-" + runtime.GOOS + "-" + arch + ".tar.gz"
}

// pickRelease 依管道（或指定的版本）選出要安裝的版本，回傳 tag 與安裝檔、sha256 的網址。
func pickRelease(rels []release, channel, want string) (tag, url, shaURL string, err error) {
	var cand []release
	for _, r := range rels {
		switch {
		case r.Draft:
		case want != "":
			if strings.TrimPrefix(r.Tag, "v") == strings.TrimPrefix(want, "v") {
				cand = append(cand, r)
			}
		case channel == "release" && r.Prerelease:
		default:
			cand = append(cand, r)
		}
	}
	if len(cand) == 0 {
		if want != "" {
			return "", "", "", fmt.Errorf("no matching release / 找不到版本 %s", want)
		}
		return "", "", "", fmt.Errorf("no matching release / 找不到 %s 的版本", channel)
	}
	best := cand[0]
	for _, r := range cand[1:] {
		if compareVersion(best.Tag, r.Tag) == "newer" {
			best = r
		}
	}
	name := assetName()
	for _, a := range best.Assets {
		switch a.Name {
		case name:
			url = a.URL
		case name + ".sha256":
			shaURL = a.URL
		}
	}
	if url == "" || shaURL == "" {
		return "", "", "", fmt.Errorf("release has no package / 版本 %s 沒有 %s", best.Tag, name)
	}
	return best.Tag, url, shaURL, nil
}

var githubClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}

func httpGet(url string, timeout time.Duration) (*http.Response, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "sse/"+version.Version)
	resp, err := githubClient.Do(req)
	if err == nil && resp.StatusCode != 200 {
		resp.Body.Close()
		err = fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return resp, cancel, nil
}

func fetchReleases() ([]release, error) {
	resp, cancel, err := httpGet(githubAPI+"/releases?per_page=100", 60*time.Second)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer resp.Body.Close()
	var rels []release
	err = json.NewDecoder(resp.Body).Decode(&rels)
	return rels, err
}

func download(url, dest string) error {
	resp, cancel, err := httpGet(url, 15*time.Minute)
	if err != nil {
		return err
	}
	defer cancel()
	defer resp.Body.Close()
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// verifySHA256：shaFile 是 sha256sum 的輸出（雜湊值、空白、檔名）。
func verifySHA256(file, shaFile string) bool {
	want, err := os.ReadFile(shaFile)
	if err != nil {
		return false
	}
	fields := strings.Fields(string(want))
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil || len(fields) == 0 {
		return false
	}
	return strings.EqualFold(fields[0], hex.EncodeToString(h.Sum(nil)))
}

// extractBinary 從安裝檔解出 sse 執行檔，放到 dir/sse。
func extractBinary(tarball, dir string) (string, error) {
	f, err := os.Open(tarball)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return "", errors.New("安裝檔裡沒有 sse")
		}
		if err != nil {
			return "", err
		}
		if strings.TrimPrefix(h.Name, "./") != "sse" || h.Typeflag != tar.TypeReg {
			continue
		}
		dest := filepath.Join(dir, "sse")
		out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(dest, 0o755)
		}
		return dest, err
	}
}

// makeWorkDir 建立放下載檔的暫存目錄：跟 /usr/local/sse 在同一個檔案系統（/tmp 是 noexec），
// 其他帳號也要能執行裡面的 sse（安裝前檢查以 nginx 的帳號執行它）。
func makeWorkDir() string {
	parent := filepath.Dir(paths.Prefix)
	must(os.MkdirAll(parent, 0o755))
	d, err := os.MkdirTemp(parent, ".sse-work.")
	must(err)
	must(os.Chmod(d, 0o755))
	return d
}

func isInstalled(st *state.State) bool {
	return exists(paths.Binary) || exists(filepath.Join(paths.Prefix, "venv/bin/python")) || (st != nil && st.Version != "")
}

// installedVersion：已安裝的程式的版本（Python 版沒有 sse 執行檔，看安裝紀錄）。
func installedVersion(st *state.State) string {
	if exists(paths.Binary) {
		if code, out := run([]string{paths.Binary, "--version"}, 10*time.Second); code == 0 && out != "" {
			return strings.SplitN(out, "\n", 2)[0]
		}
	}
	if st != nil {
		return st.Version
	}
	return ""
}

const updateHelp = `用法：sse update [--check] [--force] [--channel release|pre-release] [--version TAG]

  --check             只檢查有沒有新版
  --force             版本相同或較舊也重新安裝
  --channel CHANNEL   release（穩定版）或 pre-release（main 分支的每次更新）；之後的更新沿用
  --version TAG       安裝指定版本（例如 v2026.10.26+build.1）
`

// cmdUpdate：sudo sse update。
func cmdUpdate(g app.Global, args []string) int {
	requireRoot()
	return update(g, "update", args, false, true)
}

// cmdBootstrap：install.sh 下載的 sse（或 SSE_BINARY 指定的本機程式）由這裡接手。
//
//	sse bootstrap [--local] install|update [選項]
//
// --local：安裝的就是這個執行檔本身（開發用），不查 GitHub。
func cmdBootstrap(g app.Global, args []string) int {
	requireRoot()
	local := len(args) > 0 && args[0] == "--local"
	if local {
		args = args[1:]
	}
	if len(args) == 0 || (args[0] != "install" && args[0] != "update") {
		panic(usageError("用法：sse bootstrap [--local] install|update [選項]"))
	}
	return update(g, args[0], args[1:], local, false)
}

// update 是 sse update 與 install.sh（curl | sh）共用的流程：選版本、下載、交給新版安裝。
// strict：sse update 只接受自己的選項；install.sh 傳來的其他選項（--user 之類）交給 sse install。
func update(g app.Global, cmd string, args []string, local, strict bool) int {
	var channel, want string
	var force, check, dry bool
	var rest []string // 其他選項交給 sse install
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		switch name {
		case "--channel", "--version":
			if !hasVal {
				if i+1 >= len(args) {
					panic(usageError(name + " 要指定值"))
				}
				i++
				val = args[i]
			}
			if name == "--channel" {
				channel = val
			} else {
				want = val
			}
		case "--force":
			force = true
		case "--check":
			check = true
		case "-h", "--help":
			fmt.Print(updateHelp)
			return 0
		default:
			if strict {
				panic(usageError("不認得的選項 " + a))
			}
			dry = dry || a == "--dry-run"
			rest = append(rest, a)
		}
	}
	if channel != "" && channel != "release" && channel != "pre-release" {
		fail("--channel must be release or pre-release / --channel 只能是 release 或 pre-release")
	}
	st := loadState()
	installed := isInstalled(st)
	if cmd == "update" && !installed {
		fail("not installed / 還沒安裝：curl -fsSL … | sudo sh")
	}
	if channel == "" && st != nil {
		channel = st.Channel
	}
	if channel == "" {
		channel = "release"
	}
	if cmd == "update" && !dry {
		rest = append([]string{"--yes"}, rest...) // 要求更新就是同意了，不再詢問
	}
	exe := selfExe()
	if local {
		sayf("Installing the local build (%s) / 安裝本機的程式（%s）", version.Version, version.Version)
		return cmdInstall(g, append([]string{"--channel", channel, "--source", "local"}, rest...))
	}

	rels, err := fetchReleases()
	if err != nil {
		fail("cannot reach GitHub / 連不上 GitHub：%s（%v）", githubAPI, err)
	}
	tag, url, shaURL, err := pickRelease(rels, channel, want)
	if err != nil {
		fail("%v", err)
	}
	if installed && want == "" {
		cur := installedVersion(st)
		rel := compareVersion(cur, tag)
		shown := cur
		if shown == "" {
			shown = "?"
		}
		sayf("Installed / 目前：%s   latest %s / 最新版：%s", shown, channel, tag)
		if check {
			if rel == "newer" {
				say("New version available / 有新版：sudo sse update")
			} else {
				say("Up to date / 已是最新版本")
			}
			return 0
		}
		if cmd == "update" && !force {
			switch rel {
			case "same":
				say("Up to date / 已是最新版本")
				return 0
			case "older":
				sayf("%s is older than the installed version; not downgrading (add --force to do it anyway)", tag)
				sayf("%s 比目前安裝的版本舊，不降版（要降版請加 --force）", tag)
				return 0
			}
		}
	} else if check {
		sayf("latest %s / 最新版：%s", channel, tag)
		return 0
	}

	installArgs := append([]string{"--channel", channel, "--source", tag}, rest...)
	// install.sh 剛下載、驗過的就是這個版本：直接用，不必再下載一次
	if !sameFile(exe, paths.Binary) && strings.TrimPrefix(tag, "v") == version.Version {
		return cmdInstall(g, installArgs)
	}
	work := makeWorkDir()
	defer os.RemoveAll(work)
	sayf("Downloading %s / 下載 %s", tag, tag)
	tarball := filepath.Join(work, assetName())
	if err := download(url, tarball); err != nil {
		fail("download failed / 下載失敗：%s（%v）", url, err)
	}
	if err := download(shaURL, tarball+".sha256"); err != nil {
		fail("download failed / 下載失敗：%s（%v）", shaURL, err)
	}
	if !verifySHA256(tarball, tarball+".sha256") {
		fail("checksum mismatch / sha256 不符")
	}
	bin, err := extractBinary(tarball, work)
	if err != nil {
		fail("bad package / 安裝檔內容不對：%v", err)
	}
	if code, out := run([]string{bin, "--version"}, 30*time.Second); code != 0 {
		fail("bad package / 安裝檔內容不對（%s 執行不了：%s）", assetName(), out)
	}
	childArgs := []string{"install"}
	if g.Verbose {
		childArgs = []string{"-v", "install"}
	}
	return runChild(bin, append(childArgs, installArgs...)...)
}
