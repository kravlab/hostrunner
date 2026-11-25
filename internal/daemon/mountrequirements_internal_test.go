package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/mountcheck"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/workspace"
)

// echoRequirements are the mount requirements of the echo rule of
// mountCheckServer.
var echoRequirements = []rules.MountRequirement{
	{Path: ".git", Mode: rules.MountMounted, Rule: "echo"},
	{Path: ".git/config", Mode: rules.MountReadOnly, Rule: "echo"},
}

// echoRule is the echo rule of mountCheckServer with its requirements, as
// the per-request check is given them for a request of that rule.
var echoRule = []ruleRequirements{{rule: "echo", reqs: echoRequirements}}

// requirementsSeen is a live mount check (see MountCheck) that counts the
// calls of its protected-path stage, which returns protected, and records
// the requirements of every call of its requirements stage, in order,
// which returns result.
type requirementsSeen struct {
	mu        sync.Mutex
	checks    int
	seen      [][]rules.MountRequirement
	protected error
	result    error
}

// check counts the call and returns the protected-path stage's result, or
// the requirements stage when it is nil.
func (r *requirementsSeen) check(context.Context) (MountRequirementsCheck, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks++
	if r.protected != nil {
		return nil, r.protected
	}
	return r.requirements, nil
}

// requirements records reqs and returns the result set.
func (r *requirementsSeen) requirements(reqs []rules.MountRequirement) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, reqs)
	return r.result
}

// calls returns how many checks were made, and the requirements the
// requirements stage got, in order.
func (r *requirementsSeen) calls() (checks int, seen [][]rules.MountRequirement) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checks, append([][]rules.MountRequirement(nil), r.seen...)
}

// served has s handle a connection of its own whose client sends a frame
// of type ft carrying v, and returns the frames the client gets until the
// connection closes.
func served(t *testing.T, s *Server, ft protocol.FrameType, v any) <-chan []protocol.Frame {
	t.Helper()
	daemonEnd, client := net.Pipe()
	t.Cleanup(func() { client.Close() })
	go s.handle(context.Background(), daemonEnd)
	if err := protocol.WriteJSON(client, ft, v); err != nil {
		t.Fatal(err)
	}
	return collectFrames(client)
}

// echoRequest is a request for argv from the workspace root of
// mountCheckServer.
func echoRequest(argv ...string) protocol.Request {
	return protocol.Request{Version: protocol.Version, Argv: argv, Cwd: "/workspaces/app"}
}

// wantRequirementsSeen fails the test unless the requirements stage of
// check got want, in order.
func wantRequirementsSeen(t *testing.T, check *requirementsSeen, want ...[]rules.MountRequirement) {
	t.Helper()
	if _, seen := check.calls(); !reflect.DeepEqual(seen, want) {
		t.Fatalf("the requirements stage got %+v, want %+v", seen, want)
	}
}

// A run's two live checks, as it arrives and right before its command
// starts, are given the mount requirements of the rule that allows it.
func TestARunsMountChecksGetItsRulesRequirements(t *testing.T) {
	var check requirementsSeen
	s, _ := mountCheckServer(t, check.check)

	got := <-served(t, s, protocol.FrameRequest, echoRequest("echo"))

	if len(got) == 0 || got[len(got)-1].Type != protocol.FrameExit {
		t.Fatalf("the client got frames %v, want them to end with its exit", frameTypes(got))
	}
	wantRequirementsSeen(t, &check, echoRequirements, echoRequirements)
}

// Both live checks of a dry run, as it arrives and its last, are given
// the mount requirements of the rule that allows it.
func TestADryRunsMountCheckGetsItsRulesRequirements(t *testing.T) {
	var check requirementsSeen
	s, _ := mountCheckServer(t, check.check)

	got := <-served(t, s, protocol.FrameDryRun, echoRequest("echo"))

	if len(got) != 1 || got[0].Type != protocol.FrameAllowed {
		t.Fatalf("the client got frames %v, want Allowed alone", frameTypes(got))
	}
	wantRequirementsSeen(t, &check, echoRequirements, echoRequirements)
}

