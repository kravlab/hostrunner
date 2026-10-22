# Host surfaces that can put a yes/no question to a human

Research for issue #72 (map: #71, one-off host approval for commands the
rules do not allow). Question: what can put a yes/no question to a human
on a Linux host and carry the answer back to the daemon; for each, how
widely available it is, what it needs from the process that raises it
(`DISPLAY`, `WAYLAND_DISPLAY`, `DBUS_SESSION_BUS_ADDRESS`, a TTY), whether
the detached daemon that `hostrunner up` starts from `initializeCommand`
has that, and what happens on a headless or SSH host.

Method: primary sources only (specifications, man pages, upstream source
and first-party docs), read on 2026-10-22. Source links to a moving branch
(`main`/`master`) are as of that date. **No empirical check was run on a
host**: this research ran in a devcontainer that reaches the host only
through `git` and `gh` rules, so nothing below was observed on a real
desktop. Statements marked *inference* follow from the cited sources but
were not observed.

## TL;DR

- The daemon has **no TTY** (new session, stdin is `/dev/null`) and
  **inherits the whole environment** of `hostrunner up`, which is the
  environment of whatever runs `initializeCommand`. Whether that includes
  `DISPLAY`, `WAYLAND_DISPLAY` or `DBUS_SESSION_BUS_ADDRESS` depends on the
  launcher, not on hostrunner.
- `XDG_RUNTIME_DIR` is the one variable the setup already requires, and it
  is enough to find a systemd user bus (`$XDG_RUNTIME_DIR/bus`) and the
  default Wayland socket (`wayland-0`), even when the other variables are
  missing.
- **A terminal session the human starts** (a `hostrunner` subcommand)
  needs nothing from the daemon's environment, works over SSH and on
  headless hosts, and is the only surface that works everywhere. Its cost:
  the human has to have it open, and in a devcontainer window VS Code's
  ordinary terminals run *inside* the container.
- **Desktop notifications with actions** need only a session-bus
  connection (no `DISPLAY`), and GNOME Shell and KDE Plasma both advertise
  `actions`. They are a best-effort surface: the spec lets a server ignore
  actions, the click is not guaranteed, GNOME shows no banner for a
  non-critical notification while banners are off, and on a headless host
  nothing owns the name.
- **Dialog tools** (`zenity`, `kdialog`, `yad`) need a display connection
  (Wayland or X11, plus an X cookie for X11), exit with a code that carries
  the answer, and fail immediately without a display. Neither is
  guaranteed to be installed.
- On an **SSH host** (VS Code Remote-SSH + Dev Containers),
  `initializeCommand` runs on the remote host, so any desktop surface there
  shows on the remote host's screen (if it has one), not in front of the
  human.
- **polkit**, **systemd password agents** and the **xdg-desktop-portal
  Notification** portal are not usable as the surface, but the systemd
  password-agent protocol is close prior art for a "pending request +
  agents that answer it" design.

## 1. What the daemon has to work with

### Environment, stdio, session

`startDaemon` in `internal/launch/launch.go` builds the daemon with
`exec.Command(cfg.Daemon, "serve", …)`, sets `Stdout`/`Stderr` to the log
file, leaves `Env` and `Stdin` nil, and sets
`SysProcAttr{Setsid: true}`:

- "If Env is nil, the new process uses the current process's environment."
  ([os/exec Cmd][go-exec]) So the daemon's environment is exactly the one
  `hostrunner up` got. `internal/daemon/daemon.go` (`commandEnv`) passes it
  on to every command it runs.
- "If Stdin is nil, the process reads from the null device (os.DevNull)."
  ([os/exec Cmd][go-exec])
- After `setsid`, "Initially, the new session has no controlling
  terminal." ([setsid(2)][setsid]) The daemon cannot open `/dev/tty`, so it
  cannot prompt on a terminal itself.

### Who runs `initializeCommand`, with which environment

