# Squire Link

Squire Link is a small local proxy for [Squire](https://github.com/neostryder/neo-angband-mod-squire), the Neo Angband autoplayer mod. It lets Squire running in a browser send model requests to Jev or to a Laya server on your network. The desktop version of Neo Angband already connects to model servers, so desktop players do not need Squire Link.

Browser pages cannot send Jev's required request headers or connect directly to a server on your home network. Squire Link runs on the same computer as the browser game, accepts requests on loopback, and forwards them to the configured server. It can also read Twitch chat or a Discord channel and provide matching viewer orders to Squire.

## Download and run

Download the release asset that matches your operating system and processor from the [Squire Link releases page](https://github.com/neostryder/squire-link/releases). The file names identify both, such as `squire-link-windows-amd64.exe` for 64-bit Intel or AMD Windows and `squire-link-darwin-arm64` for Apple silicon macOS. Windows ARM and Linux ARM builds are also provided.

On Windows, open PowerShell in the download folder and run `./squire-link-windows-amd64.exe`. Use the `arm64.exe` file on Windows ARM. On macOS, open Terminal in the download folder, run `chmod +x ./squire-link-darwin-arm64`, then run `./squire-link-darwin-arm64`; use `darwin-amd64` on an Intel Mac. On Linux, make the matching `linux-amd64` or `linux-arm64` file executable with `chmod +x`, then run it with `./` and its file name.

Run `squire-link --version` to print the embedded version. Starting `squire-link` prints the config file path and listening address. Keep its terminal open while playing. By default it listens at `127.0.0.1:8765`. In Squire's Setup tab, choose "Another server" and enter `http://127.0.0.1:8765/v1/systemone/jev` for Jev or `http://127.0.0.1:8765/v1/systemone/laya` for Laya. Enter `http://127.0.0.1:8765/v1/orders` as the channel address when using chat orders.

Squire Link is a separate program. Squire does not start it automatically. Start Squire Link before opening or using the browser game, then configure the model and channel addresses in Squire's Setup tab.

## API keys and commands

Store a Jev key in your operating system keychain by running `squire-link key set jev` and entering the key at the prompt. `squire-link key list` shows whether configured keys are stored, and `squire-link key delete jev` removes a key. The key prompt hides typing in a terminal. A piped key comes from one line. After storing it, Squire Link reports its length.

The first run creates a config file in the operating system's user config folder. Run `squire-link routes` to print its path and configured routes. Pass `--config PATH` to `run`, `routes`, or `key list` to use another file. Run `squire-link --help` for command usage.

## Configuration

Squire Link creates `config.json` with these initial settings:

```json
{
  "routes": {
    "jev": { "target": "https://api.typesafe.ai/v1/systemone", "key": "jev" },
    "laya": { "target": "http://localhost:8010/v1/systemone" }
  },
  "origins": ["https://angband.rpgm.world", "https://*.itch.zone", "http://localhost:*", "http://127.0.0.1:*"],
  "channel": {
    "prefix": "!squire",
    "cooldownSeconds": 60,
    "allow": [],
    "block": [],
    "twitch": { "enabled": false, "channel": "" },
    "discord": { "enabled": false, "channelId": "" }
  }
}
```

Each entry in `routes` names a route in Squire's model URL. A route has a required `target`, optional `fallbacks` list, and optional `key` name. Targets and fallbacks must be HTTP or HTTPS URLs without credentials, query strings, or fragments. A key name refers to a value in the operating system keychain. Squire Link sends that value as a Bearer authorization header to that route's server. Leave `key` out for a server that needs no key. For example, set the Laya target to `http://192.168.1.20:8010/v1/systemone` to reach Laya on another computer.

For a route with multiple addresses, Squire Link tries them in order. Before trying each one, it checks the server's `/load` response when available and skips servers marked busy or not ready for five seconds. It skips a server for 30 seconds if it cannot answer or its load check fails. When a request gets a server error (HTTP 5xx), Squire Link tries the next address. It does not retry client errors (HTTP 4xx). Each request is at most 1 MiB and can take up to 30 seconds.

`origins` lists browser origins allowed to call the proxy. An origin has a scheme and host, with an optional port. A `*.` host prefix matches subdomains, and a `:*` suffix matches any port. Add the game's origin if it is not listed. Origins control browser access; they are not a password for other programs on your computer.

The `channel` object has `prefix`, `cooldownSeconds`, `allow`, `block`, `twitch`, and `discord` fields. The prefix is a word with no spaces and defaults to `!squire`. The cooldown is from 0 to 86400 seconds and defaults to 60. `allow` and `block` are lists of viewer names; names match without regard to case, and a non-empty allow list admits only listed names. A blocked name is always ignored. Older config files without `channel` use these defaults with both chat services disabled.

Set `twitch.enabled` to true and `twitch.channel` to the streamer's channel name to read Twitch chat anonymously. This connection can read chat but cannot write to it and uses no token. Set `discord.enabled` to true and `discord.channelId` to the channel's numeric ID to read Discord. The Discord bot needs the Message Content intent and View Channel and Read Message History permissions. Store its token with `squire-link key set discord`. The channel ID must contain 5 to 25 digits. Squire Link ignores messages from bots. Both integrations are off by default.

Chat messages become orders when they start with the prefix and include text. Squire Link removes the prefix, cleans up extra spaces, removes control characters, and keeps at most 300 characters.

Orders stay in memory until Squire reads `/v1/orders`. The game gets up to 50 orders at a time, oldest first. Squire Link says when more remain. Its queue holds 100 orders and pushes out the oldest when full. Discord checks for new messages every five seconds and skips older messages when it starts. Twitch reconnects after a lost connection.

## Security and privacy

Squire Link binds only to a loopback IP address, so it does not listen for connections from other computers. In `--mode serve`, model and order requests require the `X-Squire-Link-Token` header, and every request without a browser origin requires it too. The proxy sends requests only to configured targets and fallbacks, not to an address supplied by a game page.

API keys, the Discord bot token, and the optional link token are stored in the operating system keychain. The config file contains route names and addresses, origin rules, and chat settings, but no secret values. Twitch chat uses an anonymous read-only connection. Chat orders stay in memory and are lost when Squire Link exits.

Logs include route names, target addresses, response status, response byte counts and timing, plus the platform and viewer name for accepted chat orders. Logs do not include API key values, Discord or link tokens, request or response bodies, or chat order text. The viewer name is included so you can see which account submitted an order.

## Troubleshooting

If the browser reports a connection error, start Squire Link and confirm it prints `127.0.0.1:8765`; keep it open and check that Squire's server URL uses the same port. Open `http://127.0.0.1:8765/health` to see whether the proxy answers, its mode, routes, and version. If Squire Link reports that an origin is not allowed, add the page's exact scheme and host to `origins`, then restart Squire Link.

If a route says its key is unavailable, run `squire-link key list` and store the missing key with `squire-link key set NAME`. If Laya is unreachable, check its target URL, that Laya is running, and that both computers can reach one another on the network. `squire-link routes` prints each configured target and key status.

For Discord, check the bot token, channel ID, Message Content intent, and channel permissions. Discord errors are printed in the terminal. For Twitch, check the channel spelling and network connection; the terminal reports reconnect attempts. A chat order must use the prefix, pass the allow and block lists, and satisfy the viewer cooldown.

## Building from source

Install Go 1.26 or later, then run `go vet ./...`, `go test ./...`, and `go run build.go 1.0.0`. The build writes six standalone files to `dist/`: Windows amd64 and arm64, macOS darwin amd64 and arm64, and Linux amd64 and arm64. Windows files end in `.exe`; other file names end in the OS and architecture. The supplied version is embedded in each build and printed by `--version`.

## License

Squire Link is licensed under the GNU General Public License, version 3. See [LICENSE](LICENSE).
