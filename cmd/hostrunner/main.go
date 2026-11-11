// Command hostrunner is the host side of hostrun: it runs commands sent by
// the hostrun client from inside a devcontainer.
//
// Usage:
//
//	hostrunner up [--restart [--kill | --wait]] --dir <runtime-dir>
//	    --workspace <host-path> --container-workspace <path> [settings]
//	hostrunner serve --socket <path> --workspace <host-path> --container-workspace <path>
//	    [--watch] [settings]
//	hostrunner test [--settings <path>] [--workspace-settings <path>] [--rules-file <path>]
//	    [--] <command> [args...]
//	hostrunner allow [<workspace>]
//	hostrunner deny [<workspace>]
//	hostrunner approve [--dir <runtime-dir>] [settings]
//	hostrunner stop [--dir <runtime-dir> | --all | --choose] [--kill | --wait] [settings]
//	hostrunner restart [--dir <runtime-dir> | --all | --choose] [--kill | --wait] [settings]
//	hostrunner update [--check]
//	hostrunner instructions [--append [<path>]]
//	hostrunner version
//
// `up` is meant for devcontainer's initializeCommand: it installs the client
// into the runtime directory and starts a detached `serve --watch` there,
// which exits by itself once its armed container stops; with --restart it
// replaces a daemon already serving it. `stop` stops a daemon from the
// host, the one serving the current directory by default, asking what
// becomes of the commands it runs unless --kill or --wait says; `restart`
// then starts a new one for it (see internal/stop). `test` runs the Rules
// test: whether the rules file `up` would use with the current directory
// as the workspace (default .devcontainer/hostrunner-rules.yaml) allows
// a command; it exits 0 when it does, 126 when it does not, and 1 on any
// error. `allow` is what a human on the host runs to trust a workspace's
// rules: `up` starts a daemon only for rules and settings so allowed, and
// an `up` refused records what it asked for (the rules file, its digest
// and the daemon settings) in $XDG_DATA_HOME/hostrunner/allow; `allow`
// allows exactly that request once the rules file still has that digest
// and prints what it allowed; without a request it allows nothing and
// fails, telling to start the container first. `deny` removes the allow.
// Only the latest allow of a workspace is kept; the workspace is the
// current directory unless one is named (see internal/allow).
// `update` switches the versioned install
// of hostrunner and hostrun to the latest release (see internal/update).
// `version` prints the module version Go stamped into the binary (see
// buildVersion). `approve` runs an approval session in a host terminal: it
// serves the daemon of the runtime directory, or one chosen from the list
// of daemons, showing each approval request and sending the human's
// answer (see internal/approval). `instructions` prints the instructions
// for agents working with hostrun, or with --append adds them to a file,
// AGENTS.md by default (see internal/instructions).
//
// The settings of `up` and `serve` are [--settings <path>]
// [--workspace-settings <path>] [--startup-timeout <duration>]
// [--grace <duration>] [--reread | --no-reread]; a setting no flag gives
// comes from its HOSTRUNNER_* variable, the workspace settings file
// (<workspace>/.devcontainer/hostrunner.yaml unless --workspace-settings
// or HOSTRUNNER_WORKSPACE_SETTINGS names another), the host settings file
// ($XDG_CONFIG_HOME/hostrunner/settings.yaml unless --settings or
// HOSTRUNNER_SETTINGS names another; its section for the workspace, then
// its common part) or the default (see internal/settings). `up` resolves
// them and starts the daemon with every one as a flag. The settings of
// `stop` and `restart` are [--settings <path>] [--runtime-root <path>]
// [--stop-timeout <duration>], those of `approve` [--settings <path>]
// [--runtime-root <path>] [--approve-tail-lines <integer>]; a setting no
// flag gives comes from its HOSTRUNNER_* variable, the common part of the
// host settings file or the default. `restart` resolves the new daemon's
// settings as `up` does, for the old daemon's workspace, also taking
// [--workspace-settings <path>] and [--rules-file <path>] (not with
// --all).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kravlab/hostrunner/internal/allow"
	"github.com/kravlab/hostrunner/internal/approval"
	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/instructions"
	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/mountcheck"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/rulesfile"
	"github.com/kravlab/hostrunner/internal/settings"
	"github.com/kravlab/hostrunner/internal/stop"
	"github.com/kravlab/hostrunner/internal/transport"
	"github.com/kravlab/hostrunner/internal/update"
	"github.com/kravlab/hostrunner/internal/watch"
	"github.com/kravlab/hostrunner/internal/workspace"
)

const (
	usage             = "usage: hostrunner up|serve|test|allow|deny|approve|stop|restart|update|instructions|version [flags]; see `hostrunner <command> -h`"
	allowUsage        = "usage: hostrunner allow [<workspace>]"
	denyUsage         = "usage: hostrunner deny [<workspace>]"
	updateUsage       = "usage: hostrunner update [--check]"
	instructionsUsage = "usage: hostrunner instructions [--append [<path>]]"
	versionUsage      = "usage: hostrunner version"
)

