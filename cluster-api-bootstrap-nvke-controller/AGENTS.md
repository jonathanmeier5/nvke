# AGENTS.md

## Scope

The NVKE bootstrap provider: a Cluster API bootstrap provider that prepares a
worker and joins it through nke-agent.

Read the root [AGENTS.md](../AGENTS.md) first.

## What this provider is for

Cluster API's bootstrap contract is a **data** contract. A bootstrap provider
writes a secret; the infrastructure provider delivers it to the host. CABPK
puts a whole `kubeadm join` into cloud-init and the host runs it at first boot.

NVKE's model is **imperative**. nke-agent runs on every node, and the
controller tells it what to do over gRPC. A node that joins from cloud-init
alone never passes through the agent, so its labels, taints, CNI and node
conditions are never set and it sits outside the model.

So this provider does both, in two phases separated by the life of the host:

| Phase | What | Why there |
|---|---|---|
| 1 | Render the **boot** configuration, publish the data secret, report `dataSecretCreated` | CAPNICo holds the NicoMachine at `WaitingForBootstrapData` until this exists, so no host exists yet |
| 2 | Once the agent answers: mint a token, render the **join** configuration, drive `ExecuteKubeadmJoin`, then apply labels, taints and CNI | The host must exist first |

Phase 2 is outside the stock contract. Keep phase 1 strictly standard so the
deviation stays in one place.

## Decisions already made

Do not revisit these without saying why:

- **Join material comes from the API**, as CABPK does: mint a bootstrap token
  against the workload cluster using the `<cluster>-kubeconfig` secret. Do not
  call `GetKubeadmJoinString` on a control-plane node's agent.
- **The data secret carries boot configuration only.** No join configuration at
  rest. It is written before the control-plane endpoint is certain.
- **No kubelet `--provider-id`.** A controller back-fills the Node's provider ID
  after registration — CAPNICo's `reconcileNodeProviderID`. Format is
  `nico://<instance-id>`.
- **The attestation method is a field on `NvkeConfigTemplate`**, not on the
  infrastructure template. It decides how the agent proves identity, which is
  bootstrap behaviour. Values match the NVKE controller's existing set:
  `tpm`, `joinToken`, `dsxImds`.
- **Cluster API owns these workers.** They are not imported into the NVKE
  controller, so nothing here may depend on a node row.

## The cost of not importing

Every piece of NVKE controller logic that keys on a node row stops working for
these Machines. Known cases:

| Logic | Keys on | Consequence |
|---|---|---|
| Provider-ID back-fill | — | None. CAPNICo's copy is independent |
| SPIRE agent path and entry | `host-id` annotation to a node row | Must be rebuilt here |
| The join path | `db.Node` | Rewrite against CRDs |

When another appears, prefer re-deriving the value from Cluster API objects
over giving the provider its own record of it.

## SPIFFE entries are not optional

A node with no SPIRE registration entry gets no workload SVID. nke-agent calls
`workloadapi.NewX509Source`, which waits for the first SVID, so **the gRPC
server never starts listening**. The log stops at:

```
setting up SPIFFE mTLS for gRPC
```

No error, no crash, no restart loop. The thing that looks broken is this
provider timing out on a connection, one layer from the cause. When phase 2
cannot reach the agent, say so explicitly and name the missing entry as the
first thing to check.

## Layout

```
api/v1alpha1/     NvkeConfig, NvkeConfigTemplate. Generated deepcopy.
internal/         Controllers and the packages they use.
cmd/manager/      The manager binary.
config/           Generated CRDs and RBAC.
```

## Verification

```bash
make fmt
make vet
make test
make generate-check   # fails when the generated output is stale
```

`api/v1alpha1/zz_generated.deepcopy.go` and everything under `config/` are
generated. Change the types or the markers, then run `make generate manifests`
and commit the result with the change that caused it.

## Conventions

Follow the root guide: wrap errors with `%w`, log with `log/slog` or the
controller-runtime logger, table-driven tests with testify, and `FIXME:` with a
tracking link rather than `TODO:`.
