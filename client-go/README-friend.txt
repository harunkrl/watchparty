watchparty — how to watch together (3 steps)

1. Unzip this folder anywhere.
2. Double-click watchparty.exe.
   - Windows may show "Windows protected your PC": click "More info",
     then "Run anyway" (one time only).
   - macOS: right-click watchparty, choose "Open", confirm once.
3. Type the server address your host gives you (looks like
   http://100.101.102.103:8090), press Enter, type your name, press Enter.

Optional, to check the download was not corrupted: in PowerShell run
  Get-FileHash watchparty-windows-bundle.zip -Algorithm SHA256
and compare with the matching line in SHA256SUMS on the release page.

The movie plays in a separate mpv window. The black window is the remote
control — just type your message there and press Enter to talk.
Commands start with "/": /list | /pick | /play <file> | /queue <file> |
/skip | /help   (everything else you type goes to the room as chat)
