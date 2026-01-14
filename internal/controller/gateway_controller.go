package controller

import (
	"context"
	"fmt"
	"log/slog"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/varnish/gateway/internal/status"
)

const (
	// ControllerName is the name of this controller for GatewayClass matching.
	ControllerName = "varnish-software.com/gateway"

	// FinalizerName is added to Gateways managed by this controller.
	FinalizerName = "gateway.varnish-software.com/finalizer"

	// LabelManagedBy identifies resources created by this operator.
	LabelManagedBy = "app.kubernetes.io/managed-by"

	// LabelGatewayName identifies the Gateway that owns this resource.
	LabelGatewayName = "gateway.networking.k8s.io/gateway-name"

	// LabelGatewayNamespace identifies the namespace of the owning Gateway.
	LabelGatewayNamespace = "gateway.networking.k8s.io/gateway-namespace"

	// ManagedByValue is the value for the managed-by label.
	ManagedByValue = "varnish-gateway-operator"
)

// Config holds controller configuration from environment.
type Config struct {
	GatewayClassName string // Which GatewayClass this operator manages
	GatewayImage     string // Combined varnish+ghost+chaperone image
}

// GatewayReconciler reconciles Gateway objects.
type GatewayReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config Config
	Logger *slog.Logger
}

// Reconcile handles Gateway reconciliation.
func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Logger.With("gateway", req.NamespacedName)
	log.Info("reconciling Gateway")

	// 1. Fetch the Gateway
	var gateway gatewayv1.Gateway
	if err := r.Get(ctx, req.NamespacedName, &gateway); err != nil {
		if apierrors.IsNotFound(err) {
			// Gateway deleted, nothing to do (owned resources cleaned up by GC)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("r.Get(%s): %w", req.NamespacedName, err)
	}

	// 2. Check if this Gateway uses our GatewayClass
	if string(gateway.Spec.GatewayClassName) != r.Config.GatewayClassName {
		log.Debug("gateway uses different GatewayClass, skipping",
			"gatewayClass", gateway.Spec.GatewayClassName,
			"expected", r.Config.GatewayClassName)
		return ctrl.Result{}, nil
	}

	// 3. Handle deletion
	if !gateway.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &gateway)
	}

	// 4. Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&gateway, FinalizerName) {
		controllerutil.AddFinalizer(&gateway, FinalizerName)
		if err := r.Update(ctx, &gateway); err != nil {
			return ctrl.Result{}, fmt.Errorf("r.Update (add finalizer): %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 5. Reconcile child resources
	if err := r.reconcileResources(ctx, &gateway); err != nil {
		r.setConditions(&gateway, false, err.Error())
		if statusErr := r.Status().Update(ctx, &gateway); statusErr != nil {
			log.Error("failed to update status", "error", statusErr)
		}
		return ctrl.Result{}, err
	}

	// 6. Update status to Accepted/Programmed
	r.setConditions(&gateway, true, "")
	if err := r.Status().Update(ctx, &gateway); err != nil {
		return ctrl.Result{}, fmt.Errorf("r.Status().Update: %w", err)
	}

	log.Info("gateway reconciliation complete")
	return ctrl.Result{}, nil
}

// reconcileDelete handles Gateway deletion.
func (r *GatewayReconciler) reconcileDelete(ctx context.Context, gateway *gatewayv1.Gateway) (ctrl.Result, error) {
	log := r.Logger.With("gateway", types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace})
	log.Info("handling gateway deletion")

	// Remove finalizer to allow deletion
	if controllerutil.ContainsFinalizer(gateway, FinalizerName) {
		controllerutil.RemoveFinalizer(gateway, FinalizerName)
		if err := r.Update(ctx, gateway); err != nil {
			return ctrl.Result{}, fmt.Errorf("r.Update (remove finalizer): %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// reconcileResources creates or updates all child resources for a Gateway.
func (r *GatewayReconciler) reconcileResources(ctx context.Context, gateway *gatewayv1.Gateway) error {
	// Create resources in order (some depend on others existing)
	resources := []client.Object{
		r.buildAdminSecret(gateway),
		r.buildServiceAccount(gateway),
		r.buildVCLConfigMap(gateway),
		r.buildDeployment(gateway),
		r.buildService(gateway),
	}

	for _, desired := range resources {
		if err := r.reconcileResource(ctx, gateway, desired); err != nil {
			return err
		}
	}

	return nil
}

// reconcileResource creates or updates a single child resource.
func (r *GatewayReconciler) reconcileResource(ctx context.Context, gateway *gatewayv1.Gateway, desired client.Object) error {
	// Set owner reference
	if err := controllerutil.SetControllerReference(gateway, desired, r.Scheme); err != nil {
		return fmt.Errorf("controllerutil.SetControllerReference: %w", err)
	}

	// Check if resource exists
	existing := desired.DeepCopyObject().(client.Object)
	err := r.Get(ctx, types.NamespacedName{
		Name:      desired.GetName(),
		Namespace: desired.GetNamespace(),
	}, existing)

	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("r.Create(%s): %w", desired.GetName(), err)
		}
		r.Logger.Info("created resource",
			"kind", desired.GetObjectKind().GroupVersionKind().Kind,
			"name", desired.GetName())
		return nil
	}
	if err != nil {
		return fmt.Errorf("r.Get(%s): %w", desired.GetName(), err)
	}

	// Resource exists - update if needed
	// For simplicity, we update all resources every reconcile
	// A more sophisticated approach would compare specs
	desired.SetResourceVersion(existing.GetResourceVersion())
	if err := r.Update(ctx, desired); err != nil {
		return fmt.Errorf("r.Update(%s): %w", desired.GetName(), err)
	}

	return nil
}

// setConditions updates Gateway status conditions.
func (r *GatewayReconciler) setConditions(gateway *gatewayv1.Gateway, success bool, errMsg string) {
	if success {
		status.SetGatewayAccepted(gateway, true,
			string(gatewayv1.GatewayReasonAccepted),
			"Gateway accepted by controller")
		status.SetGatewayProgrammed(gateway, true,
			string(gatewayv1.GatewayReasonProgrammed),
			"Gateway configuration programmed")
	} else {
		status.SetGatewayAccepted(gateway, false,
			string(gatewayv1.GatewayReasonInvalid),
			errMsg)
		status.SetGatewayProgrammed(gateway, false,
			string(gatewayv1.GatewayReasonInvalid),
			errMsg)
	}

	// Set listener statuses
	r.setListenerStatuses(gateway)
}

// setListenerStatuses updates status for each Gateway listener.
func (r *GatewayReconciler) setListenerStatuses(gateway *gatewayv1.Gateway) {
	gateway.Status.Listeners = make([]gatewayv1.ListenerStatus, len(gateway.Spec.Listeners))

	for i, listener := range gateway.Spec.Listeners {
		gateway.Status.Listeners[i] = gatewayv1.ListenerStatus{
			Name: listener.Name,
			SupportedKinds: []gatewayv1.RouteGroupKind{
				{
					Group: ptr(gatewayv1.Group("gateway.networking.k8s.io")),
					Kind:  "HTTPRoute",
				},
			},
			AttachedRoutes: 0, // Updated by HTTPRouteController
			Conditions: []metav1.Condition{
				{
					Type:               string(gatewayv1.ListenerConditionAccepted),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             string(gatewayv1.ListenerReasonAccepted),
					Message:            "Listener accepted",
				},
				{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             string(gatewayv1.ListenerReasonProgrammed),
					Message:            "Listener programmed",
				},
			},
		}
	}
}

// buildLabels returns labels for resources owned by a Gateway.
func (r *GatewayReconciler) buildLabels(gateway *gatewayv1.Gateway) map[string]string {
	return map[string]string{
		LabelManagedBy:        ManagedByValue,
		LabelGatewayName:      gateway.Name,
		LabelGatewayNamespace: gateway.Namespace,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.Gateway{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ServiceAccount{}).
		Complete(r)
}

// ptr returns a pointer to the given value.
func ptr[T any](v T) *T {
	return &v
}