// The usages of the commands with settings, whose flags package settings
// names.
var (
	upUsage      = "usage: hostrunner up [--restart [--kill | --wait]] " + settings.UpUsage() + " --dir <runtime-dir> --workspace <host-path> --container-workspace <path>"
	serveUsage   = "usage: hostrunner serve --socket <path> --workspace <host-path> --container-workspace <path> [--watch] " + settings.Usage()
	testUsage    = "usage: hostrunner test " + settings.RulesFileUsage() + " [--] <command> [args...]"
	stopUsage    = "usage: hostrunner stop [--dir <runtime-dir> | --all | --choose] [--kill | --wait] " + settings.StopUsage()
	restartUsage = "usage: hostrunner restart [--dir <runtime-dir> | --all | --choose] [--kill | --wait] " + settings.RestartUsage()
	approveUsage = "usage: hostrunner approve [--dir <runtime-dir>] " + settings.ApproveUsage()
)

// errNoRuntimeRoot is the error of stop, restart and approve given no
// runtime directory when no source gives a runtime root and
// XDG_RUNTIME_DIR, its default, is unset.
var errNoRuntimeRoot = errors.New("XDG_RUNTIME_DIR is not set: give the runtime root with --runtime-root, HOSTRUNNER_RUNTIME_ROOT or runtime_root in the host settings file, or the runtime directory with --dir")

// Timeouts that are not settings (see internal/settings).
const (
	queryTimeout  = 10 * time.Second // one docker/podman ps call
	updateTimeout = 5 * time.Minute  // `update`'s version lookup and downloads
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "hostrunner: %v\n", err)
		os.Exit(exitCode(err))
	}
}

// exitError is an error that makes hostrunner exit with code instead of 1.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }

func (e *exitError) Unwrap() error { return e.err }

// exitCode is the code hostrunner exits with after run returned err: 0 for
// nil, the code of an exitError, and 1 for any other error.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := errors.AsType[*exitError](err); ok {
		return e.code
	}
	return 1
}

// run dispatches the subcommand in args; stdout receives `version`'s,
// `update`'s, `instructions`', `allow`'s and `deny`'s output and the
// approval session, which reads the human's answers from stdin; stderr
// flag help, the daemon's log, the Rules test's verdict, the warning of
// `allow` and the questions of `instructions --append`, answered on stdin.
// The caller prints a returned error; see exitCode for the exit code.
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	ctx, release := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer release()
	version := buildVersion(debug.ReadBuildInfo())
	switch args[0] {
	case "up":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cfg, err := parseUp(args[1:], exe, os.Getenv, stderr)
		if err != nil {
			return err
		}
		hostterm.WarnIgnoredRules(stderr, cfg.ignored, cfg.launch.RulesFile)
		if !cfg.restart {
			return launch.Up(ctx, cfg.launch)
		}
		sctx, commands, releaseSignals := stopCommands(stop.Commands{Answer: cfg.answer})
		defer releaseSignals()
		return stop.UpRestart(sctx, cfg.launch, commands, os.Stdin, stderr)
	case "stop", "restart":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cfg, err := parseStop(args[0], args[1:], exe, os.Getenv, stderr)
		if err != nil {
			return err
		}
		if cfg.Restart != nil {
			cfg.Restart.Runtimes = watch.Detect()
		}
		cfg.BuildVersion = version
		sctx, commands, releaseSignals := stopCommands(cfg.Commands)
		defer releaseSignals()
		cfg.Commands = commands
		return stop.Run(sctx, cfg, os.Stdin, stderr)
	case "serve":
		cfg, err := parseServe(args[1:], os.Getenv, stderr)
		if err != nil {
			return err
		}
		var runtimes []watch.Runtime
		if cfg.watch {
			if runtimes = watch.Detect(); len(runtimes) == 0 {
				return errors.New("--watch needs docker or podman in PATH")
			}
		}
		cfg.version = version
		return serve(ctx, cfg, slog.New(slog.NewTextHandler(stderr, nil)), runtimes)
	case "test":
		cfg, err := parseTest(args[1:], os.Getenv, stderr)
		if err != nil {
			return err
		}
		return rulesTest(cfg, stderr)
	case "allow":
		cfg, err := parseAllow(args[1:], os.Getenv, stderr)
		if err != nil {
			return err
		}
		return runAllow(cfg, stdout)
	case "deny":
		cfg, err := parseDeny(args[1:], os.Getenv, stderr)
		if err != nil {
			return err
		}
		return runDeny(cfg, stdout)
	case "approve":
		cfg, err := parseApprove(args[1:], os.Getenv, stderr)
		if err != nil {
			return err
		}
		cfg.BuildVersion = version
		return approval.Run(ctx, cfg, os.Stdin, stdout)
	case "update":
		check, err := parseUpdate(args[1:], stderr)
		if err != nil {
			return err
		}
		return runUpdate(ctx, check, version, stdout)
	case "instructions":
		cfg, err := parseInstructions(args[1:], stderr)
		if err != nil {
			return err
		}
		return runInstructions(cfg, os.Stdin, isTerminal(os.Stdin), stdout, stderr)
	case "version":
		if err := parseVersion(args[1:], stderr); err != nil {
			return err
		}
		_, err := fmt.Fprintln(stdout, version)
		return err
	default:
		return errors.New(usage)
	}
}

