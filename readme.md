# ghost

> Watch over things + stream your mac to remote server

Currently this daemon is WIP but I already use it for one thing, to watch over my [Karabiner](https://nikiv.dev/karabiner) config.

## Setup

Install [task](https://taskfile.dev/docs/installation). Then run `task setup` & follow instructions until it says `✔️ you are setup`.

## Commands

Run `task` to see all possible commands.

## Run the daemon

1. Create `~/.config/ghost/ghost.toml` (or point `GHOST_CONFIG` at your preferred path) with watchers you care about. Example:

   ```toml
   [[watchers]]
   name = "karabiner"
   path = "/Users/nikiv/config/i/karabiner"
   match = "karabiner.edn"
   command = "/Users/nikiv/bin/goku"
   debounce_ms = 150
   run_on_start = true
   ```

   To keep long-running processes alive (for example dev servers or tunnels), add `[[servers]]` entries. Ghost will launch each server on start, restart it if it exits, and capture all TTY input/output into a log file.

   ```toml
   [[servers]]
   name = "web"
   cwd = "/Users/nikiv/code/web-app"
   command = "npm run dev"
   restart = true            # default
   restart_delay_ms = 500    # optional
   log_path = "~/logs/web.log" # optional; defaults to ~/.local/state/ghost/servers/<name>.log
   pty = true                # default; makes the process believe it's in a terminal
   ```

   Every server stream is mirrored to the daemon output and appended to the configured log (or the default under `~/.local/state/ghost/servers`). Set `pty = false` for programs that should run without a pseudo-terminal.

   To stream your Mac to a remote server with OBS while avoiding sensitive apps, configure the `streaming` table. Ghost connects to obs-websocket, auto-starts streaming (optional) and swaps to a dedicated privacy scene whenever any excluded application is visible.

   ```toml
   [streaming]
   enabled = true
   obs_host = "127.0.0.1:4455"     # obs-websocket host (scheme optional, ws:// by default)
   obs_password = "super-secret"   # optional
   live_scene = "Desktop"
   privacy_scene = "Ghost Privacy"
   auto_start = true               # start OBS streaming once connected
   exclude_applications = ["Telegram", "Spark", "1Password"]
   privacy_mode = "onscreen"       # or "frontmost" if you only care about the active app
   poll_interval_ms = 250          # optional, defaults to 250ms
   ```

   Requirements:

   - OBS 28+ with obs-websocket enabled (Tools → WebSocket Server Settings). Point `obs_host` at that listener; use `wss://...` if you proxy TLS.
   - Create two OBS scenes (`live_scene` for normal streaming, `privacy_scene` for the standby slate). Ghost simply flips between them; layout/design is up to you.
   - Grant `ghost` screen-recording + accessibility permissions in macOS so it can enumerate on-screen windows and detect excluded apps.

   When any excluded application owns a visible window, Ghost switches to the privacy scene so the content never leaves your machine. As soon as those apps leave the screen, Ghost automatically reverts to the live scene and keeps the stream running to your remote destination.

2. From the repo run `task run` to start the Go daemon directly, or `task deploy` to install a `ghost` binary to `~/bin`.
3. Save the config file you pointed at and ghost will hot-reload watchers automatically.

## Contributing

Any PR to improve is welcome. [codex](https://github.com/openai/codex) & [cursor](https://cursor.com) are nice for dev. Great **working** & **useful** patches are most appreciated (ideally). Issues with bugs or ideas are welcome too.

### 🖤

[![Discord](https://go.nikiv.dev/badge-discord)](https://go.nikiv.dev/discord) [![X](https://go.nikiv.dev/badge-x)](https://x.com/nikivdev) [![nikiv.dev](https://go.nikiv.dev/badge-nikiv)](https://nikiv.dev)
