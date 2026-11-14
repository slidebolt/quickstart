# Git Workflow for quickstart

This repository publishes the standalone native SlideBolt installer and terminal.

- Run `go test ./...` and `go build ./cmd/slidebolt` before publishing.
- Release tags publish `slidebolt_linux_amd64`, `slidebolt_linux_arm64`, and SHA256 checksums.
- Never commit generated sandboxes, secrets, logs, device data, local addresses, or household configuration.
- Quickstart consumes released catalogs and artifacts only; it must not import plugin packages or dispatch on plugin IDs.