// buildVersion is the main module version the Go toolchain stamped into the
// binary, as returned by debug.ReadBuildInfo: the tag for a release or
// `go install …@vX.Y.Z`, a pseudo-version between tags. A build without
// VCS data reports "(devel)" or nothing, which becomes
// hostterm.DevVersion.
func buildVersion(info *debug.BuildInfo, ok bool) string {
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return hostterm.DevVersion
	}
	return info.Main.Version
}

// workspaceFlags are the flags `up` and `serve` share.
type workspaceFlags struct {
	workspace          string // workspace path on the host
	containerWorkspace string // the same workspace's path inside the container
}

// register adds --workspace and --container-workspace to fs.
func (w *workspaceFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&w.workspace, "workspace", "", "workspace directory on the host")
	fs.StringVar(&w.containerWorkspace, "container-workspace", "", "workspace directory inside the container")
}

// absolute makes the host workspace path absolute against the current
// directory, so that `up` and the daemon it starts, which may run
// elsewhere, resolve the same rules file from it.
func (w *workspaceFlags) absolute() error {
	abs, err := filepath.Abs(w.workspace)
	if err != nil {
		return fmt.Errorf("--workspace: %w", err)
	}
	w.workspace = abs
	return nil
}

// parseFlags parses args with fs and rejects positional arguments.
func parseFlags(fs *flag.FlagSet, args []string, usage string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; %s", fs.Arg(0), usage)
	}
	return nil
}

// parseUp parses the flags of `hostrunner up`; --dir and both workspace
// paths are required. exe is the running hostrunner, which becomes the
// daemon; the client is the hostrun binary installed next to it. The
// settings are resolved here, with getenv reading the environment, and
// all of them given to the daemon, with the workspace settings file they
// were read from (see settings.Flags.WorkspaceFile), which the daemon
// protects in its mount check. The allow store up checks is the one
// allow.Open opens. output, which receives the flags' help and errors, is
// where up tells the human what it says besides an error.
func parseUp(args []string, exe string, getenv func(string) string, output io.Writer) (upConfig, error) {
	var (
		dir        string
		ws         workspaceFlags
		restart    bool
		kill, wait bool
		sf         settings.Flags
	)
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&dir, "dir", "", "per-container runtime directory (mounted at /run/hostrunner)")
	ws.register(fs)
	fs.BoolVar(&restart, "restart", false, "replace the daemon serving the runtime directory, if any, with a new one")
	sf.RegisterUp(fs)
	registerAnswer(fs, &kill, &wait)
	if err := parseFlags(fs, args, upUsage); err != nil {
		return upConfig{}, err
	}
	if dir == "" || ws.workspace == "" || ws.containerWorkspace == "" || (kill || wait) && !restart {
		return upConfig{}, errors.New(upUsage)
	}
	answer, err := parseAnswer(kill, wait, upUsage)
	if err != nil {
		return upConfig{}, err
	}
	if err := ws.absolute(); err != nil {
		return upConfig{}, err
	}
	s, err := sf.Resolve(getenv, ws.workspace)
	if _, ok := errors.AsType[*settings.FlagError](err); ok {
		return upConfig{}, fmt.Errorf("%w; %s", err, upUsage)
	}
	if err != nil {
		return upConfig{}, err
	}
	wsSettings, err := sf.WorkspaceFile(getenv, ws.workspace)
	if err != nil {
		return upConfig{}, err
	}
	allows, err := allow.Open(getenv)
	if err != nil {
		return upConfig{}, err
	}
	cfg := daemonBinaries(exe)
	cfg.Dir, cfg.Workspace, cfg.ContainerWorkspace = dir, ws.workspace, ws.containerWorkspace
	cfg.Allows = allows
	cfg.Stderr = output
	cfg.WorkspaceSettings = wsSettings
	cfg.Settings = s.Settings
	return upConfig{launch: cfg, ignored: s.IgnoredRules, restart: restart, answer: answer}, nil
}

