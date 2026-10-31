# The demo in the README and on the site, recorded with VHS
# (https://github.com/charmbracelet/vhs) against the fictional fleet of main.go.
# From the repository root:
#
#   vhs docs/demo/demo.tape
#
# Nothing reaches AWS: hop talks to the local server and its fake plugin.

Output docs/demo.gif
Output site/demo.mp4

Require go

Set Shell "bash"
Set FontFamily "SF Mono"
Set FontSize 14
Set LineHeight 1.2
Set Width 1240
Set Height 680
Set Padding 20
Set Framerate 30
Set TypingSpeed 65ms
Set CursorBlink false
Set Theme { "name": "hop", "background": "#0e1524", "foreground": "#d8e4ec", "cursor": "#5ae6be", "selection": "#24324a", "black": "#0e1524", "red": "#ff7a7a", "green": "#5ae6be", "yellow": "#f5c46b", "blue": "#7aa2f7", "magenta": "#c3a6ff", "cyan": "#5ae6be", "white": "#d8e4ec", "brightBlack": "#7f8fa3", "brightRed": "#ff9b9b", "brightGreen": "#8af0d2", "brightYellow": "#f8d48f", "brightBlue": "#9db8fa", "brightMagenta": "#d6c2ff", "brightCyan": "#8af0d2", "brightWhite": "#ffffff" }

# Build hop and the fleet, point hop to it, then start from a clean screen.
Hide
Type `export DEMO=$(mktemp -d) PS1='\[\e[38;2;90;230;190m\]$\[\e[0m\] '` Enter
Type "go build -o $DEMO/bin/hop ./cmd/hop && go build -o $DEMO/bin/session-manager-plugin ./docs/demo" Enter
Type "($DEMO/bin/session-manager-plugin serve $DEMO &); until [ -f $DEMO/env.sh ]; do sleep 0.1; done; source $DEMO/env.sh; cd ~; clear" Enter
Sleep 12s
Show

# Every instance of every profile, each in its own region, in one call.
Sleep 600ms
Type "hop ls" Sleep 500ms Enter
Sleep 4s

Hide
Type "clear" Enter
Show

# Words of a name: two instances match, so hop asks instead of guessing.
Sleep 400ms
Type "hop shop app" Sleep 500ms Enter
Sleep 2.2s
Type "prod" Sleep 900ms Enter
Sleep 2.2s
Type "hostname" Sleep 300ms Enter
Sleep 1.2s
Type "exit" Sleep 300ms Enter
Sleep 1.5s

Hide
Type "clear" Enter
Show

# An IP from an alert, in another company's account: straight in.
Sleep 400ms
Type "hop 10.90.2.73" Sleep 500ms Enter
Sleep 2.2s
Type "uptime" Sleep 300ms Enter
Sleep 1.4s
Type "exit" Sleep 300ms Enter
Sleep 2.5s
