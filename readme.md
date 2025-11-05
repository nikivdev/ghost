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

2. From the repo run `task run` to start the Go daemon directly, or `task deploy` to install a `ghost` binary to `~/bin`.
3. Save the config file you pointed at and ghost will hot-reload watchers automatically.

## Contributing

Any PR to improve is welcome. [codex](https://github.com/openai/codex) & [cursor](https://cursor.com) are nice for dev. Great **working** & **useful** patches are most appreciated (ideally). Issues with bugs or ideas are welcome too.

### 🖤

[![Discord](https://go.nikiv.dev/badge-discord)](https://go.nikiv.dev/discord) [![X](https://go.nikiv.dev/badge-x)](https://x.com/nikivdev) [![nikiv.dev](https://go.nikiv.dev/badge-nikiv)](https://nikiv.dev)
