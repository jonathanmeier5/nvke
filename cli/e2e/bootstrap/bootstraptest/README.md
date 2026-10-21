# bootstraptest — the `bootstrap site` e2e lanes as Go tests

This package runs `nvkectl bootstrap site` from a Go test and states what the
run produced.

The lanes for `bootstrap site` grew as two large shell scripts,
`../run.sh` and `../run-nico.sh`. Their assertions are greps over a log file.
This package moves the part that decides whether a run passed into Go, where
an assertion names what it checks and a failure says what it saw.

## What runs where

| Lane | Command | Shape |
|---|---|---|
| provisioned, Go | `make -C cli test-e2e-bootstrap-go` | 1 node, no VIP |
| nico, Go | `make -C cli test-e2e-bootstrap-nico-go` | 1 node, no VIP |
| provisioned, shell | `make -C cli test-e2e-bootstrap` | 3 nodes, VIP |
| nico, shell | `make -C cli test-e2e-bootstrap-nico` | 3 nodes, VIP |

The Go lanes stand up a one-node site with no control-plane VIP. A VIP is
advertised by kube-vip over BGP, and it becomes reachable only when a speaker
on the host learns the `/32`. That speaker is still started by the shell
lanes, so a Go lane that asked for a VIP would write a kubeconfig that points
at an address it cannot reach. Set `NVKECTL_E2E_NODES` and
`NVKECTL_E2E_CONTROL_PLANE_VIP` to ask for another shape.

Both kinds of lane read the same `NVKECTL_E2E_*` variables, so one environment
drives all four.

## What each Go lane owns

Both lanes own the site spec, the command, and the assertions on the cluster.

The provisioned lane also owns its node containers: `../substrate` is already
a Go package, so the test starts the containers, reads their TPM endorsement
key thumbprints, writes the node inventory, and runs
`nvkectl generate sitespec` against it.

The nico lane takes a prepared host. The node network, `nico-sim` and the
profiles image are still stood up by `../run-nico.sh`. The test checks for
them first and names what is missing, so an unprepared host reports it in a
second instead of failing inside the bootstrap. Moving that preparation into
Go is the next piece of work.

## Prerequisites

Both lanes need root, because they write iptables rules and put an address on
a bridge. Run them under `sudo -E`.

They also need `nerdctl`, `kind`, `kubectl`, `helm`, `clusterctl`, `iptables`
and `ip` on `PATH`, and `NVKECTL_NVKE_CONTROLLER_IMAGE` set to a controller
image that is already in containerd.

An unmet prerequisite skips the test. Set `NVKECTL_E2E_REQUIRE=1` to make it a
failure instead. CI must set it: a lane that skips every run looks exactly
like a lane that passes.

## What runs in the merge gate

The tests above are behind the `e2e_bootstrap` build tag, so `make test` does
not try to build a cluster. The gate still compiles this package and runs its
unit tests. Those cover:

- the command line the options render, so a flag cannot be dropped silently;
- the process lifecycle — a milestone seen while the command still runs, a
  command that exits before its milestone, and `Stop` ending the whole process
  group rather than the direct child;
- the node assertions, including that a one-node cluster does not satisfy a
  three-node site;
- the nico site spec, rendered from a struct and then given to the real site
  spec loader. That check is what catches a schema change here, in the gate,
  rather than twenty minutes into a lane.

## Moving more out of the shell

Take one assertion at a time from `../run.sh` or `../run-nico.sh`, add it
here, and delete it there. The pieces that are worth moving next, in order of
value:

1. the BGP speaker, which is what keeps the Go lanes to one node;
2. the nico host preparation: `nico-sim`, the node network and the profiles
   image;
3. the etcd-encryption checks, which already live in `../etcd-lib.sh` as two
   functions with clear inputs;
4. the SPIRE attestation checks, which `../substrate` already has Go support
   for.
