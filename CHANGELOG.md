# Changelog

Squire Link release notes appear below by version. Each released version has a matching git tag.

Each entry opens with `[Visible]` for a change someone running Squire Link would notice, or `[Internal]` for a change that touches only code or tooling. Category labels follow the visibility label. A release lists Added, Changed, Removed, and Fixed entries in that order, leaving out empty categories.

## [Unreleased]

### Added

- [Visible] [Added] **Browser model access.** Squire Link accepts local requests from Squire and forwards them to configured Jev or Laya servers, adding API keys from the operating system keychain when needed.
- [Visible] [Added] **Local access controls.** The proxy listens on loopback, checks browser origins, and requires a keychain link token for model and order requests in serve mode.
- [Visible] [Added] **Fallback servers.** Routes can list fallback addresses that Squire Link tries when a server is busy, not ready, unavailable, or returns a server error.
- [Visible] [Added] **Health check.** The `/health` address reports whether the proxy is running, its mode, configured routes, and version.
- [Visible] [Added] **Chat orders.** Squire Link reads Twitch chat anonymously or polls one Discord channel with a keychain-stored bot token. It queues prefixed messages for Squire, applies allow and block lists with a per-viewer cooldown, and keeps at most 100 waiting orders.
- [Visible] [Added] **Local setup commands.** The program creates a default config file and provides commands to inspect routes and manage keychain entries.
- [Visible] [Added] **Version output.** Builds embed the supplied version and print it with `--version`.
- [Visible] [Added] **Platform builds.** The build script creates standalone Windows, macOS, and Linux binaries for amd64 and arm64.
- [Visible] [Added] **Player documentation.** The README explains downloads, setup, configuration, security, and troubleshooting; SECURITY.md describes reporting and exposure.

### Changed

- [Internal] [Changed] **Release checks.** The initial public release is documented for Go 1.26 or later and uses `go vet ./...` and `go test ./...` as repository checks.
