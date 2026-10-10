# Spec: a daemon that is ending steps aside

Ticket: [Spec: a daemon that is ending steps aside instead of accepting an arm](https://github.com/kravlab/hostrunner/issues/54)
(from [#51](https://github.com/kravlab/hostrunner/issues/51)).
Extends [watch-armed-container.md](watch-armed-container.md); vocabulary
in [CONTEXT.md](../../CONTEXT.md) (Lifecycle, step aside).

## Goal

An arm that landed while a poll ran could be accepted and then lost: the
same poll ended the watch, and the daemon exited after telling `up` it
would stay. Now every arm is either followed or answered with step aside.

## Watcher

- A mutex orders `Arm` against `Run`'s decision to end. `Arm` returns nil
  when it accepts, or why the watch ended when it refuses, and never waits
  for runtime queries.
- Before each give-up exit (armed container absent for a grace, startup
  timeout, no runtime answered), `Run` takes a pending arm instead of
  ending: back to waiting with a fresh startup timeout. Otherwise it marks
  the watch ended. Cancelling `Run`'s context marks it ended at once
  (`context.AfterFunc`), not when the loop notices, so no arm is accepted
  while the daemon stops.

## Daemon and `up`

- `serve --watch` answers a refused arm with step aside (`Armed{restart:
  true}`), as it does for other rules, and logs why (`stepping aside`
  with the watch's reason, or the rules file changed). The field and protocol version stay;
  its doc now describes step aside.
- `up` behaves as before on step aside; its messages no longer assume
  changed rules ("did not stop within … after stepping aside", "the new
  daemon stepped aside; retry").

## Tests

- `internal/watch`, in `testing/synctest` bubbles so the held poll is the
  deciding one: `watchtest.Runtime.Hold` holds a query (also past
  cancellation, like a slow CLI) while the test arms, for each give-up
  exit: the arm is accepted and the watch follows a new container. An arm
  after cancellation during a held query is refused. Arms after each of
  the four exits are refused; arms while waiting or attached are accepted.
- `cmd/hostrunner`: the arm handler steps aside once the watch has ended
  (by cancellation) and for other rules. (Through `serve` the window between the end of the
  watch and the socket closing cannot be hit reliably.)
- `internal/launch`: the existing step-aside tests.