// daemonBinaries is the part of a launch.Config that `up` and `restart`
// share: the hostrunner at exe started as the daemon and the hostrun
// beside it installed as the client.
func daemonBinaries(exe string) launch.Config {
	return launch.Config{
		Daemon: exe,
		Client: filepath.Join(filepath.Dir(exe), launch.ClientName),
	}
}

// upConfig is a parsed `hostrunner up`: with restart, the daemon serving
// the runtime directory is replaced, its commands treated as answer says.
type upConfig struct {
	launch  launch.Config
	ignored string // older default rules file left unread, if any
	restart bool
	answer  stop.Answer
}

// registerAnswer registers --kill and --wait, which say what becomes of
// the commands a daemon runs when it is stopped.
func registerAnswer(fs *flag.FlagSet, kill, wait *bool) {
	fs.BoolVar(kill, "kill", false, "kill the commands the daemon runs, without asking")
	fs.BoolVar(wait, "wait", false, "let the commands the daemon runs finish first, without asking")
}

// parseAnswer returns the answer --kill and --wait give, which exclude
// each other (a usage error then, with usage).
func parseAnswer(kill, wait bool, usage string) (stop.Answer, error) {
	switch {
	case kill && wait:
		return stop.Ask, fmt.Errorf("--kill and --wait exclude each other; %s", usage)
	case kill:
		return stop.Kill, nil
	case wait:
		return stop.Wait, nil
	}
	return stop.Ask, nil
}

// parseStop parses the flags of `hostrunner stop` or, for name "restart",
// `hostrunner restart`, which starts the hostrunner at exe, and its
// hostrun beside it, as the new daemons. Without --dir the daemons are
// looked for under the runtime root, $XDG_RUNTIME_DIR/hostrunner (where
// `hostrunner up` puts their runtime directories) unless a setting says;
// the settings are resolved with getenv reading the environment, those
// of a new daemon as up resolves them, for the old daemon's workspace,
// when restart starts it. restart checks them in the allow store
// allow.Open opens, as up does.
func parseStop(name string, args []string, exe string, getenv func(string) string, output io.Writer) (stop.Config, error) {
	usage := stopUsage
	if name == "restart" {
		usage = restartUsage
	}
	var (
		cfg        stop.Config
		kill, wait bool
		sf         settings.Flags
	)
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.Dir, "dir", "", "runtime directory of the daemon (default: the one serving the current directory)")
	fs.BoolVar(&cfg.All, "all", false, "every daemon")
	fs.BoolVar(&cfg.Choose, "choose", false, "choose the daemon from the list")
	registerAnswer(fs, &kill, &wait)
	if name == "restart" {
		sf.RegisterRestart(fs)
	} else {
		sf.RegisterStop(fs)
	}
	if err := parseFlags(fs, args, usage); err != nil {
		return stop.Config{}, err
	}
	chosen := 0
	for _, set := range []bool{cfg.Dir != "", cfg.All, cfg.Choose} {
		if set {
			chosen++
		}
	}
	if chosen > 1 {
		return stop.Config{}, fmt.Errorf("--dir, --all and --choose exclude each other; %s", usage)
	}
	var err error
	if cfg.All {
		// The new daemons' path flags name one workspace's files.
		fs.Visit(func(f *flag.Flag) {
			if err == nil && slices.Contains([]string{"rules-file", "config", "workspace-settings"}, f.Name) {
				err = fmt.Errorf("--%s and --all exclude each other: it names a file of one workspace; %s", f.Name, usage)
			}
		})
		if err != nil {
			return stop.Config{}, err
		}
	}
	if cfg.Answer, err = parseAnswer(kill, wait, usage); err != nil {
		return stop.Config{}, err
	}
	if cfg.Cwd, err = os.Getwd(); err != nil {
		return stop.Config{}, err
	}
	s, err := hostSettings(&sf, getenv, usage)
	if err != nil {
		return stop.Config{}, err
	}
	cfg.StopTimeout = s.StopTimeout
	if cfg.Dir == "" {
		if s.RuntimeRoot == "" {
			return stop.Config{}, errNoRuntimeRoot
		}
		cfg.Root = s.RuntimeRoot
	}
	if name == "restart" {
		allows, err := allow.Open(getenv)
		if err != nil {
			return stop.Config{}, err
		}
		b := daemonBinaries(exe)
		cfg.Restart = &stop.Restart{Daemon: b.Daemon, Client: b.Client, Allows: allows, Settings: func(workspace string) (settings.Resolved, error) {
			return sf.Resolve(getenv, workspace)
		}, WorkspaceSettings: func(workspace string) (string, error) {
			return sf.WorkspaceFile(getenv, workspace)
		}}
	}
	return cfg, nil
}