// An --ask for an argv its rule refuses (echo takes no arguments) is
// checked as it arrives, before it is queued, with the mount requirements
// of that rule: a failure refuses it there.
func TestAnAsksArrivalMountCheckGetsTheRequirementsOfTheRuleRefusingIt(t *testing.T) {
	check := requirementsSeen{result: errors.New("the mounts cannot be checked")}
	s, peer := mountCheckServer(t, check.check)
	collectFrames(peer)

	got := <-served(t, s, protocol.FrameAsk, protocol.Ask{Request: echoRequest("echo", "hi")})

	if len(got) != 1 || got[0].Type != protocol.FrameError {
		t.Fatalf("the client got frames %v, want its refusal alone", frameTypes(got))
	}
	wantRequirementsSeen(t, &check, echoRequirements)
}

// waitingID waits until s has one request waiting for an answer, and
// returns its id.
func waitingID(t *testing.T, s *Server) uint64 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.approvals.mu.Lock()
		for id := range s.approvals.waiting {
			s.approvals.mu.Unlock()
			return id
		}
		s.approvals.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no request is waiting for an answer")
	return 0
}

// An --ask the human approves is checked again right before the approved
// run starts, with the mount requirements of the rule that refused it.
func TestAnApprovedRunsMountCheckGetsItsRulesRequirements(t *testing.T) {
	var check requirementsSeen
	s, peer := mountCheckServer(t, check.check)
	collectFrames(peer)

	client := served(t, s, protocol.FrameAsk, protocol.Ask{Request: echoRequest("echo", "hi")})
	id := waitingID(t, s)
	if why, _ := s.approvals.answer(s.approvals.serving, protocol.Answer{ID: id, Scope: protocol.ScopeOnce}); why != "" {
		t.Fatalf("the answer was not taken: %s", why)
	}
	got := <-client

	if len(got) == 0 || got[len(got)-1].Type != protocol.FrameExit {
		t.Fatalf("the client got frames %v, want them to end with its exit", frameTypes(got))
	}
	wantRequirementsSeen(t, &check, echoRequirements, echoRequirements)
}

// The check right before an approved run starts uses the requirements of
// the rule the run was approved as, not those the rules served by then
// give the same rule: a reread while the --ask waits, which changes the
// echo rule's mounts, pairs no rule with another's requirements.
func TestAnApprovedRunIsCheckedWithTheRequirementsItWasApprovedWith(t *testing.T) {
	var check requirementsSeen
	s, peer := mountCheckServer(t, check.check)
	collectFrames(peer)
	reread, err := rules.Parse([]byte("rules:\n  - command: echo\n    args: none\n    mounts:\n      src: mounted\n"))
	if err != nil {
		t.Fatal(err)
	}

	client := served(t, s, protocol.FrameAsk, protocol.Ask{Request: echoRequest("echo", "hi")})
	id := waitingID(t, s)
	s.mu.Lock()
	s.policy = reread
	s.mu.Unlock()
	if why, _ := s.approvals.answer(s.approvals.serving, protocol.Answer{ID: id, Scope: protocol.ScopeOnce}); why != "" {
		t.Fatalf("the answer was not taken: %s", why)
	}
	got := <-client

	if len(got) == 0 || got[len(got)-1].Type != protocol.FrameExit {
		t.Fatalf("the client got frames %v, want them to end with its exit", frameTypes(got))
	}
	wantRequirementsSeen(t, &check, echoRequirements, echoRequirements)
}

// A repeat of a standing approval, for an argv its rule refuses, is
// checked as a run is, with the mount requirements of that rule.
func TestARepeatsMountChecksGetItsRulesRequirements(t *testing.T) {
	var check requirementsSeen
	s, _ := mountCheckServer(t, check.check)
	req := echoRequest("echo", "hi")
	if _, ok := s.approvals.grant(req, false, pathPasses{}, nil); !ok {
		t.Fatal("the standing approval was not granted")
	}

	got := <-served(t, s, protocol.FrameRequest, req)

	if len(got) == 0 || got[len(got)-1].Type != protocol.FrameExit {
		t.Fatalf("the client got frames %v, want them to end with its exit", frameTypes(got))
	}
	wantRequirementsSeen(t, &check, echoRequirements, echoRequirements)
}

