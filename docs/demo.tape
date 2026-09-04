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
#   0. The recording runs on fixed timings, so read the GIF before you trust
#      it: a step that took longer than its sleep leaves the next command typed
#      into a busy terminal, and the tape cannot notice.
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
#
# The tape does not produce the finished file on its own. What ships as
# docs/public/demo.gif is this recording with two things appended: a longer hold
# on the last terminal frame, and a screenshot of the app the install just
# deployed, held long enough to read before the loop restarts. Without them the
# result flashes past and starts again. The screenshot has to be taken while the
# cluster is still up, so take it before the teardown:
#
#   chrome --headless --screenshot=app.png --window-size=1400,820 https://hello--<name>.kipper.run
#   ffmpeg -i docs/public/demo.gif -loop 1 -t 3 -i last-terminal-frame.png \
#          -loop 1 -t 4.5 -i app-framed.png \
#          -filter_complex "[0:v]fps=25,setsar=1[a];[1:v]fps=25,setsar=1[b];\
#          [2:v]fps=25,setsar=1[c];[a][b][c]concat=n=3:v=1:a=0[v];[v]split[x][y];\
#          [x]palettegen=max_colors=128:stats_mode=diff[p];[y][p]paletteuse=dither=bayer" \
#          -loop 0 out.gif
#
# The screenshot is framed on the tape's own background colour with the same
# inset and corner radius, so it reads as the next panel rather than a pasted
# image. Take it with the browser in light mode: the nginx welcome page declares
# `color-scheme: light dark`, so a dark desktop renders it as more dark terminal.

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
# vhs cannot detect completion here. Both Wait+Screen and Wait+Line match
# against the visible screen, and this install prints far more than one
# screenful, so once the output scrolls neither ever matches and the recording
# is thrown away after the timeout. A fixed sleep is what works. The figure is
# a measured install plus margin: 4m40s on a 4 vCPU / 8 GB Hetzner box, most of
# it pulling images. Raise it on a slower server rather than hoping.
Sleep 390s

Type "kip app deploy --name hello --image nginx:latest --port 80"
Enter
# Same reason, and the screen is scrolled by now regardless.
Sleep 90s
Sleep 4s