- **devcontainer CLI:** `runInitializeCommand` spawns the command with
  `env: dockerEnv` ([`src/spec-node/utils.ts`][cli-utils]); `dockerEnv` is
  `cliHost.env` ([`src/spec-node/devContainers.ts`][cli-devcontainers]),
  and for the local CLI host that is `process.env`
  ([`src/spec-common/cliHost.ts`][cli-clihost]). So the daemon gets the
  environment of the shell or tool that ran `devcontainer up`, nothing
  added.
- **VS Code Dev Containers:** the extension is closed source, so the exact
  environment it passes is not verifiable from source. The spec says
  `initializeCommand` runs "on the host machine" and "wherever the source
  code is located on the host. For cloud services, this is in the cloud."
  ([json reference][json-ref]) When VS Code is launched from the desktop
  it resolves the user's shell environment on top of its own
  ([VS Code FAQ][vsc-faq]). *Inference:* a VS Code started from a
  graphical session carries that session's `DISPLAY`/`WAYLAND_DISPLAY`/
  `DBUS_SESSION_BUS_ADDRESS`, and so would the daemon; this must be
  checked on a real desktop before the spec relies on it.
- **VS Code Remote-SSH + Dev Containers:** "the extension runs commands
  and other extensions directly on the remote machine" through the VS Code
  Server it installs there ([VS Code Remote-SSH][vsc-ssh]), and Docker is
  installed on the SSH host, not locally ([remote Docker host][vsc-remote-host]).
  So `initializeCommand`, `hostrunner up` and the daemon run on the
  **remote** host, with the environment of an SSH login. `ssh` sets
  `DISPLAY` on the remote side when it forwards X11 ([ssh(1)][ssh]); whether Remote-SSH
  requests it is not covered by the docs read here.

### What `XDG_RUNTIME_DIR` alone gives

