# dot

Minimal dotfile manager written in Go.

## Build

```bash
go build -o dot .
```

Optional install:

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