// A request no rule starts is checked without mount requirements: only its
// protected paths are checked.
func TestARequestNoRuleStartsGetsNoRequirements(t *testing.T) {
	var check requirementsSeen
	s, _ := mountCheckServer(t, check.check)

	<-served(t, s, protocol.FrameRequest, echoRequest("true"))

	checks, seen := check.calls()
	if checks == 0 {
		t.Fatal("the request was not checked")
	}
	if len(seen) != 0 {
		t.Fatalf("the requirements stage got %+v, want no call", seen)
	}
}

// A request of a longer rule without mounts of its own (`echo plain`) is
// checked with the requirements of the shorter rule that starts it (echo),
// the rule of each named: the longer rule inherits them.
func TestARequestOfALongerRuleGetsTheRequirementsOfTheShorterRule(t *testing.T) {
	var check requirementsSeen
	s, _ := mountCheckServer(t, check.check)

	got := <-served(t, s, protocol.FrameRequest, echoRequest("echo", "plain", "x"))

	if len(got) == 0 || got[len(got)-1].Type != protocol.FrameExit {
		t.Fatalf("the client got frames %v, want them to end with its exit", frameTypes(got))
	}
	wantRequirementsSeen(t, &check, echoRequirements, echoRequirements)
}

// A requirement of the shorter rule that fails refuses the request of the
// longer rule naming the shorter rule that declares it, not the longer one
// that selects the request, in the refusal, the daemon's log and the
// notice.
func TestAFailedRequirementOfAShorterRuleNamesThatRule(t *testing.T) {
	check := requirementsSeen{result: requirementFailures["not mounted"].failure}
	s, peer := mountCheckServer(t, check.check)
	frames := collectFrames(peer)

	got := <-served(t, s, protocol.FrameRequest, echoRequest("echo", "plain", "x"))

	if len(got) != 1 {
		t.Fatalf("the client got frames %v, want its refusal alone", frameTypes(got))
	}
	e := decoded[protocol.Error](t, got[0], protocol.FrameError)
	if e.Code != protocol.ExitRejected || e.Message != requirementFailures["not mounted"].message {
		t.Fatalf("the client got %+v, want code %d, %q", e, protocol.ExitRejected, requirementFailures["not mounted"].message)
	}
	want := []protocol.MountCheckFailed{{Error: requirementFailures["not mounted"].failure.Error(), Rule: "echo"}}
	if got := noticedFailures(t, s, frames); !reflect.DeepEqual(got, want) {
		t.Fatalf("the session was told of %+v, want %+v", got, want)
	}
}

// requirementFailures are failed mount requirements of the echo rule, each
// with the message of its refusal.
var requirementFailures = map[string]struct {
	failure error
	message string
}{
	"not mounted": {
		&mountcheck.NotMountedError{RequirementFailure: mountcheck.RequirementFailure{Container: "c0ffee", Path: ".git", ContainerPath: "/workspaces/app/.git"}},
		`the container does not have /workspaces/app/.git as a mount point of its own, which rule "echo" requires: the command is refused until it is mounted; a human on the host can turn the mount check off: turn it off`,
	},
	"writable": {
		&mountcheck.ReadOnlyRequirementError{RequirementFailure: mountcheck.RequirementFailure{Container: "c0ffee", Path: ".git/config", ContainerPath: "/workspaces/app/.git/config"}, Source: "/host/app/.git/config"},
		`the container can write /workspaces/app/.git/config, which rule "echo" requires to be read-only: the command is refused until it is mounted read-only; a human on the host can turn the mount check off: turn it off`,
	},
}

