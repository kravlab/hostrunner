# Armed container by start time

The daemon followed "some container of the workspace is running", so after
an arm it could not tell the container being replaced from the one coming
up, and ignored whatever it saw for a grace after the arm. A container that
came up and stopped within that window, or within one poll after it, was
never attached to, and the daemon stayed until its startup timeout
([#12](https://github.com/kravlab/hostrunner/issues/12)). The daemon now
follows its armed container: it polls the runtimes for the workspace's
containers, stopped ones included, and a container whose start time is
later than the arm is the armed container at once, even if it has already
stopped. Only when no such container shows up does a container started
before the arm become the armed container, once it is still running a grace
after the arm.

## Considered Options

- **Remembering container IDs at the arm**: a container that lives less
  than one poll is still missed, and `devcontainer up` on a stopped
  container starts the same ID again. Rejected.
- **Following `docker events` / `podman events`**: nothing is missed, but
  it is a long-lived stream per runtime to restart when it breaks, with
  different formats, and podman's events depend on its events backend.
  Rejected: a stopped container keeps its start time, so polling with
  stopped containers included misses nothing either.
- **A snapshot of `(ID, start time)` at the arm** instead of comparing
  with the arm's time: no clock involved, but the reply to the arm, and so
  `up`, would wait on runtime queries, and a runtime that does not answer
  then leaves no snapshot. Rejected: the runtime and the daemon share the
  host's clock, and the container starts only after the arm is answered.
- **Attaching at once to a container running at the arm** and treating
  its disappearance within a grace as a rebuild: the same guess inverted,
  with an attach to undo. Rejected.
- **Accepting and documenting the gap**: the window was the whole grace
  (15 s), not the 2 s poll, so a container that fails right after starting
  hit it. Rejected.

## Consequences

- Each poll lists the workspace's containers in every state and, when
  there are any, inspects them for whether they run and when they started:
  two runtime calls instead of one.
- A stopped armed container still gets the grace before the daemon exits,
  so `docker restart` or a restart policy does not end the daemon.
- A container run with `--rm` (through `runArgs`) leaves no record when it
  stops, so one that stops before a poll sees it is still missed.
- The devcontainer tooling labels only the primary service of a Compose
  devcontainer with `devcontainer.local_folder`; other services do not
  affect the daemon.
