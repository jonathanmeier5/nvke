// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/external"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	bootstrapv1 "github.com/dsx-ai-factory/nvke/cluster-api-bootstrap-nvke-controller/api/v1alpha1"
)

// The SPIFFE path prefixes a SPIRE agent attests under, one per node attestor.
// They are the NVKE controller's values (internal/constants/constants.go) and
// they are contract rather than a free choice: the SPIRE server derives the
// agent's own ID from the attestation itself, so an entry whose parentID does
// not match it character for character parents nothing.
const (
	tpmAgentPathPrefix     = "/spire/agent/tpm/"
	dsxIMDSAgentPathPrefix = "/spire/agent/dsx_imds/tenant/"
	// The join-token agent's real ID embeds the token, which is a secret, so
	// SPIRE publishes this alternate ID for the entry to parent on instead.
	joinTokenAgentPathPrefix = "/join-token-spire-agent/host-id/" //nolint:gosec // A SPIFFE path prefix, not a credential.
)

const (
	// clusterStaticEntryKind and its group are the spire-controller-manager
	// CRD that turns a Kubernetes object into a SPIRE registration entry.
	clusterStaticEntryKind = "ClusterStaticEntry"

	// spireEntryNamePrefix keeps provider-owned entries out of the NVKE
	// controller's "host-<id>" namespace. Two field managers applying one
	// object name fight over ownership of every field; a separate prefix means
	// each controller owns the entries it created and neither reverts the
	// other.
	spireEntryNamePrefix = "capi-host-"

	// spireEntryFieldManager is this provider's server-side-apply identity.
	spireEntryFieldManager = "nvke-bootstrap-spire-entry"

	// managedByLabel and managedByMachineSpireEntry are copied verbatim from
	// the NVKE controller, and the value must stay the NVKE controller's
	// field-manager name even though this provider is not that controller.
	//
	// On a TPM site the SPIRE server's allowlist ConfigMap is rebuilt by
	// listing ClusterStaticEntries with exactly this label pair and reading the
	// EK public hash out of each parentID. An entry labelled anything else is
	// never added to the allowlist, so the node never node-attests, gets no
	// agent SVID, and the workload entry below is moot — with no error anywhere
	// to say so.
	managedByLabel             = "nvke.nvidia.com/managed-by"
	managedByMachineSpireEntry = "machine-spire-entry-controller"

	// defaultSpireClassName is the controller class nvkectl installs the
	// hardened SPIRE chart with. spire-controller-manager ignores an entry
	// whose className is neither empty nor its own, and so does the allowlist
	// rebuild above.
	defaultSpireClassName = "spire-mgmt-spire"
)

// agentIdentityRequeue is how long to wait for the infrastructure provider to
// publish the attestation material. It arrives when the host boots, which is
// minutes after the Machine exists, and nothing this provider watches changes
// when it does.
const agentIdentityRequeue = 30 * time.Second

var (
	// errAgentIdentityPending means the attestation material the agent's
	// SPIFFE path is built from has not been published yet. It is a wait, not
	// a failure: the entry cannot exist before the host that attests does.
	errAgentIdentityPending = errors.New("the agent identity is not published yet")

	// errAttestationMethodUnsupported means the method names an attestor this
	// provider cannot build a parent ID for.
	errAttestationMethodUnsupported = errors.New("unsupported attestation method")
)

var clusterStaticEntryGVK = schema.GroupVersionKind{ //nolint:gochecknoglobals // A constant GVK; schema.GroupVersionKind is not a const type.
	Group:   "spire.spiffe.io",
	Version: "v1alpha1",
	Kind:    clusterStaticEntryKind,
}

// SpireEntryReconciler registers the SPIRE entry one Cluster API-owned worker
// needs, so nke-agent on that host can obtain a workload SVID.
//
// Without the entry the agent calls workloadapi.NewX509Source, which waits for
// a first SVID that never arrives, and its gRPC server never starts listening.
// There is no error and no crash: the failure surfaces one layer away, as this
// provider timing out on the join. Check this entry first.
//
// The NVKE controller does the same job in MachineSpireEntryController, but
// keys both halves of the registration on its own database node row. These
// workers are owned by Cluster API and are never imported, so there is no row
// to key on and the derivation is rebuilt here from Cluster API objects alone.
type SpireEntryReconciler struct {
	client.Client

	// TrustDomain is the SPIRE trust domain the workload and its agent live
	// in. It is the only part of either SPIFFE ID that is not derivable from
	// the objects in the management cluster.
	TrustDomain string

	// SpireClassName must equal the spire-controller-manager's configured
	// class. An entry carrying any other class is read and then ignored, which
	// looks exactly like an entry that was never created. Defaults to
	// defaultSpireClassName.
	SpireClassName string

	// DSXIMDSOperatorTenant is the tenant segment of the dsxImds agent path.
	// It is read only for that attestation method.
	DSXIMDSOperatorTenant string
}

