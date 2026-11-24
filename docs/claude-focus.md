# claude-focus

A tiny Windows utility that brings the Claude desktop app to the front, or
launches it if it isn't open. Bind it to a key — for example the Copilot key —
and that key becomes "go to Claude".

## Why not just run `claude-desktop.exe`?

Pointing a hotkey at Claude's launcher (`%LOCALAPPDATA%\Microsoft\WindowsApps\claude-desktop.exe`)
works when Claude is closed, but when it's already running the launcher hands
off to the existing instance, and Windows' foreground lock stops that instance
from taking focus. Claude activates *behind* the window you were in.

`claude-focus` finds the Claude window itself and brings it forward. If
Claude isn't open (or is hidden to the tray), it starts the launcher and then
focuses the window once it appears.

Claude Code (the CLI) is also `claude.exe`, but it never owns a top-level
window, so `claude-focus` won't mistake a terminal session for the desktop app.

## Build

Build it as a GUI-subsystem binary so it never flashes a console window:

```powershell
go build -trimpath -ldflags "-H windowsgui" -o bin\ .\cmd\claude-focus
```

(Release archives for Windows already include `claude-focus.exe` built this
way. A plain `go build -o bin\ .\cmd\...` also produces it, but as a console
binary.)

## Binding it to the Copilot key with PowerToys

Windows' own *Settings → Personalization → Text input → Customize Copilot key*
may only offer Copilot, so use [PowerToys](https://learn.microsoft.com/windows/powertoys/)
Keyboard Manager instead:

1. Open **PowerToys → Keyboard Manager → Remap a shortcut** and add a remap.
2. For the shortcut, press the Copilot key. It sends **Win + Shift + F23**;
   let the editor capture it rather than typing it in.
3. Set the action to **Run program**:
   - **Program:** the **full path** to `claude-focus.exe`. Keyboard Manager
     resolves bare names against its own install folder, not `PATH`, so
     `claude-focus.exe` on its own fails with "program not found".
   - **Arguments:** none.
   - **If running:** leave as **Show window** (it exits immediately, so it is
     never found running) — or **Start another**; either works.
   - **Visibility:** **Normal** for the `-H windowsgui` build (it has no
     window to show). **Hidden** only if you built a console binary — on the
     GUI build it just makes PowerToys retry looking for a window to hide.

PowerToys must be running for the remap to work.

### Keyboards where Fn switches the key to Menu

On some keyboards (Surface keyboards, for example) the Copilot key sits where
the Menu key used to be, and **Fn** switches it between Copilot
(Win + Shift + F23) and Menu. With Fn lock on, pressing the key alone sends
**Menu**, and the remap above never sees it.

To have both modes open Claude, add a key remap that turns Menu into Copilot:

1. **Keyboard Manager → Remap a key**: **Menu** → **Win + Shift + F23**, with
   the condition set to when the key is pressed **alone**.
2. Keep the **Win + Shift + F23 → Run program** shortcut from above.

A single-key remap can feed a shortcut remap, so this works. The reverse does
not: Keyboard Manager ignores keys it sends itself, so one *shortcut* remap
never triggers another. Remapping Win + Shift + F23 to some other shortcut
that runs the program just swallows the Copilot key and does nothing.

## Troubleshooting

Keyboard Manager logs every remap it fires to
`%LOCALAPPDATA%\Microsoft\PowerToys\Keyboard Manager\Engine\Logs\<version>\log_<date>.log`.
Look for `ChordKeyboardHandler` lines:

- No new lines when you press the key → the shortcut didn't match; recapture it.
  If the key is in Menu mode, see the Fn section above.
- `keyEventList.size` lines but no `trying to run` → the key was remapped to
  another shortcut, which doesn't chain; point it at **Run program** instead.
- `Error starting … The program was not found` → use a full path.
- `already running … TryShowExistingProgram` → *If running* is set to
  **Show window** and a process with the same file name was already running,
  so PowerToys focused that instead of launching. Only an issue when the
  program is a shared host such as `explorer.exe` or `powershell.exe`.
