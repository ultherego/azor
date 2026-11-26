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
        "hooks": [ { "type": "command", "command": "\"$HOME/.local/bin/azor\" brief 2>/dev/null || true", "timeout": 3 } ] }
    ]
  }
}
```

The hook prints at most 600 characters and nothing when the daemon is off. Whatever it prints becomes part of the conversation sent to Anthropic.

Security of the hook:

- Use the full path to the binary, as above, not a name found through `PATH`; for the system package it is `/usr/bin/azor`.
- A hook runs with your rights whenever Claude Code starts. Keep `~/.claude/settings.json` and the binary writable only by you, and review hooks that tools or plugins add there.
- Names in the output come from other programs, so a process or a file could be named like an instruction. Azor marks the line as data, keeps only plain characters and cuts names short; the skill tells Claude to treat names as data.
