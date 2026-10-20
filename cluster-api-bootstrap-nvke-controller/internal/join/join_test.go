// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package join

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClient is a scripted Client. Each KubeadmStatus call returns the next
// entry in statuses; the last entry repeats, so a poll loop that never reaches
// a terminal state keeps seeing it.
type fakeClient struct {
	statuses  []Status
	statusErr error
	joinErr   error

	statusCalls int
	joinCalls   int
	issued      []Request
}

func (c *fakeClient) KubeadmStatus(_ context.Context) (Status, error) {
	c.statusCalls++
	if c.statusErr != nil {
		return Status{}, c.statusErr
	}
	if len(c.statuses) == 0 {
		return Status{}, nil
	}
	index := min(c.statusCalls-1, len(c.statuses)-1)
	return c.statuses[index], nil
}

func (c *fakeClient) ExecuteKubeadmJoin(_ context.Context, req Request) error {
	c.joinCalls++
	c.issued = append(c.issued, req)
	return c.joinErr
}

var _ Client = (*fakeClient)(nil)

// Statuses the agent reports, in the order a worker passes through them.
var (
	statusFresh   = Status{State: "uninitialized", AgentVersion: "v0.3.0"}
	statusJoining = Status{State: StatePending, Phase: PhaseJoin, Message: "Joining node"}
	statusJoined  = Status{State: StateReady, Phase: PhaseJoin, Message: "node successfully joined"}
	statusFailed  = Status{State: StateError, Phase: PhaseJoin, Message: "exit status 1"}
)

func validRequest() Request {
	return Request{
		Material: Material{
			APIServerEndpoint: "10.0.0.1:6443",
			Token:             "abcdef.0123456789abcdef",
			CACertHashes:      []string{"sha256:" + strings.Repeat("a", 64)},
		},
		Configuration: "apiVersion: kubeadm.k8s.io/v1beta3\nkind: JoinConfiguration\n",
		Files: []File{{
			Path:    "/etc/kubernetes/patches/kubeletconfiguration0+merge.yaml",
			Content: []byte("kind: KubeletConfiguration\n"),
			Mode:    0o644,
			Owner:   "root:root",
		}},
	}
}

func testJoiner(client Client) *Joiner {
	// Short budgets keep the timeout case fast; the loop is the same one the
	// defaults drive.
	return &Joiner{Client: client, PollInterval: time.Millisecond, Timeout: 50 * time.Millisecond}
}

