# Connecting Azor to Claude Code

Run these from this directory.

Skill (Claude queries history when it is relevant):

    mkdir -p ~/.claude/skills
    ln -s "$PWD/azor" ~/.claude/skills/azor

Hook (one line of current state when a session starts). Merge into `~/.claude/settings.json`:

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
