# Records examples/demo/demo.gif (AC-23): record, exact replay, swap-model
# replay with a divergence, diff. Run from the repository root:
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
Wait+Screen@60s /\$ *$/
Show

Type "# 1. Record a real agent session, with two MCP servers"
Enter
Sleep 1s
Type "retrace record --session checkout --mcp orders=http://127.0.0.1:9001/mcp --mcp shipping=http://127.0.0.1:9002/mcp -- uv run --locked python agent.py"
Sleep 500ms
Enter
Wait+Screen@120s /\$ *$/
Sleep 4s

Type "clear"
Enter
Type "# 2. Replay it exactly: no network, same trajectory"
Enter
Sleep 1s
Type "retrace run checkout --as checkout.exact --recorded-command"
Sleep 500ms
Enter
Wait+Screen@60s /\$ *$/
Type "retrace diff checkout checkout.exact"
Sleep 500ms
Enter
Wait+Screen@30s /\$ *$/
Sleep 4s

Type "clear"
Enter
Type "# 3. Swap the model, and see where it diverges"
Enter
Sleep 1s
Type "retrace run checkout --as checkout.swap --model claude-sonnet-5-5 --recorded-command"
Sleep 500ms
Enter
Wait+Screen@120s /\$ *$/
Type "retrace diff checkout checkout.swap"
Sleep 500ms
Enter
Wait+Screen@30s /\$ *$/
Sleep 6s

Hide
Type "demo_down"
Enter
Sleep 1s
Show