// A failed mount requirement refuses a run, a dry run and an --ask with
// 126, naming the container path, the reason and the rule, and how to
// turn the check off; no human is asked about it.
func TestAFailedMountRequirementRefusesTheRequest(t *testing.T) {
	for name, tc := range requirementFailures {
		for _, ft := range []protocol.FrameType{protocol.FrameRequest, protocol.FrameDryRun, protocol.FrameAsk} {
			t.Run(fmt.Sprintf("%s/%d", name, ft), func(t *testing.T) {
				check := requirementsSeen{result: tc.failure}
				s, peer := mountCheckServer(t, check.check)
				collectFrames(peer)
				var v any = echoRequest("echo")
				if ft == protocol.FrameAsk {
					v = protocol.Ask{Request: echoRequest("echo", "hi")}
				}

				got := <-served(t, s, ft, v)

				if len(got) != 1 {
					t.Fatalf("the client got frames %v, want its refusal alone", frameTypes(got))
				}
				if e := decoded[protocol.Error](t, got[0], protocol.FrameError); e.Code != protocol.ExitRejected || e.Message != tc.message {
					t.Fatalf("the client got %+v, want code %d, %q", e, protocol.ExitRejected, tc.message)
				}
				s.approvals.mu.Lock()
				defer s.approvals.mu.Unlock()
				if len(s.approvals.waiting) != 0 {
					t.Fatalf("requests %v wait for an answer, want none", s.approvals.waiting)
				}
			})
		}
	}
}

// unresolvableRequirement is a failure of the requirements stage other
// than a failed requirement: a required path that cannot be resolved.
var unresolvableRequirement = &mountcheck.UnresolvableError{Path: "/host/app/.git/config", Err: errors.New("too many levels of symbolic links")}

// A requirements stage that cannot check the rule's mount requirements
// refuses the commands of that rule only, naming it, and how to turn the
// check off.
func TestAnUncheckableMountRequirementRefusesTheRulesCommand(t *testing.T) {
	check := requirementsSeen{result: unresolvableRequirement}
	s, _ := mountCheckServer(t, check.check)

	r := s.mount.requestRefusal(context.Background(), echoRule)

	const want = `the daemon cannot check the mounts rule "echo" requires: the command is refused; a human on the host can turn the mount check off: turn it off`
	if r == nil || r.code != protocol.ExitRejected || r.message != want {
		t.Fatalf("got %+v, want code %d, %q", r, protocol.ExitRejected, want)
	}
}

// The session is told of a requirements stage that cannot check the
// rule's mount requirements with the rule in the frame's Rule.
func TestAnUncheckableMountRequirementIsNoticedWithItsRule(t *testing.T) {
	check := requirementsSeen{result: unresolvableRequirement}
	s, peer := mountCheckServer(t, check.check)
	frames := collectFrames(peer)

	s.mount.requestRefusal(context.Background(), echoRule)

	want := []protocol.MountCheckFailed{{Error: unresolvableRequirement.Error(), Rule: "echo"}}
	if got := noticedFailures(t, s, frames); !reflect.DeepEqual(got, want) {
		t.Fatalf("the session was told of %+v, want %+v", got, want)
	}
}

// requirementsStage returns the per-request mount check whose
// protected-path stage passes and whose requirements stage returns the
// results in turn, one per call.
func requirementsStage(results ...error) MountCheck {
	var calls atomic.Int32
	return func(context.Context) (MountRequirementsCheck, error) {
		return func([]rules.MountRequirement) error { return results[calls.Add(1)-1] }, nil
	}
}

// The session is told of a failed mount requirement once as it starts
// failing, with the rule in the frame's Rule and not in its Error: not
// again for the same container path and reason, again after a check
// passed, and for another container path.
func TestAFailedMountRequirementIsNoticedOncePerFailure(t *testing.T) {
	notMounted := requirementFailures["not mounted"].failure
	writable := requirementFailures["writable"].failure
	results := []error{notMounted, notMounted, nil, notMounted, writable}
	s, peer := mountCheckServer(t, requirementsStage(results...))
	frames := collectFrames(peer)

	for range results {
		s.mount.requestRefusal(context.Background(), echoRule)
	}

	const (
		notMountedText = `container c0ffee: .git is not a mount of its own: nothing is mounted at /workspaces/app/.git`
		writableText   = `container c0ffee: .git/config is not read-only: host /host/app/.git/config is mounted writable at /workspaces/app/.git/config`
	)
	want := []protocol.MountCheckFailed{
		{Error: notMountedText, Rule: "echo"},
		{Error: notMountedText, Rule: "echo"},
		{Error: writableText, Rule: "echo"},
	}
	if got := noticedFailures(t, s, frames); !reflect.DeepEqual(got, want) {
		t.Fatalf("the session was told of %+v, want %+v", got, want)
	}
}