// stopCommands returns the context a stop, restart or `up --restart` runs
// in, cancelled by SIGTERM, and how it treats the commands of a daemon it
// stops: parsed, the Answer and StopTimeout of the command line, with the
// human on stdin when it is a terminal and Ctrl+C (SIGINT) asking what
// becomes of a draining daemon rather than ending the command; release
// stops both signals.
func stopCommands(parsed stop.Commands) (ctx context.Context, commands stop.Commands, release func()) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	commands = parsed
	commands.Terminal, commands.Interrupts = isTerminal(os.Stdin), ch
	return ctx, commands, func() {
		signal.Stop(ch)
		cancel()
	}
}

// serveConfig holds the settings of `hostrunner serve`.
type serveConfig struct {
	socket string // Unix socket to listen on
	workspaceFlags
	watch             bool        // exit once the armed container stops
	workspaceSettings string      // the workspace settings file, absolute (see settings.Flags.WorkspaceFile)
	version           string      // the build version the daemon reports (see buildVersion)
	allows            allow.Store // where a hand edit of the rules file is checked as allowed
	settings.Settings             // the daemon's settings
}

// parseServe parses the flags of `hostrunner serve`; the socket and both
// workspace paths are required. The settings no flag gives are resolved
// like `up` does, with getenv reading the environment, so a hand-started
// serve runs like one `up` starts; given all of them, as `up` does, it
// reads no other source. The workspace settings file, which a --watch
// daemon protects in its mount check, is named by the rule `up` follows
// (see settings.Flags.WorkspaceFile), so the one `up` passes with
// --workspace-settings. The allow store a hand edit of the rules file
// is checked in is the one allow.Open opens, as `up` checks.
func parseServe(args []string, getenv func(string) string, output io.Writer) (serveConfig, error) {
	var cfg serveConfig
	var sf settings.Flags
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.socket, "socket", "", "Unix socket path to listen on")
	cfg.workspaceFlags.register(fs)
	fs.BoolVar(&cfg.watch, "watch", false, "exit once the armed container stops")
	sf.Register(fs)
	if err := parseFlags(fs, args, serveUsage); err != nil {
		return cfg, err
	}
	if cfg.socket == "" || cfg.workspace == "" || cfg.containerWorkspace == "" {
		return cfg, errors.New(serveUsage)
	}
	if err := cfg.workspaceFlags.absolute(); err != nil {
		return cfg, err
	}
	s, err := sf.ResolveAsGiven(getenv, cfg.workspace)
	if _, ok := errors.AsType[*settings.FlagError](err); ok {
		return cfg, fmt.Errorf("%w; %s", err, serveUsage)
	}
	if err != nil {
		return cfg, err
	}
	cfg.Settings = s
	if cfg.workspaceSettings, err = sf.WorkspaceFile(getenv, cfg.workspace); err != nil {
		return cfg, err
	}
	if cfg.allows, err = allow.Open(getenv); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// parseApprove parses the flags of `hostrunner approve`: --dir, the runtime
// directory of the daemon to serve, whose approval socket is beside it.
// Without it the session lists the daemons of the runtime root,
// $XDG_RUNTIME_DIR/hostrunner (where `hostrunner up` puts their runtime
// directories) unless a setting says, and the current directory chooses
// the default; with it, the session serves that daemon alone. The
// settings are resolved with getenv reading the environment; the number
// of tail lines applies with --dir or without.
func parseApprove(args []string, getenv func(string) string, output io.Writer) (approval.Config, error) {
	var (
		dir string
		sf  settings.Flags
	)
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&dir, "dir", "", "runtime directory of the daemon to serve (default: choose from the list)")
	sf.RegisterApprove(fs)
	if err := parseFlags(fs, args, approveUsage); err != nil {
		return approval.Config{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return approval.Config{}, err
	}
	s, err := hostSettings(&sf, getenv, approveUsage)
	if err != nil {
		return approval.Config{}, err
	}
	c := approval.Config{Cwd: cwd, TailLines: s.ApproveTailLines}
	if dir != "" {
		c.Socket = launch.ApprovalSocket(dir)
		return c, nil
	}
	if s.RuntimeRoot == "" {
		return approval.Config{}, errNoRuntimeRoot
	}
	c.Root = s.RuntimeRoot
	return c, nil
}

// hostSettings resolves the settings of stop, restart or approve that sf
// registered, with getenv reading the environment; a flag error carries
// usage.
func hostSettings(sf *settings.Flags, getenv func(string) string, usage string) (settings.Settings, error) {
	s, err := sf.ResolveHost(getenv)
	if _, ok := errors.AsType[*settings.FlagError](err); ok {
		return s, fmt.Errorf("%w; %s", err, usage)
	}
	return s, err
}

// parseVersion parses the arguments of `hostrunner version`, which takes
// none; output receives the usage for -h.
func parseVersion(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(output)
	return parseFlags(fs, args, versionUsage)
}

