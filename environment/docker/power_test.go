package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/system"
)

// containerNotFoundError mimics the Docker SDK's not-found error so that
// client.IsErrNotFound treats it exactly like a missing container.
type containerNotFoundError struct{}

func (containerNotFoundError) Error() string { return "no such container" }

func (containerNotFoundError) NotFound() {}

// fakeDockerClient satisfies client.APIClient through embedding and overrides
// only the calls the termination path makes.
type fakeDockerClient struct {
	client.APIClient

	inspect    container.InspectResponse
	inspectErr error
	removeErr  error

	killedSignal  string
	removedForced bool
}

func (f *fakeDockerClient) ContainerInspect(_ context.Context, _ string) (container.InspectResponse, error) {
	return f.inspect, f.inspectErr
}

func (f *fakeDockerClient) ContainerKill(_ context.Context, _ string, signal string) error {
	f.killedSignal = signal
	return nil
}

func (f *fakeDockerClient) ContainerRemove(_ context.Context, _ string, options container.RemoveOptions) error {
	f.removedForced = options.Force
	return f.removeErr
}

// inspectWithState builds the minimal inspect payload the termination path reads.
func inspectWithState(running bool, status string) container.InspectResponse {
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			State: &container.State{Running: running, Status: status},
		},
	}
}

// newTerminationTestEnvironment wires an Environment around the fake client
// with just enough state for SignalContainer and Terminate to operate.
func newTerminationTestEnvironment(fake *fakeDockerClient, state string) *Environment {
	return &Environment{
		Id:      "termination-test",
		client:  fake,
		st:      system.NewAtomicString(state),
		emitter: events.NewBus(),
	}
}

// TestTerminateDoesNotMarkABootingServerOffline covers half of
// pterodactyl/panel#5712: a kill sent while the container was created but not
// yet started marked the booting server offline while the boot kept running.
// The created container must instead be force removed with the published
// state walked to stopping first, so that neither the attach stream's own
// offline transition nor the boot's error handling can look like a crash.
func TestTerminateDoesNotMarkABootingServerOffline(t *testing.T) {
	fake := &fakeDockerClient{inspect: inspectWithState(false, "created")}
	e := newTerminationTestEnvironment(fake, environment.ProcessStartingState)

	stateEvents := make(chan []byte, 8)
	e.Events().On(stateEvents)
	defer e.Events().Off(stateEvents)

	if err := e.Terminate(context.Background(), "SIGKILL"); err != nil {
		t.Fatal(err)
	}
	if got := e.State(); got != environment.ProcessStoppingState {
		t.Fatalf("expected environment to publish the stopping state, got %q", got)
	}
	if !fake.removedForced {
		t.Fatal("expected the created container to be force removed so the boot aborts")
	}

	// The environment must never publish offline itself here; the boot's
	// failure handling owns that transition after the removal...
	for {
		select {
		case raw := <-stateEvents:
			event := events.MustDecode(raw)
			if state, _ := event.Data.(string); state == environment.ProcessOfflineState {
				t.Fatal("expected terminate not to publish an offline state for a booting server")
			}
		default:
			return
		}
	}
}

// TestTerminateRestoresStartingWhenRemovalFails ensures a docker failure
// while aborting a boot does not strand the published state at stopping: the
// boot still owns its container, so the starting state it relies on for
// console-based promotion must be restored.
func TestTerminateRestoresStartingWhenRemovalFails(t *testing.T) {
	fake := &fakeDockerClient{
		inspect:   inspectWithState(false, "created"),
		removeErr: errors.New("simulated docker daemon hiccup"),
	}
	e := newTerminationTestEnvironment(fake, environment.ProcessStartingState)

	if err := e.Terminate(context.Background(), "SIGKILL"); err == nil {
		t.Fatal("expected the docker removal failure to be returned to the caller")
	}
	if got := e.State(); got != environment.ProcessStartingState {
		t.Fatalf("expected the starting state to be restored after a failed removal, got %q", got)
	}
}

// TestTerminateDuringLateBootLeavesOfflineToTheStream ensures a kill that
// catches the container after it started, while the environment still says
// starting, kills the container but leaves the offline transition to the
// attach stream so crash detection sees stopping as the previous state.
func TestTerminateDuringLateBootLeavesOfflineToTheStream(t *testing.T) {
	fake := &fakeDockerClient{inspect: inspectWithState(true, "running")}
	e := newTerminationTestEnvironment(fake, environment.ProcessStartingState)

	if err := e.Terminate(context.Background(), "SIGKILL"); err != nil {
		t.Fatal(err)
	}
	if fake.killedSignal != "SIGKILL" {
		t.Fatalf("expected SIGKILL to be sent to the running container, got %q", fake.killedSignal)
	}
	if got := e.State(); got != environment.ProcessStoppingState {
		t.Fatalf("expected the state to stop at stopping until the stream closes, got %q", got)
	}
}

// TestTerminateStillStopsARunningServer guards the normal kill flow.
func TestTerminateStillStopsARunningServer(t *testing.T) {
	fake := &fakeDockerClient{inspect: inspectWithState(true, "running")}
	e := newTerminationTestEnvironment(fake, environment.ProcessRunningState)

	if err := e.Terminate(context.Background(), "SIGKILL"); err != nil {
		t.Fatal(err)
	}
	if got := e.State(); got != environment.ProcessOfflineState {
		t.Fatalf("expected a running server to be marked offline after terminate, got %q", got)
	}
	if fake.killedSignal != "SIGKILL" {
		t.Fatalf("expected SIGKILL to be sent to the container, got %q", fake.killedSignal)
	}
}

// TestTerminateBeforeContainerCreationReportsInstead ensures a kill that
// arrives before the boot has created a container reports the situation
// honestly instead of pretending the server went offline.
func TestTerminateBeforeContainerCreationReportsInstead(t *testing.T) {
	fake := &fakeDockerClient{inspectErr: containerNotFoundError{}}
	e := newTerminationTestEnvironment(fake, environment.ProcessStartingState)

	if err := e.Terminate(context.Background(), "SIGKILL"); err == nil {
		t.Fatal("expected terminating before the container exists to return an error instead of faking offline")
	}
	if got := e.State(); got != environment.ProcessStartingState {
		t.Fatalf("expected environment to stay in the starting state, got %q", got)
	}
}

// TestTerminateWithNoContainerWhileStoppedStaysOffline guards the existing
// behavior for servers that are already stopped when the kill arrives.
func TestTerminateWithNoContainerWhileStoppedStaysOffline(t *testing.T) {
	fake := &fakeDockerClient{inspectErr: containerNotFoundError{}}
	e := newTerminationTestEnvironment(fake, environment.ProcessOfflineState)

	if err := e.Terminate(context.Background(), "SIGKILL"); err != nil {
		t.Fatal(err)
	}
	if got := e.State(); got != environment.ProcessOfflineState {
		t.Fatalf("expected an already stopped server to stay offline, got %q", got)
	}
}