// +kubebuilder:rbac:groups=spire.spiffe.io,resources=clusterstaticentries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=*,verbs=get;list;watch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

// Reconcile keeps one Machine's ClusterStaticEntry in step with the Machine.
func (r *SpireEntryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	machine := &clusterv1.Machine{}
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Another bootstrap provider's Machine is not this controller's to
	// register, and it has no NvkeConfig to read the attestation method from.
	if !isNvkeBootstrapConfigRef(machine.Spec.Bootstrap.ConfigRef) {
		return ctrl.Result{}, nil
	}

	hostID := machineHostID(machine)

	// A ClusterStaticEntry is cluster-scoped, so it cannot carry an owner
	// reference to a namespaced Machine: Kubernetes treats a cluster-scoped
	// object with a namespaced owner as having an invalid owner and collects
	// it immediately. Deletion therefore has to be explicit, in the window
	// Cluster API's own finalizer holds the Machine open.
	if !machine.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.deleteEntry(ctx, hostID)
	}

	config := &bootstrapv1.NvkeConfig{}
	configKey := client.ObjectKey{Namespace: machine.Namespace, Name: machine.Spec.Bootstrap.ConfigRef.Name}
	if err := r.Get(ctx, configKey, config); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("waiting for the NvkeConfig the Machine references", "nvkeConfig", configKey.Name)
			return ctrl.Result{RequeueAfter: agentIdentityRequeue}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting NvkeConfig %s/%s: %w", configKey.Namespace, configKey.Name, err)
	}

	method := config.Spec.AttestationMethod
	if method == "" {
		method = bootstrapv1.AttestationMethodTPM
	}

	identity, err := r.agentIdentity(ctx, machine, hostID, method)
	if err != nil {
		if errors.Is(err, errAgentIdentityPending) {
			logger.V(1).Info("waiting for the infrastructure provider to publish the agent identity", "attestationMethod", method)
			return ctrl.Result{RequeueAfter: agentIdentityRequeue}, nil
		}
		return ctrl.Result{}, err
	}

	agentPath, err := agentSPIFFEIDPath(method, identity)
	if err != nil {
		if errors.Is(err, errAgentIdentityPending) {
			logger.V(1).Info("waiting for the infrastructure provider to publish the agent identity", "attestationMethod", method)
			return ctrl.Result{RequeueAfter: agentIdentityRequeue}, nil
		}
		return ctrl.Result{}, fmt.Errorf("deriving the agent SPIFFE path for machine %s/%s: %w", machine.Namespace, machine.Name, err)
	}

	if method == bootstrapv1.AttestationMethodJoinToken {
		// The entry is applied anyway so it is already in place when minting
		// lands, but the parent it names does not exist until a token is
		// created with this alternate ID and written into the boot data.
		// FIXME: https://github.com/dsx-ai-factory/nvke/issues/1 — mint the join
		// token during phase 1 and write it into the boot data.
		logger.Info("the joinToken entry parents on an agent that exists only once a token is minted with the same ID", "agentPath", agentPath)
	}

	parentID := spiffeIDFromPath(r.TrustDomain, agentPath)
	workloadID := workloadSPIFFEID(r.TrustDomain, hostID)
	name := spireEntryName(hostID)

	if err := r.applyEntry(ctx, name, parentID, workloadID); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("applied the SPIRE entry", "entry", name, "parentID", parentID, "spiffeID", workloadID)
	return ctrl.Result{}, nil
}