// failingRequirements returns the per-request mount check whose
// protected-path stage passes and whose requirements stage fails with
// failure for every rule.
func failingRequirements(failure error) MountCheck {
	return func(context.Context) (MountRequirementsCheck, error) {
		return func([]rules.MountRequirement) error { return failure }, nil
	}
}

// A passing check of a request of another rule, here one without mount
// requirements, says nothing of the failed requirement of git: the session
// is told of it once, however the requests of the two rules alternate.
func TestAFailedMountRequirementIsNoticedOnceAcrossOtherRulesPassing(t *testing.T) {
	s, peer := mountCheckServer(t, failingRequirements(requirementFailures["not mounted"].failure))
	frames := collectFrames(peer)
	git := []ruleRequirements{{rule: "git", reqs: echoRequirements}}

	for range 3 {
		s.mount.requestRefusal(context.Background(), git)
		s.mount.requestRefusal(context.Background(), nil)
	}

	got := noticedFailures(t, s, frames)
	if len(got) != 1 || got[0].Rule != "git" {
		t.Fatalf("the session was told of %+v, want of the failure of git once", got)
	}
}

// requirementsFailingFor returns the per-request mount check whose
// protected-path stage passes and whose requirements stage fails with
// failure for the requirements that rule declares, and passes the others.
func requirementsFailingFor(rule string, failure error) MountCheck {
	return func(context.Context) (MountRequirementsCheck, error) {
		return func(reqs []rules.MountRequirement) error {
			if reqs[0].Rule == rule {
				return failure
			}
			return nil
		}, nil
	}
}

// A request that a longer rule selects is refused for the requirement of
// the shorter rule that fails, the rule that declares it named, while the
// longer rule's own requirement passes: the session is told of the failure
// of the shorter rule once, however often the request repeats.
func TestAFailedRequirementOfOneOfTwoRulesNamesThatRuleOnly(t *testing.T) {
	failure := requirementFailures["not mounted"].failure
	s, peer := mountCheckServer(t, requirementsFailingFor("git", failure))
	frames := collectFrames(peer)
	rrs := []ruleRequirements{
		{rule: "git", reqs: []rules.MountRequirement{{Path: ".git", Mode: rules.MountMounted, Rule: "git"}}},
		{
			rule: "git commit",
			reqs: []rules.MountRequirement{{Path: ".git/hooks", Mode: rules.MountReadOnly, Rule: "git commit"}},
		},
	}

	var r *refusal
	for range 2 {
		r = s.mount.requestRefusal(context.Background(), rrs)
	}

	if r == nil || !strings.Contains(r.message, `rule "git" requires`) {
		t.Fatalf("got %+v, want a refusal naming rule git", r)
	}
	want := []protocol.MountCheckFailed{{Error: failure.Error(), Rule: "git"}}
	if got := noticedFailures(t, s, frames); !reflect.DeepEqual(got, want) {
		t.Fatalf("the session was told of %+v, want %+v", got, want)
	}
}

// When the requirements of both rules fail, the request is refused for the
// first, and the session is told of the failure of each rule.
func TestFailedRequirementsOfTwoRulesAreEachNoticed(t *testing.T) {
	failure := requirementFailures["not mounted"].failure
	s, peer := mountCheckServer(t, failingRequirements(failure))
	frames := collectFrames(peer)
	rrs := []ruleRequirements{
		{rule: "git", reqs: []rules.MountRequirement{{Path: ".git", Mode: rules.MountMounted, Rule: "git"}}},
		{rule: "git commit", reqs: []rules.MountRequirement{{Path: ".git", Mode: rules.MountMounted, Rule: "git commit"}}},
	}

	r := s.mount.requestRefusal(context.Background(), rrs)

	if r == nil || !strings.Contains(r.message, `rule "git" requires`) {
		t.Fatalf("got %+v, want a refusal naming rule git", r)
	}
	want := []protocol.MountCheckFailed{
		{Error: failure.Error(), Rule: "git"},
		{Error: failure.Error(), Rule: "git commit"},
	}
	if got := noticedFailures(t, s, frames); !reflect.DeepEqual(got, want) {
		t.Fatalf("the session was told of %+v, want %+v", got, want)
	}
}

