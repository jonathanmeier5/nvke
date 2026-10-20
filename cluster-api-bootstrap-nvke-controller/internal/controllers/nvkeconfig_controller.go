// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controllers reconciles NvkeConfig objects.
package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/paused"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1 "github.com/dsx-ai-factory/nvke/cluster-api-bootstrap-nvke-controller/api/v1alpha1"
	"github.com/dsx-ai-factory/nvke/cluster-api-bootstrap-nvke-controller/internal/bootstrapdata"
	"github.com/dsx-ai-factory/nvke/cluster-api-bootstrap-nvke-controller/internal/join"
	"github.com/dsx-ai-factory/nvke/cluster-api-bootstrap-nvke-controller/internal/joinmaterial"
)

// joinTimeout bounds one join attempt. It is shorter than the reconcile budget
// so a slow node does not hold the worker; the next reconcile picks it up.
const joinTimeout = 4 * time.Minute

// ReadyCondition is the condition Cluster API mirrors onto the Machine as
// BootstrapConfigReady. The type string is contract, not a free choice.
const ReadyCondition = "Ready"

// NvkeConfigReconciler reconciles an NvkeConfig.
//
// The reconcile has two phases, separated in time by the life of the host:
//
//	Phase 1  render the boot configuration, publish it as a data secret, and
//	         report dataSecretCreated. Until this lands the infrastructure
//	         provider holds the machine at WaitingForBootstrapData, so no host
//	         exists yet.
//
//	Phase 2  once the host is up and nke-agent answers, mint a bootstrap token
//	         against the workload cluster, render the join configuration and
//	         drive the agent through kubeadm join, then apply the post-join
//	         labels, taints and CNI.
//
// Phase 2 is outside the stock bootstrap contract, which ends at the secret.
// It is deliberate: nke-agent is the only sanctioned way NVKE changes a node,
// and a plain cloud-init join would leave the node outside that model.
//
// Only phase 1 is implemented.
type NvkeConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// SpireServerAddress, SpireServerPort, SpireTrustDomain and
	// SpireTrustBundle configure the node's spire-agent. They are required
	// unless DisableMTLS is set: an agent with no SPIRE configuration blocks in
	// NewX509Source and never opens its port.
	SpireServerAddress string
	SpireServerPort    string
	SpireTrustDomain   string
	SpireTrustBundle   string

	// NodeAgentPort is nvke-agent's --secure-server-port. Empty takes the
	// renderer's default, which is the port the controller dials.
	NodeAgentPort string

	// DisableMTLS serves the agent's gRPC in plaintext and leaves SPIRE out of
	// the rendered configuration. Development only; it must match how the rest
	// of the site was built.
	DisableMTLS bool

	// ContainerNodes marks the hosts as nico-sim containers. It keeps
	// NKE_LOCAL_DEV=true and --disable-mlx5-rescan in the agent environment:
	// without the first, kubeadm fails preflight inside a container because
	// /proc/config.gz is unavailable.
	ContainerNodes bool
}

// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=nvkeconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=nvkeconfigs/status,verbs=get;update;patch
// Setting blockOwnerDeletion on the data secret's owner reference requires update on the owner's finalizers.
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=nvkeconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=nvkeconfigtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status;machines;machines/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// Reading <cluster>-kubeconfig and <cluster>-ca is covered by the secrets rule above.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile moves one NvkeConfig towards its desired state.
func (r *NvkeConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	config := &bootstrapv1.NvkeConfig{}
	if err := r.Get(ctx, req.NamespacedName, config); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The Machine controller sets itself as the controller owner once it sees
	// the config, so an unowned config is simply early and not an error.
	machine, err := util.GetOwnerMachine(ctx, r.Client, config.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting owner machine: %w", err)
	}
	if machine == nil {
		logger.V(1).Info("waiting for the Machine controller to set the owner reference")
		return ctrl.Result{}, nil
	}

	cluster, err := util.GetClusterByName(ctx, r.Client, config.Namespace, machine.Spec.ClusterName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting cluster %s: %w", machine.Spec.ClusterName, err)
	}

	// Pause is not propagated onto bootstrap objects by Cluster API, so each
	// provider has to honour it itself.
	if isPaused, requeue, err := paused.EnsurePausedCondition(ctx, r.Client, cluster, config); err != nil || isPaused || requeue {
		return ctrl.Result{}, err
	}

	helper, err := patch.NewHelper(config, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building patch helper: %w", err)
	}
	defer func() {
		if patchErr := helper.Patch(ctx, config); patchErr != nil && err == nil {
			err = patchErr
		}
	}()

	// The secret is written once. The infrastructure provider reads it only on
	// the instance-create path, so rewriting it later would change nothing on a
	// host that already exists. Phase 2 runs from here on every reconcile.
	if config.Status.DataSecretName != "" {
		requeue, joinErr := r.reconcileJoin(ctx, config, machine, cluster)
		return ctrl.Result{RequeueAfter: requeue}, joinErr
	}

	data, err := bootstrapdata.Render(bootstrapdata.Input{
		ClusterName:        cluster.Name,
		MachineName:        machine.Name,
		AttestationMethod:  string(config.Spec.AttestationMethod),
		SpireServerAddress: r.SpireServerAddress,
		SpireServerPort:    r.SpireServerPort,
		SpireTrustDomain:   r.SpireTrustDomain,
		SpireTrustBundle:   r.SpireTrustBundle,
		NodeAgentPort:      r.NodeAgentPort,
		NodeAgentExtraArgs: nil,
		DisableMTLS:        r.DisableMTLS,
		ContainerNode:      r.ContainerNodes,
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("rendering the boot configuration: %w", err)
	}

	if err := r.storeBootstrapData(ctx, config, cluster.Name, []byte(data)); err != nil {
		return ctrl.Result{}, fmt.Errorf("storing the bootstrap data: %w", err)
	}

	config.Status.DataSecretName = config.Name
	config.Status.Initialization.DataSecretCreated = ptr.To(true)
	setReady(config, metav1.ConditionTrue, "DataSecretCreated", "The boot configuration has been published")

	logger.Info("published the boot configuration", "secret", config.Name, "machine", machine.Name)
	return ctrl.Result{}, nil
}

// storeBootstrapData writes the payload to a secret shaped the way Cluster API
// and the infrastructure providers expect it.
//
// The infrastructure provider reads data["value"] and nothing else; an absent
// or empty key stalls the machine. The owner is the NvkeConfig rather than the
// Machine, which is what the kubeadm provider does, so the secret is collected
// with the config it belongs to.
func (r *NvkeConfigReconciler) storeBootstrapData(ctx context.Context, config *bootstrapv1.NvkeConfig, clusterName string, data []byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.Name,
			Namespace: config.Namespace,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Data: map[string][]byte{"value": data},
		Type: clusterv1.ClusterSecretType,
	}
	if err := controllerutil.SetControllerReference(config, secret, r.Scheme); err != nil {
		return fmt.Errorf("setting the owner reference: %w", err)
	}

	if err := r.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating secret %s/%s: %w", secret.Namespace, secret.Name, err)
		}
		if err := r.Update(ctx, secret); err != nil {
			return fmt.Errorf("updating secret %s/%s: %w", secret.Namespace, secret.Name, err)
		}
	}
	return nil
}