// agentIdentity collects the material the agent's SPIFFE path is built from.
// Only the dsxImds and tpm methods read the infrastructure machine, and both
// read a status field the infrastructure provider fills in after the host
// boots, so an absent one means wait.
func (r *SpireEntryReconciler) agentIdentity(
	ctx context.Context,
	machine *clusterv1.Machine,
	hostID string,
	method bootstrapv1.AttestationMethod,
) (agentIdentity, error) {
	identity := agentIdentity{HostID: hostID, DSXIMDSOperatorTenant: r.DSXIMDSOperatorTenant}
	if method == bootstrapv1.AttestationMethodJoinToken {
		return identity, nil
	}

	if !machine.Spec.InfrastructureRef.IsDefined() {
		return identity, fmt.Errorf("the Machine has no infrastructure reference: %w", errAgentIdentityPending)
	}

	// The reference carries no version — Cluster API resolves it from the
	// CRD's contract labels — and this provider deliberately does not import
	// the infrastructure provider's types, so the object is read untyped.
	infra, err := external.GetObjectFromContractVersionedRef(ctx, r.Client, machine.Spec.InfrastructureRef, machine.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return identity, fmt.Errorf("the infrastructure machine is not readable yet: %w", errAgentIdentityPending)
		}
		return identity, fmt.Errorf("getting the infrastructure machine for %s/%s: %w", machine.Namespace, machine.Name, err)
	}

	// A missing field and an empty one are the same thing here: the
	// infrastructure provider has not published it yet.
	identity.TPMEKPubHash, _, err = unstructured.NestedString(infra.Object, "status", "tpmEkPubHash")
	if err != nil {
		return identity, fmt.Errorf("reading status.tpmEkPubHash from %s %s: %w", infra.GetKind(), infra.GetName(), err)
	}
	identity.PrimaryDPUMachineID, _, err = unstructured.NestedString(infra.Object, "status", "primaryDPUMachineID")
	if err != nil {
		return identity, fmt.Errorf("reading status.primaryDPUMachineID from %s %s: %w", infra.GetKind(), infra.GetName(), err)
	}

	return identity, nil
}

// applyEntry server-side applies the ClusterStaticEntry.
//
// The object is untyped so that registering a worker does not make
// spire-controller-manager a dependency of the bootstrap provider for four
// string fields. The shape is the one the NVKE controller applies, minus the
// TTL fields it serialises as "0s" by accident: this entry is a different
// object under a different field manager, so matching that quirk buys nothing.
func (r *SpireEntryReconciler) applyEntry(ctx context.Context, name, parentID, workloadID string) error {
	entry := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"className": r.spireClassName(),
			"parentID":  parentID,
			"spiffeID":  workloadID,
			// Every root-owned process on the host matches. nke-agent runs as
			// root, and SPIRE has no selector that distinguishes it further on
			// a host this provider does not otherwise control.
			"selectors": []any{"unix:uid:0"},
		},
	}}
	entry.SetGroupVersionKind(clusterStaticEntryGVK)
	entry.SetName(name)
	entry.SetLabels(map[string]string{managedByLabel: managedByMachineSpireEntry})

	if err := r.Patch(ctx, entry, client.Apply, client.FieldOwner(spireEntryFieldManager), client.ForceOwnership); err != nil {
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("the %s CRD is not installed, so no worker can attest: %w", clusterStaticEntryKind, err)
		}
		return fmt.Errorf("applying %s %s: %w", clusterStaticEntryKind, name, err)
	}
	return nil
}

func (r *SpireEntryReconciler) deleteEntry(ctx context.Context, hostID string) error {
	entry := &unstructured.Unstructured{}
	entry.SetGroupVersionKind(clusterStaticEntryGVK)
	entry.SetName(spireEntryName(hostID))

	if err := r.Delete(ctx, entry); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return fmt.Errorf("deleting %s %s: %w", clusterStaticEntryKind, entry.GetName(), err)
	}
	return nil
}

func (r *SpireEntryReconciler) spireClassName() string {
	if r.SpireClassName == "" {
		return defaultSpireClassName
	}
	return r.SpireClassName
}

// agentIdentity is the material one agent's SPIFFE path is built from. Which
// fields matter depends on the attestation method.
type agentIdentity struct {
	// HostID identifies the host across the trust domain. See machineHostID.
	HostID string

	// TPMEKPubHash is the SHA-256 of the host TPM's endorsement public key,
	// which the infrastructure provider computes from the EK certificate.
	TPMEKPubHash string

	// PrimaryDPUMachineID is the NICo machine ID of the DPU on the host's
	// primary interface.
	PrimaryDPUMachineID string

	// DSXIMDSOperatorTenant is the tenant the DSX IMDS attestor issues under.
	DSXIMDSOperatorTenant string
}

// machineHostID returns the identifier both halves of the registration are
// keyed on.
//
// The NVKE controller keys its entries on its own database node row's UUID.
// These Machines are owned by Cluster API and are never imported, so that row
// does not exist, and the substitute has to come from the Cluster API objects
// themselves. The owning Machine's UID is the defensible one:
//
//   - It is a UUID, so it drops into the /host/<uuid> workload ID and the
//     /join-token-spire-agent/host-id/<uuid> agent path unchanged. Nothing
//     downstream has to learn a second shape.
//   - It is unique across the whole cluster. A ClusterStaticEntry is
//     cluster-scoped and a trust domain is one flat namespace, so a
//     namespace-scoped key is not enough: the Machine name alone collides
//     between two namespaces, and the two Machines would fight over one entry
//     until one host authenticated as the other.
//   - The API server mints it once and it cannot be rewritten, so the entry
//     cannot drift from the host it registers while the Machine lives. A
//     replaced Machine gets a new UID and therefore a new entry, which is
//     correct: it is a different host.
//
// The alternatives were worse. The NICo instance ID is published late, only
// after the host exists, and would tie this provider to one infrastructure
// provider. The infrastructure machine's UID is one indirection further from
// the object this provider already holds and is no more stable.
func machineHostID(machine *clusterv1.Machine) string {
	return string(machine.UID)
}

