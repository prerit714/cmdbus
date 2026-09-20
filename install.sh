#!/usr/bin/env sh
# Install cmdbus: clone (or update) the source, build it, put it on your PATH.
#
#   sh install.sh
#
# The repository is private, so git must be able to reach it (SSH key or a
# credential helper). Override any of these with environment variables:
#
#   CMDBUS_REPO  git URL to clone       (default: git@github.com:prerit714/cmdbus.git)
#   CMDBUS_SRC   where the source goes  (default: ~/.local/share/cmdbus)
#   CMDBUS_BIN   where the binary goes  (default: ~/.local/bin)
set -eu

REPO="${CMDBUS_REPO:-git@github.com:prerit714/cmdbus.git}"
SRC="${CMDBUS_SRC:-$HOME/.local/share/cmdbus}"
BIN="${CMDBUS_BIN:-$HOME/.local/bin}"

for tool in git go; do
	command -v "$tool" >/dev/null 2>&1 || { echo "install.sh: '$tool' is required but was not found" >&2; exit 1; }
done

# 1. Clone, or fast-forward an existing clone.
if [ -d "$SRC/.git" ]; then
	echo "==> updating $SRC"
	git -C "$SRC" pull --ff-only
else
	echo "==> cloning $REPO into $SRC"
	mkdir -p "$(dirname "$SRC")"
	git clone "$REPO" "$SRC"
fi

# 2. Build the single binary.
echo "==> building $BIN/cmdbus"
mkdir -p "$BIN"
(cd "$SRC" && go build -o "$BIN/cmdbus" .)

# 3. Put $BIN on the user's PATH, once, in the startup file of their shell.
case ":$PATH:" in
*":$BIN:"*)
	echo "==> $BIN is already on your PATH"
	;;
*)
	case "$(basename "${SHELL:-sh}")" in
	zsh) rc="$HOME/.zshrc" line="export PATH=\"$BIN:\$PATH\"" ;;
	bash) rc="$HOME/.bashrc" line="export PATH=\"$BIN:\$PATH\"" ;;
	fish) rc="$HOME/.config/fish/config.fish" line="fish_add_path \"$BIN\"" ;;
	*) rc="$HOME/.profile" line="export PATH=\"$BIN:\$PATH\"" ;;
	esac
	if [ -f "$rc" ] && grep -qF "$line" "$rc"; then
		echo "==> $rc already adds $BIN to your PATH"
	else
		mkdir -p "$(dirname "$rc")"
		printf '\n# added by cmdbus install.sh\n%s\n' "$line" >>"$rc"
		echo "==> added $BIN to your PATH in $rc"
	fi
	echo "    open a new terminal, or run:  $line"
	;;
esac

echo "==> installed. Try:  cmdbus -h   |   cmdbus docs"
