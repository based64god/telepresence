package itest

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/stretchr/testify/require"

	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/v2/pkg/client/cli/cmd"
)

type StatusResponse struct {
	RootDaemon cmd.RootDaemonStatus `json:"root_daemon,omitempty"`
	UserDaemon cmd.UserDaemonStatus `json:"user_daemon,omitempty"`
	Error      string               `json:"err,omitempty"`
}

func TelepresenceStatus(ctx context.Context, args ...string) (*StatusResponse, error) {
	stdout, stderr, err := Telepresence(ctx, append([]string{"status", "--output", "json"}, args...)...)
	var status StatusResponse
	jErr := json.Unmarshal([]byte(stdout), &status)
	if err != nil {
		if jErr == nil && status.Error != "" {
			dlog.Error(ctx, status.Error)
			return nil, errors.New(status.Error)
		}
		dlog.Error(ctx, stderr)
		return nil, err
	}
	if jErr != nil {
		return nil, jErr
	}
	return &status, nil
}

func TelepresenceStatusOk(ctx context.Context, args ...string) *StatusResponse {
	status, err := TelepresenceStatus(ctx, args...)
	require.NoError(getT(ctx), err)
	return status
}
