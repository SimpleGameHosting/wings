package docker

import (
	"context"
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
	return nil
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
// The created container must instead be force removed so the boot's own error
// handling settles the state.
func TestTerminateDoesNotMarkABootingServerOffline(t *testing.T) {
	fake := &fakeDockerClient{inspect: inspectWithState(false, "created")}
	e := newTerminationTestEnvironment(fake, environment.ProcessStartingState)

	if err := e.Terminate(context.Background(), "SIGKILL"); err != nil {
		t.Fatal(err)
	}
	if got := e.State(); got != environment.ProcessStartingState {
		t.Fatalf("expected environment to stay in the starting state, got %q", got)
	}
	if !fake.removedForced {
		t.Fatal("expected the created container to be force removed so the boot aborts")
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