// parseUpdate parses the flags of `hostrunner update` and returns whether
// --check was given; output receives the flag help for -h.
func parseUpdate(args []string, output io.Writer) (check bool, err error) {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.BoolVar(&check, "check", false, "only report whether a newer release exists")
	err = parseFlags(fs, args, updateUsage)
	return check, err
}

// runUpdate switches the install of the running hostrunner, whose version
// is current, and its hostrun to the latest release (only reports with check)
// and prints the outcome to stdout. Errors are prefixed "update: ". The
// network calls are bounded by updateTimeout.
func runUpdate(ctx context.Context, check bool, current string, stdout io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	// update.Run needs the version directory, not the bin/ symlink. On
	// Linux os.Executable already resolves symlinks (/proc/self/exe);
	// resolving again keeps that true where it does not.
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	res, err := update.Run(ctx, update.Config{
		APIURL:      update.APIURL,
		DownloadURL: update.DownloadURL,
		Current:     current,
		Dir:         filepath.Dir(exe),
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		CheckOnly:   check,
	})
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	_, err = fmt.Fprintln(stdout, updateMessage(res))
	return err
}

// updateMessage is the line `hostrunner update` prints for res.
func updateMessage(res update.Result) string {
	switch res.Outcome {
	case update.Available:
		return fmt.Sprintf("hostrunner: %s available (current %s)", res.Latest, res.Current)
	case update.Updated:
		return fmt.Sprintf("hostrunner: updated %s -> %s", res.Current, res.Latest)
	default:
		return fmt.Sprintf("hostrunner: already up to date (%s)", res.Current)
	}
}

// defaultInstructionsFile is the file `instructions --append` writes when
// given no path, relative to the current directory.
const defaultInstructionsFile = "AGENTS.md"

// instructionsConfig holds the settings of `hostrunner instructions`.
type instructionsConfig struct {
	append bool   // add the block to path instead of printing it
	path   string // with append: the file to add it to
}

// parseInstructions parses the arguments of `hostrunner instructions`:
// --append and, after it, at most one path, defaultInstructionsFile when
// none is given. A path without --append is an error.
func parseInstructions(args []string, output io.Writer) (instructionsConfig, error) {
	var cfg instructionsConfig
	fs := flag.NewFlagSet("instructions", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.BoolVar(&cfg.append, "append", false, "add the instructions to the file named after the flags (default "+defaultInstructionsFile+") instead of printing them")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	paths := 0
	if cfg.append {
		paths = 1
	}
	if fs.NArg() > paths {
		return cfg, fmt.Errorf("unexpected argument %q; %s", fs.Arg(paths), instructionsUsage)
	}
	if cfg.append {
		cfg.path = defaultInstructionsFile
		if fs.NArg() == 1 {
			cfg.path = fs.Arg(0)
		}
		if cfg.path == "" {
			return cfg, errors.New("empty path; " + instructionsUsage)
		}
	}
	return cfg, nil
}

// runInstructions prints the instructions block to stdout or, with
// cfg.append, adds it to cfg.path, asking on stderr and reading the
// answers from stdin when terminal says stdin is one, then prints the
// outcome to stdout.
func runInstructions(cfg instructionsConfig, stdin io.Reader, terminal bool, stdout, stderr io.Writer) error {
	if !cfg.append {
		_, err := io.WriteString(stdout, instructions.Block())
		return err
	}
	outcome, err := instructions.AppendFile(cfg.path, instructions.Prompt{In: stdin, Out: stderr, Terminal: terminal})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, instructionsMessage(outcome, cfg.path))
	return err
}

// instructionsMessage is the line `instructions --append` prints after
// writing path with outcome.
func instructionsMessage(outcome instructions.Outcome, path string) string {
	switch outcome {
	case instructions.Created:
		return "hostrunner: created " + path
	case instructions.Appended:
		return "hostrunner: appended instructions to " + path
	case instructions.Replaced:
		return "hostrunner: replaced instructions in " + path
	default:
		return "hostrunner: " + path + " left unchanged"
	}
}

// isTerminal reports whether f is a terminal, so a human can answer
// questions read from it. The descriptor is reached through SyscallConn,
// not Fd, which would put a pipe into blocking mode.
func isTerminal(f *os.File) bool {
	raw, err := f.SyscallConn()
	if err != nil {
		return false
	}
	terminal := false
	_ = raw.Control(func(fd uintptr) {
		_, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
		terminal = err == nil
	})
	return terminal
}