// A check of a request of the same rule that passes its requirements ends
// the failure: the next failure of that rule is told again.
func TestAFailedMountRequirementIsNoticedAgainAfterThatRulePassed(t *testing.T) {
	failure := requirementFailures["not mounted"].failure
	s, peer := mountCheckServer(t, requirementsStage(failure, nil, failure))
	frames := collectFrames(peer)

	for range 3 {
		s.mount.requestRefusal(context.Background(), echoRule)
	}

	want := []protocol.MountCheckFailed{{Error: failure.Error(), Rule: "echo"}, {Error: failure.Error(), Rule: "echo"}}
	if got := noticedFailures(t, s, frames); !reflect.DeepEqual(got, want) {
		t.Fatalf("the session was told of %+v, want %+v", got, want)
	}
}

// Two rules whose requirements fail on the same container path, for the
// same reason, refuse the commands of each: the session is told of the
// failure of each rule.
func TestTwoRulesFailingOnOnePathAreEachNoticed(t *testing.T) {
	failure := requirementFailures["not mounted"].failure
	s, peer := mountCheckServer(t, failingRequirements(failure))
	frames := collectFrames(peer)

	s.mount.requestRefusal(context.Background(), []ruleRequirements{{rule: "git", reqs: echoRequirements}})
	s.mount.requestRefusal(context.Background(), []ruleRequirements{{rule: "git status", reqs: echoRequirements}})

	want := []protocol.MountCheckFailed{{Error: failure.Error(), Rule: "git"}, {Error: failure.Error(), Rule: "git status"}}
	if got := noticedFailures(t, s, frames); !reflect.DeepEqual(got, want) {
		t.Fatalf("the session was told of %+v, want %+v", got, want)
	}
}

// The refusal of a failed mount requirement carries the rule in its error,
// which the daemon logs, although the session's notice names it apart.
func TestAFailedMountRequirementsRefusalErrorNamesTheRule(t *testing.T) {
	check := requirementsSeen{result: requirementFailures["not mounted"].failure}
	s, _ := mountCheckServer(t, check.check)

	r := s.mount.requestRefusal(context.Background(), echoRule)

	if r == nil {
		t.Fatal("the request was not refused")
	}
	if want := `mount check failed: rule "echo": container c0ffee: `; !strings.HasPrefix(r.err.Error(), want) {
		t.Fatalf("the refusal's error is %q, want it to start with %q", r.err, want)
	}
}

// The session is told of a protected path the container can write, which
// refuses every command, without a rule: the frame's Rule is empty even
// for a request of a rule with mount requirements.
func TestAFailedProtectedPathIsNoticedWithoutARule(t *testing.T) {
	failure := writableAt("/host/first")
	check := requirementsSeen{protected: failure}
	s, peer := mountCheckServer(t, check.check)
	frames := collectFrames(peer)

	s.mount.requestRefusal(context.Background(), echoRule)

	want := []protocol.MountCheckFailed{{Error: failure.Error()}}
	if got := noticedFailures(t, s, frames); !reflect.DeepEqual(got, want) {
		t.Fatalf("the session was told of %+v, want %+v", got, want)
	}
}

// Without the mount check (mount_check: false), a rule's mount
// requirements are not checked: its command runs.
func TestWithoutTheMountCheckARulesRequirementsAreNotChecked(t *testing.T) {
	mapper, err := workspace.NewMapper(t.TempDir(), "/workspaces/app")
	if err != nil {
		t.Fatal(err)
	}
	p, err := rules.Parse([]byte("rules:\n  - command: echo\n    args: none\n    mounts:\n      .git: mounted\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(mapper, p, slog.New(slog.DiscardHandler))

	got := <-served(t, s, protocol.FrameRequest, echoRequest("echo"))

	if len(got) == 0 || got[len(got)-1].Type != protocol.FrameExit {
		t.Fatalf("the client got frames %v, want them to end with its exit", frameTypes(got))
	}
	if e := decoded[protocol.Exit](t, got[len(got)-1], protocol.FrameExit); e.Code != 0 {
		t.Fatalf("the command exited %d, want 0", e.Code)
	}
}
