#!/bin/sh
# Installs CloudSync for the current user. It is shipped inside the Linux
# release archive next to the `cloudsync` program:
#   tar xzf cloudsync-linux-amd64.tar.gz && cd cloudsync-linux-amd64 && ./install.sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
bin="${HOME}/.local/bin"

if [ ! -x "$here/cloudsync" ]; then
	echo "install.sh: no 'cloudsync' program next to this script." >&2
	exit 1
fi

# Already installed: let `update` swap it, since it stops the running copy
# after its last upload, keeps the previous one, and goes back to it if the
# new one doesn't start.
if [ -x "$bin/cloudsync" ]; then
	exec "$here/cloudsync" update
fi

mkdir -p "$bin"
cp "$here/cloudsync" "$bin/cloudsync.new"
chmod 755 "$bin/cloudsync.new"
mv -f "$bin/cloudsync.new" "$bin/cloudsync"
echo "Installed CloudSync to $bin/cloudsync"

case ":$PATH:" in
*":$bin:"*) ;;
*)
	echo
	echo "$bin isn't on your PATH yet. Add this line to ~/.bashrc (or your shell's"
	echo "startup file), then open a new terminal:"
	echo "  export PATH=\"\$HOME/.local/bin:\$PATH\""
	;;
esac

cat <<'EOF'

Next steps:
  1. Save your Google sign-in client as ~/.config/cloudsync/client_secret.json
     (see the README, step 2).
  2. cloudsync login
  3. cloudsync init
  4. cloudsync plan   then   cloudsync sync
  5. cloudsync start
EOF
