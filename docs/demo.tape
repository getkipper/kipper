# Records the README demo: a bare Linux server to a running app.
#
#   brew install vhs
#   export KIPPER_DEMO_HOST=<ip of a throwaway server>
#   export KIPPER_DEMO_KEY=~/.ssh/id_ed25519
#   export KIPPER_DEMO_NAME=<a free kipper.run label, e.g. demo>
#   vhs docs/demo.tape
#
# Run it against a server you are willing to destroy, and destroy it afterwards.
# The recording installs a real cluster and deploys a real app.
#
# Two things to know before recording:
#
#   1. `kip install` prints the cluster's admin password. The sed filter below
#      replaces that line before it reaches the screen, so the GIF never carries
#      a live credential. If the run is interrupted, rotate the password anyway.
#   2. The installer writes to /usr/local/bin and calls sudo when it cannot.
#      vhs cannot answer a sudo prompt, so it waits for output that never comes
#      and gives up with no recording. Own the directory before you start
#      (`sudo chown $(id -u) /usr/local/bin`) rather than working around it: the
#      GIF then shows the path a viewer would get.
#   3. `--admin-kubeconfig` is here because the recording is unattended. Piping
#      the install through the redaction filter leaves it without a terminal,
#      and `ResolveKubeconfigMode` defers the login in that case, which would
#      leave the next command with no credential. A person installing by hand
#      gets the interactive sign-in instead and does not need the flag.
#   4. `--domain` takes a free kipper.run label, which keeps the server's
#      address out of every URL on screen: console--demo.kipper.run rather than
#      console--203-0-113-10.kipper.run. Pick one that is unregistered, and
#      release it afterwards with `kip cluster uninstall`, or it stays claimed
#      for four months. Anything already taken is refused before the install
#      touches the server.
#
# Nothing here is staged. Every line is real output from a real run; the tape
# only compresses playback so that several minutes of install fit a README.

Require curl
Require ssh
Require sed

Output docs/public/demo.gif

Set Shell bash
Set FontSize 16
Set Width 1400
Set Height 820
Set Padding 24
Set Framerate 24
Set Margin 16
Set BorderRadius 8

# The install takes minutes of real time. Frames are captured at real speed and
# played back compressed. Raise this if the finished GIF is still too long.
Set PlaybackSpeed 8.0

# Typing is deliberately slow so it reads correctly once PlaybackSpeed compresses
# it: 400ms at 8x lands around 50ms on screen.
Set TypingSpeed 400ms

Set Theme { "name": "Kipper", "black": "#0f172a", "red": "#f87171", "green": "#34d399", "yellow": "#fbbf24", "blue": "#0ea5e9", "magenta": "#a78bfa", "cyan": "#22d3ee", "white": "#e2e8f0", "brightBlack": "#334155", "brightRed": "#fca5a5", "brightGreen": "#6ee7b7", "brightYellow": "#fcd34d", "brightBlue": "#38bdf8", "brightMagenta": "#c4b5fd", "brightCyan": "#67e8f9", "brightWhite": "#f8fafc", "background": "#0f172a", "foreground": "#e2e8f0", "selection": "#1e293b", "cursor": "#0ea5e9" }

Hide
Type "clear" Enter
Show

Sleep 1s
Type "curl -sL https://getkipper.com/install | sh"
Enter
Wait+Screen@3m /installed to/
Sleep 1s

Type "kip install --host $KIPPER_DEMO_HOST --ssh-key $KIPPER_DEMO_KEY --domain $KIPPER_DEMO_NAME.kipper.run --admin-email you@example.com --admin-kubeconfig 2>&1 | sed -E 's/^([[:space:]]*Password:[[:space:]]*).*/\1********/'"
Enter
Wait+Screen@30m /Cluster ready/
Sleep 2s

Type "kip app deploy --name hello --image nginx:latest --port 80"
Enter
Wait+Screen@5m /Live at/
Sleep 4s