// agentSPIFFEIDPath returns the SPIFFE path of the SPIRE agent that attests on
// the host, which is the parent of the workload entry.
//
// The derivation per method matches the NVKE controller's NicoSpiffeController.
// An empty identity field is reported as errAgentIdentityPending rather than
// rendering a truncated path, because a path that parents nothing is
// indistinguishable at the SPIRE server from one that simply has not attested.
func agentSPIFFEIDPath(method bootstrapv1.AttestationMethod, identity agentIdentity) (string, error) {
	switch method {
	case bootstrapv1.AttestationMethodTPM:
		if identity.TPMEKPubHash == "" {
			return "", fmt.Errorf("tpm: the EK public hash is empty: %w", errAgentIdentityPending)
		}
		return tpmAgentPathPrefix + identity.TPMEKPubHash, nil

	case bootstrapv1.AttestationMethodDSXIMDS:
		if identity.DSXIMDSOperatorTenant == "" {
			return "", fmt.Errorf("dsxImds: no operator tenant is configured: %w", errAttestationMethodUnsupported)
		}
		if identity.PrimaryDPUMachineID == "" {
			return "", fmt.Errorf("dsxImds: the primary DPU machine ID is empty: %w", errAgentIdentityPending)
		}
		return dsxIMDSAgentPathPrefix + identity.DSXIMDSOperatorTenant + "/bm/" + identity.PrimaryDPUMachineID, nil

	case bootstrapv1.AttestationMethodJoinToken:
		if identity.HostID == "" {
			return "", fmt.Errorf("joinToken: the host ID is empty: %w", errAgentIdentityPending)
		}
		return joinTokenAgentPathPrefix + identity.HostID, nil

	default:
		return "", fmt.Errorf("%q: %w", method, errAttestationMethodUnsupported)
	}
}

// workloadSPIFFEID returns the ID the entry issues to the workload, which is
// the ID nke-agent presents on its gRPC server and the one this provider's own
// dialer has to authorize. The NVKE controller's second form,
// /tenant/<t>/service/<s>/bm/<id>, is deliberately not produced here: it is
// selected by a node-row flag that these Machines do not have, and an entry in
// the wrong shape exists but is never matched.
func workloadSPIFFEID(trustDomain, hostID string) string {
	return fmt.Sprintf("spiffe://%s/host/%s", trustDomain, hostID)
}

// spiffeIDFromPath joins a trust domain and a path into a SPIFFE ID.
func spiffeIDFromPath(trustDomain, path string) string {
	if len(path) > 0 && path[0] == '/' {
		return fmt.Sprintf("spiffe://%s%s", trustDomain, path)
	}
	return fmt.Sprintf("spiffe://%s/%s", trustDomain, path)
}

// spireEntryName is the ClusterStaticEntry's object name.
func spireEntryName(hostID string) string {
	return spireEntryNamePrefix + hostID
}

// isNvkeBootstrapConfigRef reports whether a Machine's bootstrap reference
// points at this provider.
func isNvkeBootstrapConfigRef(ref clusterv1.ContractVersionedObjectReference) bool {
	return ref.APIGroup == bootstrapv1.GroupName && ref.Kind == "NvkeConfig" && ref.Name != ""
}

// SetupWithManager registers the reconciler.
//
// It watches Machines rather than NvkeConfigs because the Machine is where the
// identity, the infrastructure reference and a deletion window that Cluster
// API already holds open all meet. The predicate keeps every other provider's
// Machines out of the queue.
func (r *SpireEntryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.TrustDomain == "" {
		return errors.New("the SPIRE trust domain is required: every entry this controller writes names it")
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Machine{}, builder.WithPredicates(predicate.NewPredicateFuncs(hasNvkeBootstrapConfig))).
		Named("spireentry").
		Complete(r)
}

func hasNvkeBootstrapConfig(obj client.Object) bool {
	machine, ok := obj.(*clusterv1.Machine)
	if !ok {
		return false
	}
	return isNvkeBootstrapConfigRef(machine.Spec.Bootstrap.ConfigRef)
}
