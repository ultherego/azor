# Claude Code integration

Skill (Claude queries history when it is relevant):

    ln -s "$PWD/azor" ~/.claude/skills/azor

Optional hook (one line of current state when a session starts). Merge into `~/.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [
      { "matcher": "startup|resume",
        "hooks": [ { "type": "command", "command": "PATH=\"$HOME/.local/bin:$PATH\" azor brief 2>/dev/null || true", "timeout": 3 } ] }
    ]
  }
}
```

The hook prints at most 600 characters and nothing when the daemon is off. Whatever it prints becomes part of the conversation sent to Anthropic.