func TestJoinerJoin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		client        *fakeClient
		request       Request
		wantOutcome   Outcome
		wantStatus    Status
		wantJoinCalls int
		assertErr     func(t *testing.T, err error)
	}{
		{
			name:          "clean join",
			client:        &fakeClient{statuses: []Status{statusFresh, statusJoining, statusJoined}},
			request:       validRequest(),
			wantOutcome:   OutcomeJoined,
			wantJoinCalls: 1,
			assertErr:     func(t *testing.T, err error) { t.Helper(); require.NoError(t, err) },
		},
		{
			name:          "already joined issues nothing",
			client:        &fakeClient{statuses: []Status{statusJoined}},
			request:       validRequest(),
			wantOutcome:   OutcomeAlreadyJoined,
			wantJoinCalls: 0,
			assertErr:     func(t *testing.T, err error) { t.Helper(); require.NoError(t, err) },
		},
		{
			name:          "a join in flight is polled, not reissued",
			client:        &fakeClient{statuses: []Status{statusJoining, statusJoining, statusJoined}},
			request:       validRequest(),
			wantOutcome:   OutcomeResumed,
			wantJoinCalls: 0,
			assertErr:     func(t *testing.T, err error) { t.Helper(); require.NoError(t, err) },
		},
		{
			name:          "a previously failed join is reissued",
			client:        &fakeClient{statuses: []Status{statusFailed, statusJoining, statusJoined}},
			request:       validRequest(),
			wantOutcome:   OutcomeJoined,
			wantJoinCalls: 1,
			assertErr:     func(t *testing.T, err error) { t.Helper(); require.NoError(t, err) },
		},
		{
			name:          "join failure reports the agent's message",
			client:        &fakeClient{statuses: []Status{statusFresh, statusJoining, statusFailed}},
			request:       validRequest(),
			wantJoinCalls: 1,
			wantStatus:    statusFailed,
			assertErr: func(t *testing.T, err error) {
				t.Helper()
				var failed *FailedError
				require.ErrorAs(t, err, &failed)
				assert.Equal(t, "exit status 1", failed.Message)
			},
		},
		{
			name:          "a join still running at the deadline times out",
			client:        &fakeClient{statuses: []Status{statusFresh, statusJoining}},
			request:       validRequest(),
			wantJoinCalls: 1,
			wantStatus:    statusJoining,
			assertErr: func(t *testing.T, err error) {
				t.Helper()
				require.ErrorIs(t, err, ErrTimeout)
				// The last status is reported, so the caller can say what the
				// node was doing when the budget ran out.
				assert.Contains(t, err.Error(), "Joining node")
			},
		},
		{
			name:          "a rejected request issues nothing",
			client:        &fakeClient{statuses: []Status{statusFresh}},
			request:       Request{Configuration: "kind: JoinConfiguration\n"},
			wantJoinCalls: 0,
			assertErr: func(t *testing.T, err error) {
				t.Helper()
				require.ErrorContains(t, err, "the API server endpoint is required")
			},
		},
		{
			name:          "an unreachable agent is not mistaken for a failed join",
			client:        &fakeClient{statusErr: errors.New("connection refused")},
			request:       validRequest(),
			wantJoinCalls: 0,
			assertErr: func(t *testing.T, err error) {
				t.Helper()
				require.ErrorContains(t, err, "connection refused")
				var failed *FailedError
				assert.NotErrorAs(t, err, &failed)
			},
		},
		{
			name:          "a refused join surfaces as an error",
			client:        &fakeClient{statuses: []Status{statusFresh}, joinErr: errors.New("unavailable")},
			request:       validRequest(),
			wantJoinCalls: 1,
			wantStatus:    statusFresh,
			assertErr: func(t *testing.T, err error) {
				t.Helper()
				require.ErrorContains(t, err, "issuing kubeadm join")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			result, err := testJoiner(test.client).Join(t.Context(), test.request)
			test.assertErr(t, err)
			assert.Equal(t, test.wantJoinCalls, test.client.joinCalls)

			if err != nil {
				// An error describes what happened; the outcome would only
				// repeat it less precisely.
				assert.Empty(t, result.Outcome)
				assert.Equal(t, test.wantStatus, result.Status, "the last status the agent reported")
				return
			}
			assert.Equal(t, test.wantOutcome, result.Outcome)
			assert.True(t, result.Status.Joined(), "a successful call ends on a joined status")
		})
	}
}

func TestJoinerJoinSendsTheRequestThrough(t *testing.T) {
	t.Parallel()

	client := &fakeClient{statuses: []Status{statusFresh, statusJoined}}
	request := validRequest()

	_, err := testJoiner(client).Join(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, client.issued, 1)
	assert.Equal(t, request, client.issued[0])
}

func TestJoinerCompleted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		client  *fakeClient
		want    bool
		wantErr string
	}{
		{name: "a joined node is complete", client: &fakeClient{statuses: []Status{statusJoined}}, want: true},
		{name: "a joining node is not", client: &fakeClient{statuses: []Status{statusJoining}}},
		{name: "a failed node is not", client: &fakeClient{statuses: []Status{statusFailed}}},
		{
			// The agent does not reload its state file, so a restart during a
			// join reports a fresh node. Nothing here can tell that apart from
			// a node that never started one.
			name:   "an agent restarted mid-join reports incomplete",
			client: &fakeClient{statuses: []Status{statusFresh}},
		},
		{name: "an unreachable agent errors", client: &fakeClient{statusErr: errors.New("connection refused")}, wantErr: "connection refused"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := testJoiner(test.client).Completed(t.Context())
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestJoinerWaitHonoursTheContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := testJoiner(&fakeClient{statuses: []Status{statusJoining}}).Wait(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrTimeout, "a cancelled caller is not a timed-out join")
}

func TestMaterialCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		material Material
		want     string
	}{
		{
			name:     "one hash",
			material: Material{APIServerEndpoint: "10.0.0.1:6443", Token: "abcdef.0123456789abcdef", CACertHashes: []string{"sha256:aa"}},
			want:     "kubeadm join 10.0.0.1:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:aa",
		},
		{
			name:     "a rotated CA has two",
			material: Material{APIServerEndpoint: "cp.example:6443", Token: "abcdef.0123456789abcdef", CACertHashes: []string{"sha256:aa", "sha256:bb"}},
			want:     "kubeadm join cp.example:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:aa --discovery-token-ca-cert-hash sha256:bb",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := test.material.Command()
			assert.Equal(t, test.want, got)
			// The agent derives the node's role from this string and the NVKE
			// controller's parser requires the prefix.
			assert.True(t, strings.HasPrefix(got, "kubeadm join "))
			assert.NotContains(t, got, "--control-plane")
		})
	}
}

func TestRequestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(r *Request)
		wantErr string
	}{
		{name: "a complete worker request", mutate: func(*Request) {}},
		{
			name:    "no endpoint",
			mutate:  func(r *Request) { r.Material.APIServerEndpoint = "" },
			wantErr: "the API server endpoint is required",
		},
		{
			name:    "no token",
			mutate:  func(r *Request) { r.Material.Token = "" },
			wantErr: "the bootstrap token is required",
		},
		{
			name:    "no hash",
			mutate:  func(r *Request) { r.Material.CACertHashes = nil },
			wantErr: "at least one discovery CA certificate hash is required",
		},
		{
			name:    "a bare hex hash without kubeadm's type prefix",
			mutate:  func(r *Request) { r.Material.CACertHashes = []string{strings.Repeat("a", 64)} },
			wantErr: "type:value",
		},
		{
			name:    "whitespace would split the command into extra argv entries",
			mutate:  func(r *Request) { r.Material.Token = "abcdef.0123 456789abcdef" },
			wantErr: "must not contain whitespace",
		},
		{
			// The agent only runs the command string when the configuration is
			// empty, and that path execs `kubeadm kubeadm join …`.
			name:    "no configuration",
			mutate:  func(r *Request) { r.Configuration = "" },
			wantErr: "the kubeadm join configuration is required",
		},
		{
			name:    "a control-plane configuration",
			mutate:  func(r *Request) { r.Configuration += "controlPlane:\n  localAPIEndpoint: {}\n" },
			wantErr: "workers only",
		},
		{
			name:    "a relative file path",
			mutate:  func(r *Request) { r.Files[0].Path = "etc/kubernetes/patch.yaml" },
			wantErr: "path must be absolute",
		},
		{
			name:    "a file with no mode",
			mutate:  func(r *Request) { r.Files[0].Mode = 0 },
			wantErr: "mode is required",
		},
		{
			name:    "a file mode with more than permission bits",
			mutate:  func(r *Request) { r.Files[0].Mode = 0o4755 },
			wantErr: "only permission bits",
		},
		{
			name:    "a file with no owner",
			mutate:  func(r *Request) { r.Files[0].Owner = "" },
			wantErr: "user:group",
		},
		{
			name: "two files on one path",
			mutate: func(r *Request) {
				duplicate := r.Files[0]
				r.Files = append(r.Files, duplicate)
			},
			wantErr: "duplicate path",
		},
		{
			name:    "a file over the agent's 1 MiB limit",
			mutate:  func(r *Request) { r.Files[0].Content = make([]byte, maxFileBytes+1) },
			wantErr: "exceeds the agent's",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			request := validRequest()
			test.mutate(&request)

			err := request.Validate()
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestStatusPredicates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      Status
		wantJoined  bool
		wantJoining bool
		wantFailed  bool
	}{
		{name: "a fresh agent", status: statusFresh},
		{name: "joining", status: statusJoining, wantJoining: true},
		{name: "joined", status: statusJoined, wantJoined: true},
		{name: "failed", status: statusFailed, wantFailed: true},
		{
			// The agent's constants are lowercase, but the NVKE controller
			// folds case before comparing and so does this.
			name:       "the case the agent reports does not matter",
			status:     Status{State: "Ready", Phase: "Join"},
			wantJoined: true,
		},
		{
			// A ready node in another phase has not joined. A worker never
			// runs one, but init leaves exactly this state behind.
			name:   "ready after an init is not joined",
			status: Status{State: StateReady, Phase: "initialize"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.wantJoined, test.status.Joined(), "Joined")
			assert.Equal(t, test.wantJoining, test.status.Joining(), "Joining")
			assert.Equal(t, test.wantFailed, test.status.Failed(), "Failed")
		})
	}
}