The README requires `XDG_RUNTIME_DIR` to be set for the process that
starts the devcontainer, since the runtime directory and the mount are
`${localEnv:XDG_RUNTIME_DIR}/hostrunner/${devcontainerId}` (README, "Use
in a devcontainer"). So a working daemon always has it. From it alone:

- **Session bus:** `pam_systemd` exports
  `DBUS_SESSION_BUS_ADDRESS` for `$XDG_RUNTIME_DIR/bus` when that socket
  exists, "This setting matches what dbus.socket does for the user session"
  ([`pam_systemd.c`][pam-systemd-c]). The Go library `godbus/dbus` uses
  `DBUS_SESSION_BUS_ADDRESS` if set and otherwise tries
  `$XDG_RUNTIME_DIR/bus` before falling back to `dbus-launch`
  (`getSessionBusAddress`, `tryDiscoverDbusSessionBusAddress` in
  [`conn.go`][godbus-conn], [`conn_other.go`][godbus-conn-other]). The
  D-Bus spec only defines the variable and an X11 root-window property,
  leaving the rest to "platform-specific methods" ([D-Bus spec,
  well-known instances][dbus-spec]).
- **Wayland:** `wl_display_connect(NULL)` uses `WAYLAND_DISPLAY` "if it is
  set, otherwise display "wayland-0" will be used", relative to
  `XDG_RUNTIME_DIR` ([`wayland-client.c`][wayland-client]).
- **Per-user, not per-session:** "If a user logs in twice at the same time,
  both sessions will see the same `$XDG_RUNTIME_DIR` and the same
  contents", and the directory is "removed on the user's final logout"
  ([pam_systemd(8)][pam-systemd]). *Inference:* an SSH login and a
  graphical login of the same user on one host share one user bus, so a
  daemon started from SSH can reach that user's desktop notification
  server if a desktop session is running on that host.

### Lifetime

The daemon outlives `hostrunner up` and keeps running while its
container runs (`internal/launch/launch.go`, package comment). *Inference:*
display variables captured at start can go stale if the graphical session
ends while the container keeps running; a surface chosen per request
should be looked up per request, not once at start.

## 2. Surfaces

### 2.1 A terminal session the human starts (`hostrunner` subcommand)

A subcommand (for example `hostrunner approve`, name not decided) that the
human runs in a host terminal, connects to the daemon on a channel the
container cannot reach, shows pending requests and sends the answers.

- **Availability:** ships with hostrunner; no other package needed.
- **Needs:** a terminal on the host. The TTY belongs to the human's
  process, not to the daemon, so the daemon's missing TTY and environment
  do not matter. Over SSH, `SSH_TTY` "is set to the name of the tty …
  associated with the current shell" ([ssh(1)][ssh]); headless hosts work
  the same way.
- **Channel outside the container:** the container mounts only the
  per-container directory `…/hostrunner/${devcontainerId}` read-only at
  `/run/hostrunner` (README, "Use in a devcontainer"), and map #71 notes
  that read-only does not stop the container from connecting to a socket
  there. A control socket elsewhere (for example in the parent
  `$XDG_RUNTIME_DIR/hostrunner/`, which is not mounted) is outside the
  mount.
- **VS Code caveat:** in a devcontainer window "any terminal window you
  open in VS Code (Terminal > New Terminal) will automatically run in the
  container rather than locally" ([Dev Containers docs][vsc-containers]).
  A host terminal is a separate terminal app, or **Terminal: Create New
  Integrated Terminal (Local)**, which launches "the default local terminal
  profile … in remote windows" ([VS Code terminal advanced][vsc-term]).
  With Remote-SSH, "local" there means the human's machine, not the SSH
  host (*inference* from the same section); the human would need an SSH
  terminal to the host.
- **Weakness:** nothing tells the human that a request is waiting unless
  the subcommand is already running, or another channel (the `hostrun`
  client's own stderr, a notification) tells them to start it.
- **Prior art:**
  - systemd password agents: a requester drops an `ask.xxxx` file into a
    directory; "multiple different agents may watch via inotify() … and the
    agent which answers first wins"; agents must hide the prompt when the
    file is removed or `NotAfter=` passes; one shipped agent is "A simple
    tty agent which can be run manually to respond to all queued
    passwords" ([Password Agents][pw-agents]).
  - polkit: applications launched from an SSH login may have no
    authentication agent, and can use `PolkitAgentTextListener` or
    `pkttyagent` for a textual one instead ([polkit(8)][polkit];
    [pkttyagent(1)][pkttyagent]).

### 2.2 Desktop notifications with actions (freedesktop spec, `notify-send`)

**Protocol** ([Desktop Notifications Specification 1.3][notif-spec]):

- One server owns `org.freedesktop.Notifications` on the **session bus**;
  it "can optionally be activated automatically by the bus process, however
  this is not required and notification server clients must not assume
  that it is available" ([Basic Design][notif-basic]).
- `Notify(app_name, replaces_id, app_icon, summary, body, as actions,
  a{sv} hints, INT32 expire_timeout)` returns an id; actions are
  identifier/label pairs; `expire_timeout` 0 means never expire
  ([Protocol][notif-protocol]).
- The answer comes back as the signal `ActionInvoked(id, action_key)`; a
  close comes as `NotificationClosed(id, reason)` with reason 1 expired,
  2 dismissed by the user, 3 closed by `CloseNotification`
  ([Protocol][notif-protocol]).
- Caveats in the spec itself: without the `actions` capability "the
  server is free to ignore them"; for `ActionInvoked`, "Clients should not
  assume the server will generate this signal" ([Protocol][notif-protocol]);
  the `default` action is "usually invoked by clicking the notification"
  ([Basic Design][notif-basic]); "Information that must be presented in a
  more complex form should use an application-specific dialog"
  ([Markup][notif-markup]).
- Body text may contain markup (`<b>`, `<i>`, `<u>`, `<a>`, `<img>`)
  ([Markup][notif-markup]), so a command line shown in the body has to be
  escaped.

**Servers:** GNOME Shell's `GetCapabilities` returns `actions`,
`persistence` and others ([`notificationDaemon.js`][gs-notifd]); KDE
Plasma's returns `actions`, `persistence`, `inline-reply` and others
([`server_p.cpp`][plasma-server]). GNOME Shell skips the banner for a
notification when its source's `show-banners` policy is off (the global
`show-banners` setting or the per-app one) unless its urgency is critical (`_onNotificationRequestBanner` in
[`messageTray.js`][gs-messagetray]).

**Needs from the process:** a session-bus connection only; no `DISPLAY`
or `WAYLAND_DISPLAY`. Found through `DBUS_SESSION_BUS_ADDRESS` or, on a
systemd user bus, `$XDG_RUNTIME_DIR/bus` (section 1).

**The daemon:** has a bus address if the launcher had
`DBUS_SESSION_BUS_ADDRESS`, or if the host uses a systemd user bus at
`$XDG_RUNTIME_DIR/bus` (section 1). hostrunner has no D-Bus dependency
today (`go.mod` lists `yaml/v3`, `x/mod`, `x/sys`); speaking the protocol
directly would add one (e.g. `godbus/dbus`), shelling out would add a
runtime dependency on `notify-send`.

**`notify-send`** (package `libnotify-bin` on Debian/Ubuntu):

- `-A, --action=[NAME=]Text…` "Implies --wait"; "The NAME of the action is
  output to stdout" ([notify-send(1)][notify-send-man]).
- If the server lacks `actions`, it prints "Actions are not supported by
  this notifications server. Displaying non-interactively." and exits 1;
  if the notification is closed without an action it exits 0 with nothing
  on stdout (`handle_closed`, `handle_action`, `main` in
  [`notify-send.c`][notify-send-c]).
- Actions and `--wait` arrived in libnotify **0.7.10** ("Add support for
  notification actions and responses") ([libnotify NEWS][libnotify-news]).
  Ubuntu 22.04 ships 0.7.9 and 24.04 ships 0.8.3 ([Launchpad
  libnotify][lp-libnotify]); Debian 12 ships 0.8.1 and Debian 13 0.8.6
  ([bookworm][deb-libnotify-12], [trixie][deb-libnotify-13]).

**Headless / SSH:** on a host with no graphical session nothing owns
`org.freedesktop.Notifications`; unless a service file activates one, the
call fails ("in case of failure, an error will be returned", [D-Bus spec,
message types][dbus-spec]). With Remote-SSH the notification goes to the
remote host's desktop, if any, not the human's screen (section 1).

### 2.3 `zenity`

- **What it does:** `zenity --question` "will return either 0, 1 or 5,
  depending on whether the user pressed OK, Cancel or timeout has been
  reached"; `--ok-label`, `--cancel-label`, `--extra-button=TEXT` and
  `--timeout` exist ([zenity(1)][zenity-man]). An extra button prints its
  label to stdout and exits 1; Esc also exits 1 (defaults
  `ZENITY_EXTRA_DEFAULT 1`, `ZENITY_ESC_DEFAULT 1`, `ZENITY_ERROR_DEFAULT
  -1` in [`util.c`][zenity-util-c]; `ZENITY_UTIL_RESPONSE_HANDLE_EXTRA_BUTTONS`
  in [`util.h`][zenity-util-h]). So more than two answers (refuse, once,
  daemon lifetime, add a rule) need extra buttons and stdout parsing.
- **Toolkit:** current zenity depends on `gtk4 >= 4.10` and
  `libadwaita-1 >= 1.2` ([`meson.build`][zenity-meson]).
- **Needs:** a GDK display. GDK tries its compiled backends in order,
  Wayland before X11 ([`gdkdisplaymanager.c`][gdk-dm]); Wayland connects
  with `wl_display_connect` (`WAYLAND_DISPLAY`, else `wayland-0` under
  `XDG_RUNTIME_DIR`) ([`gdkdisplay-wayland.c`][gdk-wl]; [`wayland-client.c`][wayland-client]);
  X11 uses `DISPLAY` ([XOpenDisplay(3)][xopendisplay]) and an
  authorization cookie from `~/.Xauthority` or `XAUTHORITY`
  ([Xsecurity(7)][xsecurity]). `gtk_init` "will terminate your program if
  it was unable to initialize the windowing system" ([gtk_init][gtk-init]).
- **Availability:** Debian's `gnome-core` metapackage depends on
  `zenity (>= 4)` ([gnome-core][deb-gnome-core]); Debian 13 ships 4.1.90
  ([zenity][deb-zenity]). Not guaranteed elsewhere.
- **Headless / SSH:** no display, so it exits at startup. With `ssh -X`,
  `DISPLAY` points to a proxy and the window opens on the SSH client's X
  server ([ssh(1)][ssh]).

### 2.4 `kdialog`

- **What it does:** `--yesno`, `--warningyesno`, `--yesnocancel`, with
  `--yes-label`, `--no-label`, `--cancel-label`; "return 0 for ok, yes and
  continue, 1 for no and 2 for cancel" ([`kdialog.cpp`][kdialog-cpp];
  [KDE kdialog docs][kdialog-docs]).
- **Toolkit:** Qt (`QApplication`) ([`kdialog.cpp`][kdialog-cpp]).
- **Needs:** a Qt platform plugin. Qt prefers `xcb` when `DISPLAY` is set
  and prepends `wayland` when `WAYLAND_DISPLAY` is set or
  `XDG_SESSION_TYPE` is `wayland`; if no plugin initializes it aborts with
  "This application failed to start because no Qt platform plugin could be
  initialized" ([`qguiapplication.cpp`][qt-guiapp]). With neither
  variable set, Qt only tries the build's default platform, and
  `QT_QPA_PLATFORM` overrides the whole choice (same source).
- **Availability:** Debian 13 ships `kdialog` 25.04 ([kdialog][deb-kdialog]);
  Debian's `kde-plasma-desktop` metapackage does not list it
  ([kde-plasma-desktop][deb-kde-plasma]).
- **Headless / SSH:** as zenity.

### 2.5 Others

- **`yad`**: a GTK dialog tool forked from zenity; Debian 13 ships 0.40.0
  ([yad][deb-yad]). Same display needs as zenity, less common.
- **TTY dialog tools** (`dialog`, `whiptail`): need a terminal; the daemon
  has none (section 1). Usable only inside the human-started subcommand.
- **polkit**: authentication agents exist to make the session user
  "prove that the user of the session really is the user", and actions
  must be declared in XML "that the mechanism installs into the
  /usr/share/polkit-1/actions directory" ([polkit(8)][polkit]). A root-
  installed file and a password prompt, not a free choice among four
  answers: not a fit (*inference*).
- **systemd password agents**: for "system-level passwords", answers go to
  a socket where "Access to the socket is restricted to privileged users"
  ([Password Agents][pw-agents]). Not usable directly; the protocol is
  prior art for 2.1.
- **xdg-desktop-portal Notification**: "lets sandboxed applications send
  and withdraw notifications. It is not possible for the application to
  learn if the notification was actually presented to the user"
  ([portal Notification][portal-notif]). Meant for sandboxed apps; adds
  nothing over 2.2 for a host daemon.
- **`wall`**: systemd ships a wall agent that only announces that a
  password is wanted ([Password Agents][pw-agents]); a broadcast has no
  answer path.

## 3. Comparison

| Surface | Installed | Needs from the raiser | Daemon has it? | Answer path | Headless host | SSH host (Remote-SSH) |
|---|---|---|---|---|---|---|
| `hostrunner` subcommand in a host terminal | always (ships with hostrunner) | nothing; the human's terminal has the TTY | n/a | control socket outside the mount | works | works (SSH terminal to the host) |
| Notification with actions (D-Bus, or `notify-send`) | server in GNOME/KDE sessions; `notify-send` ≥ 0.7.10 needed for actions | session bus (`DBUS_SESSION_BUS_ADDRESS` or `$XDG_RUNTIME_DIR/bus`) | likely, via the launcher's env or the user bus (*unverified*) | `ActionInvoked` signal / stdout; not guaranteed | no server: call fails | shows on the remote host's desktop, if any |
| `zenity` | GNOME (Debian `gnome-core`) | Wayland socket or `DISPLAY` + X cookie | only if the launcher's env had them, or `wayland-0` exists (*unverified*) | exit code 0/1/5, extra button label on stdout | exits at startup | no display, or `ssh -X` client's display |
| `kdialog` | not guaranteed even on KDE | `DISPLAY` or `WAYLAND_DISPLAY`/`XDG_SESSION_TYPE=wayland` | only if the launcher's env had them | exit code 0/1/2 | aborts at startup | as zenity |
| polkit / password agents / portal / wall | – | – | – | not a fit | – | – |

## 4. Facts the decision in #73 waits on

1. The daemon never has a TTY; any terminal prompt has to happen in a
   process the human starts.
2. The daemon's environment is the launcher's: complete with the
   devcontainer CLI (`process.env`), unverified for VS Code (closed
   source). Whether `DISPLAY`/`WAYLAND_DISPLAY`/`DBUS_SESSION_BUS_ADDRESS`
   reach the daemon from VS Code needs one empirical check on a desktop.
3. `XDG_RUNTIME_DIR` is guaranteed and gives a fallback to the user bus
   and `wayland-0`, but not to X11 (`DISPLAY` and the cookie have no
   fallback).
4. Only the terminal subcommand works on headless and SSH hosts. With
   Remote-SSH every desktop surface lands on the remote host.
5. Notifications can carry a choice but are best-effort by spec (actions
   optional, signal not guaranteed, no banner on GNOME for a non-critical
   notification while banners are off); they suit a nudge ("a request is waiting") better than the only
   channel for the answer.
6. Dialog tools can carry four answers only through extra buttons
   (zenity) or a fixed three (kdialog), are optional packages, and fail
   outright without a display.
7. The answer channel must live outside the per-container mount (map
   #71); the parent `$XDG_RUNTIME_DIR/hostrunner/` is not mounted.
8. The systemd password-agent protocol (pending request, many agents,
   first answer wins, withdraw on expiry) is a tested model for combining
   a terminal agent with optional desktop agents.

## 5. Not verified here

- The environment the VS Code Dev Containers extension passes to
  `initializeCommand`, locally and over Remote-SSH.
- Whether VS Code Remote-SSH forwards X11.
- Notification and dialog behaviour on a real GNOME/KDE desktop, under
  Do Not Disturb, and with a lock screen.
- Desktop servers other than GNOME Shell and KDE Plasma.

<!-- Link targets (reference-style), all read on 2026-10-22. -->

[go-exec]: https://pkg.go.dev/os/exec#Cmd
[setsid]: https://man7.org/linux/man-pages/man2/setsid.2.html
[cli-utils]: https://github.com/devcontainers/cli/blob/main/src/spec-node/utils.ts
[cli-devcontainers]: https://github.com/devcontainers/cli/blob/main/src/spec-node/devContainers.ts
[cli-clihost]: https://github.com/devcontainers/cli/blob/main/src/spec-common/cliHost.ts
[json-ref]: https://containers.dev/implementors/json_reference/
[vsc-faq]: https://code.visualstudio.com/docs/supporting/faq#_resolving-shell-environment-fails
[vsc-ssh]: https://code.visualstudio.com/docs/remote/ssh
[vsc-remote-host]: https://code.visualstudio.com/remote/advancedcontainers/develop-remote-host
[vsc-containers]: https://code.visualstudio.com/docs/devcontainers/containers
[vsc-term]: https://code.visualstudio.com/docs/terminal/advanced
[ssh]: https://man.openbsd.org/ssh.1
[pam-systemd]: https://www.freedesktop.org/software/systemd/man/latest/pam_systemd.html
[pam-systemd-c]: https://github.com/systemd/systemd/blob/main/src/login/pam_systemd.c
[godbus-conn]: https://github.com/godbus/dbus/blob/master/conn.go
[godbus-conn-other]: https://github.com/godbus/dbus/blob/master/conn_other.go
[dbus-spec]: https://dbus.freedesktop.org/doc/dbus-specification.html
[wayland-client]: https://gitlab.freedesktop.org/wayland/wayland/-/blob/main/src/wayland-client.c
[pw-agents]: https://systemd.io/PASSWORD_AGENTS/
[polkit]: https://www.freedesktop.org/software/polkit/docs/latest/polkit.8.html
[pkttyagent]: https://www.freedesktop.org/software/polkit/docs/latest/pkttyagent.1.html
[notif-spec]: https://specifications.freedesktop.org/notification-spec/latest/
[notif-basic]: https://specifications.freedesktop.org/notification-spec/latest/basic-design.html
[notif-protocol]: https://specifications.freedesktop.org/notification-spec/latest/protocol.html
[notif-markup]: https://specifications.freedesktop.org/notification-spec/latest/markup.html
[gs-notifd]: https://gitlab.gnome.org/GNOME/gnome-shell/-/blob/main/js/ui/notificationDaemon.js
[gs-messagetray]: https://gitlab.gnome.org/GNOME/gnome-shell/-/blob/main/js/ui/messageTray.js
[plasma-server]: https://invent.kde.org/plasma/plasma-workspace/-/blob/master/libnotificationmanager/server_p.cpp
[notify-send-man]: https://manpages.debian.org/unstable/libnotify-bin/notify-send.1.en.html
[notify-send-c]: https://gitlab.gnome.org/GNOME/libnotify/-/blob/master/tools/notify-send.c
[libnotify-news]: https://gitlab.gnome.org/GNOME/libnotify/-/blob/master/NEWS
[lp-libnotify]: https://launchpad.net/ubuntu/+source/libnotify
[deb-libnotify-12]: https://packages.debian.org/bookworm/libnotify-bin
[deb-libnotify-13]: https://packages.debian.org/trixie/libnotify-bin
[zenity-man]: https://manpages.debian.org/unstable/zenity/zenity.1.en.html
[zenity-util-c]: https://gitlab.gnome.org/GNOME/zenity/-/blob/master/src/util.c
[zenity-util-h]: https://gitlab.gnome.org/GNOME/zenity/-/blob/master/src/util.h
[zenity-meson]: https://gitlab.gnome.org/GNOME/zenity/-/blob/master/meson.build
[gdk-dm]: https://gitlab.gnome.org/GNOME/gtk/-/blob/main/gdk/gdkdisplaymanager.c
[gdk-wl]: https://gitlab.gnome.org/GNOME/gtk/-/blob/main/gdk/wayland/gdkdisplay-wayland.c
[xopendisplay]: https://www.x.org/releases/current/doc/man/man3/XOpenDisplay.3.xhtml
[xsecurity]: https://www.x.org/releases/current/doc/man/man7/Xsecurity.7.xhtml
[gtk-init]: https://docs.gtk.org/gtk4/func.init.html
[deb-gnome-core]: https://packages.debian.org/trixie/gnome-core
[deb-zenity]: https://packages.debian.org/trixie/zenity
[kdialog-cpp]: https://invent.kde.org/utilities/kdialog/-/blob/master/src/kdialog.cpp
[kdialog-docs]: https://develop.kde.org/docs/administration/kdialog/
[qt-guiapp]: https://github.com/qt/qtbase/blob/dev/src/gui/kernel/qguiapplication.cpp
[deb-kdialog]: https://packages.debian.org/trixie/kdialog
[deb-kde-plasma]: https://packages.debian.org/trixie/kde-plasma-desktop
[deb-yad]: https://packages.debian.org/trixie/yad
[portal-notif]: https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.Notification.html