func setReady(config *bootstrapv1.NvkeConfig, status metav1.ConditionStatus, reason, message string) {
	for i := range config.Status.Conditions {
		if config.Status.Conditions[i].Type == ReadyCondition {
			config.Status.Conditions[i].Status = status
			config.Status.Conditions[i].Reason = reason
			config.Status.Conditions[i].Message = message
			return
		}
	}
	config.Status.Conditions = append(config.Status.Conditions, metav1.Condition{
		Type:    ReadyCondition,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
}

// SetupWithManager registers the reconciler.
func (r *NvkeConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&bootstrapv1.NvkeConfig{}).
		Named("nvkeconfig").
		Complete(r)
}

// JoinedCondition records that the node was driven through kubeadm join.
//
// The agent's own status is not enough on its own: it does not reload its state
// file at startup, so an agent restarted mid-join reports uninitialized and a
// retry would run the join a second time. This condition is the durable bit.
const JoinedCondition = "Joined"

// reconcileJoin is phase 2: drive the node through kubeadm join once its host
// exists and nvke-agent answers.
//
// Returns a requeue delay when it is simply early — no address yet, or the
// agent is not listening. Those are the normal states between the
// infrastructure provider creating a host and that host finishing boot.
func (r *NvkeConfigReconciler) reconcileJoin(
	ctx context.Context,
	config *bootstrapv1.NvkeConfig,
	machine *clusterv1.Machine,
	cluster *clusterv1.Cluster,
) (time.Duration, error) {
	logger := log.FromContext(ctx)

	if conditionTrue(config, JoinedCondition) {
		return 0, nil
	}

	address := internalIP(machine)
	if address == "" {
		logger.V(1).Info("waiting for the infrastructure provider to report an address")
		return 20 * time.Second, nil
	}

	client, err := join.Dial(ctx, join.DialConfig{
		Address: address,
		Port:    r.NodeAgentPort,
		Mode:    r.transportMode(),
	})
	if err != nil {
		// The host exists but its agent has not opened its port yet. That is
		// ordinary for a node still booting, so it is a wait rather than a
		// failure.
		logger.V(1).Info("the node agent is not reachable yet", "address", address, "err", err.Error())
		return 20 * time.Second, nil
	}
	defer func() { _ = client.Close() }()

	joiner := &join.Joiner{Client: client, Timeout: joinTimeout}

	// Ask the agent before issuing anything: a join that already succeeded must
	// never be issued twice, and this is the only way to see a join this
	// provider did not itself record.
	done, err := joiner.Completed(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading the agent's kubeadm status: %w", err)
	}
	if done {
		setCondition(config, JoinedCondition, metav1.ConditionTrue, "AlreadyJoined", "The node had already joined")
		return 0, nil
	}

	material, joinConfig, err := r.buildJoinMaterial(ctx, machine, cluster, address)
	if err != nil {
		return 0, err
	}

	logger.Info("joining the node", "machine", machine.Name, "address", address)
	if _, err := joiner.Join(ctx, join.Request{Material: material, Configuration: joinConfig}); err != nil {
		setCondition(config, JoinedCondition, metav1.ConditionFalse, "JoinFailed", err.Error())
		return 0, fmt.Errorf("joining %s: %w", machine.Name, err)
	}

	setCondition(config, JoinedCondition, metav1.ConditionTrue, "Joined", "The node joined the cluster")
	logger.Info("the node joined", "machine", machine.Name)
	return 0, nil
}

// buildJoinMaterial mints a bootstrap token against the workload cluster and
// renders the JoinConfiguration that carries it.
//
// The token comes from the cluster's own API rather than from a control-plane
// node's agent, so the join does not depend on any particular node being
// reachable.
func (r *NvkeConfigReconciler) buildJoinMaterial(
	ctx context.Context,
	machine *clusterv1.Machine,
	cluster *clusterv1.Cluster,
	address string,
) (join.Material, string, error) {
	workload, err := remote.NewClusterClient(ctx, "nvke-bootstrap", r.Client, client.ObjectKeyFromObject(cluster))
	if err != nil {
		return join.Material{}, "", fmt.Errorf("connecting to the workload cluster: %w", err)
	}

	token, err := joinmaterial.MintToken(ctx, workload)
	if err != nil {
		return join.Material{}, "", fmt.Errorf("minting a bootstrap token: %w", err)
	}

	caSecret := &corev1.Secret{}
	caKey := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name + "-ca"}
	if err := r.Get(ctx, caKey, caSecret); err != nil {
		return join.Material{}, "", fmt.Errorf("reading the cluster CA %s: %w", caKey, err)
	}
	hash, err := joinmaterial.CACertHash(caSecret.Data["tls.crt"])
	if err != nil {
		return join.Material{}, "", fmt.Errorf("hashing the cluster CA: %w", err)
	}

	endpoint := fmt.Sprintf("%s:%d", cluster.Spec.ControlPlaneEndpoint.Host, cluster.Spec.ControlPlaneEndpoint.Port)
	material := join.Material{
		APIServerEndpoint: endpoint,
		Token:             token.String(),
		CACertHashes:      []string{hash},
	}

	joinConfig, err := joinmaterial.RenderJoinConfiguration(joinmaterial.JoinConfigInput{
		APIServerEndpoint: endpoint,
		Token:             token.String(),
		CACertHashes:      []string{hash},
		NodeName:          machine.Name,
		NodeIP:            address,
	})
	if err != nil {
		return join.Material{}, "", fmt.Errorf("rendering the join configuration: %w", err)
	}
	return material, joinConfig, nil
}

func (r *NvkeConfigReconciler) transportMode() join.TransportMode {
	if r.DisableMTLS {
		return join.TransportPlaintext
	}
	return join.TransportSPIFFE
}

// internalIP returns the address the agent listens on, or empty when the
// infrastructure provider has not reported one yet.
func internalIP(machine *clusterv1.Machine) string {
	for _, addr := range machine.Status.Addresses {
		if addr.Type == clusterv1.MachineInternalIP {
			return addr.Address
		}
	}
	return ""
}

func conditionTrue(config *bootstrapv1.NvkeConfig, conditionType string) bool {
	for _, c := range config.Status.Conditions {
		if c.Type == conditionType {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

func setCondition(config *bootstrapv1.NvkeConfig, conditionType string, status metav1.ConditionStatus, reason, message string) {
	for i := range config.Status.Conditions {
		if config.Status.Conditions[i].Type == conditionType {
			config.Status.Conditions[i].Status = status
			config.Status.Conditions[i].Reason = reason
			config.Status.Conditions[i].Message = message
			return
		}
	}
	config.Status.Conditions = append(config.Status.Conditions, metav1.Condition{
		Type: conditionType, Status: status, Reason: reason, Message: message,
	})
}