// serve runs the daemon until ctx is cancelled or, with watch, until the
// devcontainer has stopped (as reported by runtimes). An arm request from
// `hostrunner up` puts the watcher back into waiting for the container.
//
// The rules file is read here: an invalid file stops the daemon from
// starting (fail closed), a missing one makes it deny every command. Later
// edits, e.g. by a `git pull` on the host, are reread once the daemon sees
// the file changed (every PollInterval), once their content is allowed
// in cfg's allow store for the workspace and the daemon's settings, as
// `up` checks it: an edit not allowed keeps the rules served, and records
// the allow request, until a `hostrunner allow` (see allowOptions).
// Without Reread they take
// effect only in a new daemon, except that an approval session which adds
// a rule makes the daemon reread the file, and one that found the file
// edited by hand has it reread once allowed. The digest of the rules
// reread is the one arms compare.
//
// With watch, the daemon also checks the mounts of the armed container on
// every attach (see protectedPaths and daemon.WithMountCheck): it refuses
// every command until the first check of each arm, and while the latest one finds
// that the container can write its configuration. Without watch there is
// no container to check, and no check; with MountCheck off, which only a
// human on the host can set, none either.
func serve(ctx context.Context, cfg serveConfig, log *slog.Logger, runtimes []watch.Runtime) error {
	policy, err := rules.Load(cfg.RulesFile)
	if err != nil {
		return err
	}
	mapper, err := workspace.NewMapper(cfg.workspace, cfg.containerWorkspace)
	if err != nil {
		return err
	}
	// The approval socket lies beside the runtime directory, the socket's
	// directory, which the container mounts: the container must not reach
	// the channel a human approves on.
	approvalSocket := launch.ApprovalSocket(filepath.Dir(cfg.socket))
	if err := checkApprovalSocket(cfg.workspace, approvalSocket); err != nil {
		return err
	}
	l, err := transport.Unix{Path: cfg.socket}.Listen()
	if err != nil {
		return err
	}
	al, err := transport.Unix{Path: approvalSocket}.Listen()
	if err != nil {
		l.Close()
		return err
	}
	log.Info("listening", "socket", cfg.socket, "approval_socket", approvalSocket, "workspace", cfg.workspace, "container_workspace", cfg.containerWorkspace, "rules_file", cfg.RulesFile, "rules_digest", policy.Digest(), "watch", cfg.watch, "no_reread", !cfg.Reread, "mount_check", cfg.MountCheck)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		watcher   *watch.Watcher
		srv       *daemon.Server  // set before the watcher runs, which calls Attached
		mountOpts []daemon.Option // the mount check's, see wireMountCheck
	)
	if cfg.watch {
		wc := watch.Config{
			Workspace:      cfg.workspace,
			PollInterval:   cfg.PollInterval,
			QueryTimeout:   queryTimeout,
			StartupTimeout: cfg.StartupTimeout,
			Grace:          cfg.Grace,
		}
		mountOpts = wireMountCheck(cfg, &wc, &srv)
		watcher = watch.New(runtimes, wc, log)
	}
	reread := func() (daemon.Policy, string, error) {
		p, err := rules.Load(cfg.RulesFile)
		if err != nil {
			return nil, "", err
		}
		return p, p.Digest(), nil
	}
	opts := []daemon.Option{
		daemon.WithArmHandler(armHandler(cfg.RulesFile, cfg.Reread, cfg.Settings.Daemon(), watcher, log)),
		daemon.WithApprovals(al, cfg.RulesFile),
		daemon.WithReread(policy.Digest(), reread),
		daemon.WithAskTimeout(cfg.AskTimeout),
		daemon.WithRequestTimeout(cfg.RequestTimeout),
		daemon.WithBuildVersion(cfg.version),
		daemon.WithSettings(cfg.Settings.Daemon()),
	}
	opts = append(opts, allowOptions(cfg)...)
	if cfg.Reread {
		opts = append(opts, daemon.WithRereadEvery(cfg.PollInterval))
	}
	if watcher != nil {
		opts = append(opts, daemon.WithContainerStopped(watcher.ContainerStopped))
		opts = append(opts, mountOpts...)
	}
	srv = daemon.New(mapper, policy, log, opts...)
	if watcher != nil {
		go func() {
			defer cancel()
			if err := watcher.Run(ctx); err != nil && ctx.Err() == nil {
				log.Warn("stopping", "reason", err)
			}
		}()
	}
	return srv.Serve(ctx, l)
}

// wireMountCheck sets up the mount check of a `serve --watch` of cfg, the
// one place that looks at cfg.MountCheck: with it on, it sets in wc the
// callbacks that bring the Server, which *srv points to before the
// watcher runs, each arm and the check of each attach, and returns the
// option that has the Server refuse commands by the check's results; with
// it off, it leaves wc as it is and returns none.
func wireMountCheck(cfg serveConfig, wc *watch.Config, srv **daemon.Server) []daemon.Option {
	if !cfg.MountCheck {
		return nil
	}
	wc.Attached = func(armedAt time.Time, armed []watch.Container) {
		(*srv).MountChecked(armedAt, mountcheck.Check(armed, protectedPaths(cfg)))
	}
	// Before `up` gets its reply, so before its container starts.
	wc.Armed = func(armedAt time.Time) { (*srv).MountRearmed(armedAt) }
	return []daemon.Option{daemon.WithMountCheck(settings.MountCheckOffHint())}
}

