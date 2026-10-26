package server

import (
	"testing"

	"emperror.dev/errors"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waitResult(status container.WaitResponse) (<-chan container.WaitResponse, <-chan error) {
	statusChannel := make(chan container.WaitResponse, 1)
	statusChannel <- status
	return statusChannel, make(chan error)
}

func TestAwaitInstallContainerSucceedsOnZeroExit(t *testing.T) {
	require.NoError(t, awaitInstallContainer(waitResult(container.WaitResponse{StatusCode: 0})))
}

func TestAwaitInstallContainerFailsOnNonZeroExit(t *testing.T) {
	err := awaitInstallContainer(waitResult(container.WaitResponse{StatusCode: 100}))

	var exitError *installScriptExitError
	require.True(t, errors.As(err, &exitError))
	assert.Equal(t, int64(100), exitError.code)
	assert.Contains(t, err.Error(), "exited with code 100")
}

func TestAwaitInstallContainerFailsOnWaitError(t *testing.T) {
	err := awaitInstallContainer(waitResult(container.WaitResponse{Error: &container.WaitExitError{Message: "boom"}}))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestAwaitInstallContainerReturnsDockerError(t *testing.T) {
	errorChannel := make(chan error, 1)
	errorChannel <- errors.New("daemon gone")

	err := awaitInstallContainer(make(chan container.WaitResponse), errorChannel)

	assert.EqualError(t, err, "daemon gone")
}
