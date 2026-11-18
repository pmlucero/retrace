# Records examples/demo/demo.gif (AC-23): record, exact replay, live-tools
# replay with a divergence (a changed shipping API), diff. Run from the repository root:
#
#   vhs examples/demo/demo.tape                  # real API, needs ANTHROPIC_API_KEY
#   DEMO_FAKE=1 vhs examples/demo/demo.tape      # free rehearsal, scripted fake
#
# See examples/demo/SMOKE.md. VHS is a manual tool, not a CI dependency.

Output examples/demo/demo.gif

Set Shell "bash"
Set FontSize 16
Set Width 1400
Set Height 800
Set Padding 20
Set TypingSpeed 40ms
Set Theme "Catppuccin Mocha"

# Hidden: servers, cassette directory, PATH. Not part of the GIF.
Hide
Type "export PS1='$ ' && source examples/demo/vhs-setup.sh && demo_up && clear"
Enter
Wait+Screen@60s /(?m)\$ *$/
Show

Type "# 1. Record a real agent session, with two MCP servers"
Enter
Sleep 1s
Type "retrace record --session checkout --mcp orders=http://127.0.0.1:9001/mcp --mcp shipping=http://127.0.0.1:9002/mcp -- uv run --locked python agent.py"
Sleep 500ms
Enter
Wait+Screen@120s /(?m)\$ *$/
Sleep 4s

Type "clear"
Enter
Type "# 2. Replay it exactly: no network, same trajectory"
Enter
Sleep 1s
Type "retrace run checkout --as checkout.exact --recorded-command"
Sleep 500ms
Enter
Wait+Screen@60s /(?m)\$ *$/
Type "retrace diff checkout checkout.exact"
Sleep 500ms
Enter
Wait+Screen@30s /(?m)\$ *$/
Sleep 4s

Type "clear"
Enter
Type "# 3. Your shipping API changed: replay with live tools, see where the agent diverges"
Enter
Sleep 1s
Type "retrace run checkout --as checkout.live --tools live --mcp shipping=http://127.0.0.1:9003/mcp --recorded-command"
Sleep 500ms
Enter
Wait+Screen@120s /(?m)\$ *$/
Type "retrace diff checkout checkout.live"
Sleep 500ms
Enter
Wait+Screen@30s /(?m)\$ *$/
Sleep 6s

Hide
Type "demo_down"
Enter
Sleep 1s
Show