// protectedPaths are the host paths of cfg's daemon configuration that the
// armed container must not be able to write (see mountcheck.Check): the
// workspace's .devcontainer directory, the rules file and the workspace
// settings file, wherever these two lie.
func protectedPaths(cfg serveConfig) []string {
	return []string{filepath.Join(cfg.workspace, rulesfile.Dir), cfg.RulesFile, cfg.workspaceSettings}
}

// allowOptions returns the options that have the daemon of cfg reread
// only rules file content allowed in cfg's allow store for its workspace,
// its runtime directory and its daemon settings, as `up` checks them (with
// the settings the daemon gives, its own or those of a change an arm
// reports not allowed), and record a pending request of its own rules file
// and settings for content not allowed under its runtime directory, so
// that `hostrunner allow` allows what the daemon checks. They also have it
// allow a rule added from the approval session to a rules file that was
// allowed, for every runtime directory that covered it (see
// allow.Store.AllowAddedRule). The runtime directory is the one its socket
// lies in, beside which its approval socket lies, the cfg.Dir of the `up`
// that started it.
func allowOptions(cfg serveConfig) []daemon.Option {
	place := allow.Place{Workspace: cfg.workspace, RuntimeDir: filepath.Dir(cfg.socket)}
	// target is what the daemon's own rules file and settings, with the
	// given digest of its content, are allowed as.
	target := func(digest string) allow.Target {
		return allow.NewTarget(cfg.RulesFile, digest, cfg.Settings.Daemon())
	}
	return []daemon.Option{
		daemon.WithAllowed(func(sc allow.Scope) (bool, error) {
			return cfg.allows.Allowed(place, sc)
		}),
		daemon.WithAllowRequest(func(digest string) error {
			return cfg.allows.Request(place, target(digest))
		}),
		daemon.WithAllowRecorder(func(writtenOver allow.Scope, digest string) error {
			_, err := cfg.allows.AllowAddedRule(place, writtenOver, digest)
			return err
		}),
	}
}

// checkApprovalSocket refuses an approval socket inside the workspace,
// which the container mounts and so could reach to answer its own
// requests. Other mounts the daemon cannot see: the runtime directory
// `hostrunner up` uses lies under $XDG_RUNTIME_DIR, which no container
// mounts whole.
func checkApprovalSocket(workspace, socket string) error {
	ws, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(socket))
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(ws, dir); err == nil && filepath.IsLocal(rel) {
		return fmt.Errorf("approval socket %s lies inside the workspace %s, which the container mounts", socket, workspace)
	}
	return nil
}

// armHandler answers the arms of `hostrunner up`, which arms the daemon on
// every container start with the digest of the rules it validated and
// the daemon settings it resolved. digest is that of the rules the daemon
// serves from rulesFile, which a reread changes; ownSettings are the
// daemon's settings (settings.Settings.Daemon), reread among them. The
// daemon steps aside for a new one when the digests differ, when a
// setting the arm carries is not its own, when an arm carrying none, from
// an `up` that predates them, does not reread as the daemon does, or when
// watcher has ended and will not follow the arm; otherwise watcher, if
// any, waits for the container of this arm.
func armHandler(rulesFile string, reread bool, ownSettings map[string]string, watcher *watch.Watcher, log *slog.Logger) func(a protocol.Arm, digest string) (stepAside bool) {
	return func(a protocol.Arm, digest string) bool {
		if a.ConfigDigest != digest {
			log.Info("stepping aside: the rules file changed", "rules_file", rulesFile)
			return true
		}
		if rereads := !a.NoReread; a.Settings == nil && rereads != reread {
			log.Info("stepping aside: --no-reread changed", "no_reread", a.NoReread)
			return true
		}
		if changed := changedSettings(ownSettings, a.Settings); len(changed) > 0 {
			var args []any // each key changed, with the daemon's value and the arm's
			for _, name := range changed {
				own, ok := ownSettings[name]
				if !ok {
					own = "(none)"
				}
				args = append(args, name, own+" → "+a.Settings[name])
			}
			log.Info("stepping aside: the settings changed", args...)
			return true
		}
		if watcher == nil {
			return false
		}
		if err := watcher.Arm(); err != nil {
			log.Info("stepping aside", "reason", err)
			return true
		}
		return false
	}
}

// changedSettings returns the keys, sorted, of the settings in armed
// whose value differs from own; a key own lacks differs, whatever its
// value.
func changedSettings(own, armed map[string]string) []string {
	var changed []string
	for name, v := range armed {
		if got, ok := own[name]; !ok || got != v {
			changed = append(changed, name)
		}
	}
	slices.Sort(changed)
	return changed
}
