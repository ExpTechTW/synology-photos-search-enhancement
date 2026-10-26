// Package version：程式版本。release 時由 CI 用 -ldflags 改成 YYYY.MM.DD+build.N（跟 git tag 相同）。
package version

var Version = "0.0.0+dev"
