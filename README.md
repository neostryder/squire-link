# Squire Link

Squire Link lets [Squire](https://github.com/neostryder/neo-angband-mod-squire), the autoplayer mod for Neo Angband, reach its model server when you play in a web browser. The desktop app already does this for you, so if you play there you don't need Squire Link.

A game page in a browser can't call Jev, because Jev doesn't send the headers a browser requires, and it can't reach a server on another computer in your home. Squire Link runs on your own computer and listens only on `127.0.0.1`. The game page sends its requests to it, and it forwards them to the servers you list in its config file. When a server needs an API key, Squire Link adds the key from your system keychain, so the game page never holds it.

## Running it

Download the file for your system from the releases page and run it from a terminal:

```
squire-link
```

It prints where its config file is and starts listening on `127.0.0.1:8765`. Leave the terminal open while you play. In Squire's Setup tab, choose "Another server" and enter `http://127.0.0.1:8765/v1/systemone/jev` for Jev, or `http://127.0.0.1:8765/v1/systemone/laya` for Laya.

To store your Jev key in the system keychain:

```
squire-link key set jev
```

Paste the key and press Enter. Squire Link prints only its length. `squire-link key list` shows which keys are set, and `squire-link key delete jev` removes one.

## The config file

The first run creates this file in your user config folder (`squire-link routes` prints its path):

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

Each route is a name you use in the address and the server it forwards to. `key` names the keychain entry to send as the API key; leave it out for a server that needs none. Point `laya` at the computer that runs Laya, for example `http://192.168.1.20:8010/v1/systemone`.

If you run Laya on more than one computer, add the others as `fallbacks`, for example `"fallbacks": ["http://192.168.1.21:8010/v1/systemone"]`. Squire Link tries the target first and moves to the next address when a server is busy, reports that it is not ready, answers with a server error, or does not answer; it skips that server for a few seconds, or for half a minute if it did not answer. A request a server refuses outright is not sent anywhere else. The log names the server that answered.

`origins` lists the web pages allowed to use Squire Link. `*.` at the start of a host matches any subdomain, and `:*` matches any port. A page that isn't listed gets an error naming the config file, so if you play from somewhere else, add its address here.

Squire Link forwards only to the servers in this file, never to an address a page asks for.

## Orders from chat

Viewers of your stream can give the squire orders from Twitch chat or a Discord channel. Both are off until you turn them on in the `channel` section of the config file, and a config file from an older Squire Link reads as if the section were there with both off.

A chat message becomes an order only when it starts with the prefix, `!squire` unless you change `prefix`, for example `!squire run from uniques`. The prefix is removed and the rest is kept, up to 300 characters. Each viewer can give one order every `cooldownSeconds` (60 unless you change it; 0 turns the wait off). Names in `block` are never taken. When `allow` lists any names, only those viewers are taken. Names are matched without regard to case: the Twitch login name, or the Discord user name.

- **Twitch.** Set `"twitch": { "enabled": true, "channel": "yourname" }`. Squire Link reads the channel's chat anonymously and can't write to it, so it needs no token.
- **Discord.** Create a bot in the Discord developer portal, turn on its Message Content intent, and invite it to your server with the View Channel and Read Message History permissions on the channel. Store its token with `squire-link key set discord`, then set `"discord": { "enabled": true, "channelId": "123456789012345678" }`, using the channel's ID (turn on Developer Mode in Discord, then right-click the channel and choose Copy Channel ID). Squire Link checks the channel for new messages every 5 seconds and skips messages from bots. Messages written before it started are not taken.

Squire Link keeps the orders until Squire collects them from `http://127.0.0.1:8765/v1/orders`, the address to enter as the channel address in Squire's Setup tab. Each read returns up to 50 orders, oldest first, as `{"orders":[...],"more":true}` (or `false` when none remain), and removes only the orders returned. Squire keeps reading while `more` is true. Squire Link keeps at most the latest 100 waiting orders; when the queue is full, it drops the oldest and logs the running drop count. The log shows who gave each order, but not its words.

## Building from source

With Go 1.26 or later:

```
go test ./...
go run build.go 1.0.0
```

The binaries for Windows, macOS and Linux, on x64 and ARM, land in `dist/`.

## License

Squire Link is licensed under the GNU General Public License, version 3. See [LICENSE](LICENSE).
