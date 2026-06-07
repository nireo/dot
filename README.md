# dot

Minimal dotfile manager written in Ruby. For some reason every time I tried to setup GNU stow it didn't work as I wanted it to. So I mainly wrote this in the way that I understand how these systems work. `dot` holds a mapping from folders/files to where they should be on disk, from this mapping it creates symlinks to the correct places such that the configurations can by read by different software. All in a few hundred lines of easily understandable Ruby code.

## Install

```bash
./install.sh
```

## Setup

`dot` uses `$DOTFILES` as the repo path. If not set, it defaults to `~/.dotfiles`.

Example:

```bash
export DOTFILES="$HOME/.dotfiles"
mkdir -p "$DOTFILES"
cd "$DOTFILES"
git init
```

## Usage

Track a file into your dotfiles repo:

```bash
dot track ~/.bashrc shell/.bashrc
```

Track a directory (including subdirectories) into your dotfiles repo:

```bash
dot track ~/.config/nvim nvim
```

Track a nested config without tracking its whole parent directory. A target path
ending in `/` is treated as a repo directory and the source basename is appended:

```bash
dot track ~/.pi/agents pi/
# stores it as $DOTFILES/pi/agents and maps that to ~/.pi/agents
```

Apply all mappings from `.dot.map`:

```bash
dot link
```

Show status of tracked files (`OK`, `STRAY`, `MISSING`, `BROKEN`):

```bash
dot list
```

Ignore files in tracked directories with `.dot-local-ignore`
(or `.stow-local-ignore`), one regex per line, for example `^/README.*`.

Add, commit, and push repo changes:

```bash
dot sync
```
