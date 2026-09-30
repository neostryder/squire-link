# Changelog

All notable changes to Squire Link are recorded here, and each released version has a matching git tag.

Each entry opens with `[Visible]` for a change someone running Squire Link would notice, or `[Internal]` for one that touches only code or tooling. A release lists its changes under Added, Changed, Removed and Fixed, in that order, leaving out any that are empty.

## [Unreleased]

### Added

- [Visible] **Orders from chat.** Squire Link can read Twitch chat, anonymously, and one Discord channel, through a bot token kept with `squire-link key set discord`, and pass on every message that starts with `!squire` as an order for Squire to collect from `/v1/orders`. Messages are cut at 300 characters, and allow and block lists plus a per-viewer cooldown keep a busy chat in check. Both are off until you turn them on in the config file.
