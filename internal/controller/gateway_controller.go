package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
	"github.com/varnish/gateway/internal/status"
	"github.com/varnish/gateway/internal/vcl"
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
	ImagePullSecrets string // Comma-separated list of image pull secret names
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
	log.Debug("reconciling Gateway")

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
		// Update status to reflect error
		if statusErr := r.updateGatewayStatus(ctx, &gateway, false, err.Error()); statusErr != nil {
			log.Error("failed to update status", "error", statusErr)
		}
		return ctrl.Result{}, err
	}

	// 6. Update status to Accepted/Programmed
	// Use Server-Side Apply for status update - no conflicts with other controllers
	if err := r.updateGatewayStatus(ctx, &gateway, true, ""); err != nil {
		return ctrl.Result{}, fmt.Errorf("r.updateGatewayStatus: %w", err)
	}

	log.Debug("gateway reconciliation complete")
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
	// Fetch GatewayClassParameters for extra args and logging config
	var varnishdExtraArgs []string
	var logging *gatewayparamsv1alpha1.VarnishLogging
	if params := r.getGatewayClassParameters(ctx, gateway); params != nil {
		varnishdExtraArgs = params.Spec.VarnishdExtraArgs
		logging = params.Spec.Logging
	}

	// Generate VCL content (ghost preamble + user VCL)
	vclContent := r.generateVCL(ctx, gateway)

	// Parse image pull secrets
	imagePullSecrets := r.parseImagePullSecrets()

	// Compute infrastructure hash for pod restart detection
	infraConfig := InfrastructureConfig{
		GatewayImage:      r.Config.GatewayImage,
		VarnishdExtraArgs: varnishdExtraArgs,
		Logging:           logging,
		ImagePullSecrets:  imagePullSecrets,
	}
	infraHash := infraConfig.ComputeHash()

	// Create resources in order (some depend on others existing)
	// ConfigMap must be created first so HTTPRoute controller can process routes immediately
	resources := []client.Object{
		r.buildVCLConfigMap(gateway, vclContent),
		r.buildAdminSecret(gateway),
		r.buildServiceAccount(gateway),
		r.buildClusterRoleBinding(gateway),
		r.buildDeployment(gateway, varnishdExtraArgs, logging, infraHash),
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
		// Get GVK from scheme since TypeMeta is not populated on typed objects
		gvks, _, _ := r.Scheme.ObjectKinds(desired)
		kind := ""
		if len(gvks) > 0 {
			kind = gvks[0].Kind
		}
		r.Logger.Info("created resource",
			"kind", kind,
			"name", desired.GetName())
		return nil
	}
	if err != nil {
		return fmt.Errorf("r.Get(%s): %w", desired.GetName(), err)
	}

	// For ConfigMaps, update only main.vcl, preserve routing.json (owned by HTTPRoute controller)
	if desiredCM, ok := desired.(*corev1.ConfigMap); ok {
		existingCM := existing.(*corev1.ConfigMap)
		// Check if main.vcl changed
		if existingCM.Data["main.vcl"] != desiredCM.Data["main.vcl"] {
			// Update only main.vcl, keep routing.json from existing
			existingCM.Data["main.vcl"] = desiredCM.Data["main.vcl"]
			if err := r.Update(ctx, existingCM); err != nil {
				return fmt.Errorf("r.Update(%s): %w", desired.GetName(), err)
			}
			r.Logger.Info("updated configmap main.vcl",
				"name", desired.GetName())
		}
		return nil
	}

	// For Deployments, check if image needs updating (supports rolling updates)
	if desiredDeploy, ok := desired.(*appsv1.Deployment); ok {
		existingDeploy := existing.(*appsv1.Deployment)
		if needsDeploymentUpdate(existingDeploy, desiredDeploy) {
			// Update the pod template spec to trigger a rolling update
			existingDeploy.Spec.Template = desiredDeploy.Spec.Template
			existingDeploy.Spec.Strategy = desiredDeploy.Spec.Strategy
			if err := r.Update(ctx, existingDeploy); err != nil {
				return fmt.Errorf("r.Update(%s): %w", desired.GetName(), err)
			}
			r.Logger.Info("updated deployment",
				"name", desired.GetName(),
				"image", desiredDeploy.Spec.Template.Spec.Containers[0].Image)
			return nil
		}
	}

	return nil
}

// needsDeploymentUpdate checks if the Deployment needs to be updated.
func needsDeploymentUpdate(existing, desired *appsv1.Deployment) bool {
	if len(existing.Spec.Template.Spec.Containers) == 0 ||
		len(desired.Spec.Template.Spec.Containers) == 0 {
		return false
	}

	// Check if image changed
	if existing.Spec.Template.Spec.Containers[0].Image !=
		desired.Spec.Template.Spec.Containers[0].Image {
		return true
	}

	// Check if infrastructure hash changed (triggers pod restart)
	existingHash := ""
	desiredHash := ""
	if existing.Spec.Template.Annotations != nil {
		existingHash = existing.Spec.Template.Annotations[AnnotationInfraHash]
	}
	if desired.Spec.Template.Annotations != nil {
		desiredHash = desired.Spec.Template.Annotations[AnnotationInfraHash]
	}

	return existingHash != desiredHash
}

// updateGatewayStatus updates Gateway status using Server-Side Apply.
// Creates a minimal patch object to avoid conflicts with HTTPRoute controller.
func (r *GatewayReconciler) updateGatewayStatus(ctx context.Context, gateway *gatewayv1.Gateway, success bool, errMsg string) error {
	// Create minimal Gateway object for SSA patch - only include fields we own
	patch := &gatewayv1.Gateway{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gatewayv1.GroupVersion.String(),
			Kind:       "Gateway",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      gateway.Name,
			Namespace: gateway.Namespace,
		},
	}

	// Set gateway-level conditions
	if success {
		status.SetGatewayAccepted(patch, true,
			string(gatewayv1.GatewayReasonAccepted),
			"Gateway accepted by controller")
		status.SetGatewayProgrammed(patch, true,
			string(gatewayv1.GatewayReasonProgrammed),
			"Gateway configuration programmed")
	} else {
		status.SetGatewayAccepted(patch, false,
			string(gatewayv1.GatewayReasonInvalid),
			errMsg)
		status.SetGatewayProgrammed(patch, false,
			string(gatewayv1.GatewayReasonInvalid),
			errMsg)
	}

	// Set listener statuses (conditions and SupportedKinds only, not AttachedRoutes)
	r.setListenerStatusesForPatch(patch, gateway)

	// Apply the patch
	if err := r.Status().Patch(ctx, patch, client.Apply,
		client.FieldOwner("varnish-gateway-controller"),
		client.ForceOwnership); err != nil {
		return fmt.Errorf("r.Status().Patch: %w", err)
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

// setListenerStatusesForPatch sets listener statuses for SSA patch.
// Only sets fields owned by Gateway controller (conditions, SupportedKinds).
// Does NOT set AttachedRoutes (owned by HTTPRoute controller).
func (r *GatewayReconciler) setListenerStatusesForPatch(patch *gatewayv1.Gateway, original *gatewayv1.Gateway) {
	// Build map of existing listener statuses to preserve condition times
	existingStatuses := make(map[gatewayv1.SectionName]gatewayv1.ListenerStatus)
	for _, ls := range original.Status.Listeners {
		existingStatuses[ls.Name] = ls
	}

	patch.Status.Listeners = make([]gatewayv1.ListenerStatus, len(original.Spec.Listeners))

	for i, listener := range original.Spec.Listeners {
		existing, hasExisting := existingStatuses[listener.Name]

		// Preserve existing condition times if status unchanged
		acceptedTime := metav1.Now()
		programmedTime := metav1.Now()
		if hasExisting {
			for _, c := range existing.Conditions {
				if c.Type == string(gatewayv1.ListenerConditionAccepted) && c.Status == metav1.ConditionTrue {
					acceptedTime = c.LastTransitionTime
				}
				if c.Type == string(gatewayv1.ListenerConditionProgrammed) && c.Status == metav1.ConditionTrue {
					programmedTime = c.LastTransitionTime
				}
			}
		}

		patch.Status.Listeners[i] = gatewayv1.ListenerStatus{
			Name: listener.Name,
			SupportedKinds: []gatewayv1.RouteGroupKind{
				{
					Group: ptr(gatewayv1.Group("gateway.networking.k8s.io")),
					Kind:  "HTTPRoute",
				},
			},
			// DO NOT set AttachedRoutes - that's owned by HTTPRoute controller
			Conditions: []metav1.Condition{
				{
					Type:               string(gatewayv1.ListenerConditionAccepted),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: original.Generation,
					LastTransitionTime: acceptedTime,
					Reason:             string(gatewayv1.ListenerReasonAccepted),
					Message:            "Listener accepted",
				},
				{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: original.Generation,
					LastTransitionTime: programmedTime,
					Reason:             string(gatewayv1.ListenerReasonProgrammed),
					Message:            "Listener programmed",
				},
			},
		}
	}
}

// setListenerStatuses updates status for each Gateway listener.
func (r *GatewayReconciler) setListenerStatuses(gateway *gatewayv1.Gateway) {
	// Build map of existing listener statuses to preserve AttachedRoutes and condition times
	existingStatuses := make(map[gatewayv1.SectionName]gatewayv1.ListenerStatus)
	for _, ls := range gateway.Status.Listeners {
		existingStatuses[ls.Name] = ls
	}

	gateway.Status.Listeners = make([]gatewayv1.ListenerStatus, len(gateway.Spec.Listeners))

	for i, listener := range gateway.Spec.Listeners {
		existing, hasExisting := existingStatuses[listener.Name]

		// Preserve existing AttachedRoutes count (set by HTTPRoute controller)
		attachedRoutes := int32(0)
		if hasExisting {
			attachedRoutes = existing.AttachedRoutes
		}

		// Preserve existing condition times if status unchanged
		acceptedTime := metav1.Now()
		programmedTime := metav1.Now()
		if hasExisting {
			for _, c := range existing.Conditions {
				if c.Type == string(gatewayv1.ListenerConditionAccepted) && c.Status == metav1.ConditionTrue {
					acceptedTime = c.LastTransitionTime
				}
				if c.Type == string(gatewayv1.ListenerConditionProgrammed) && c.Status == metav1.ConditionTrue {
					programmedTime = c.LastTransitionTime
				}
			}
		}

		gateway.Status.Listeners[i] = gatewayv1.ListenerStatus{
			Name: listener.Name,
			SupportedKinds: []gatewayv1.RouteGroupKind{
				{
					Group: ptr(gatewayv1.Group("gateway.networking.k8s.io")),
					Kind:  "HTTPRoute",
				},
			},
			AttachedRoutes: attachedRoutes,
			Conditions: []metav1.Condition{
				{
					Type:               string(gatewayv1.ListenerConditionAccepted),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: acceptedTime,
					Reason:             string(gatewayv1.ListenerReasonAccepted),
					Message:            "Listener accepted",
				},
				{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: programmedTime,
					Reason:             string(gatewayv1.ListenerReasonProgrammed),
					Message:            "Listener programmed",
				},
			},
		}
	}
}

// getGatewayClassParameters fetches GatewayClassParameters for the given Gateway.
// Returns nil if not found or if ParametersRef is not set.
func (r *GatewayReconciler) getGatewayClassParameters(ctx context.Context, gateway *gatewayv1.Gateway) *gatewayparamsv1alpha1.GatewayClassParameters {
	// 1. Get GatewayClass
	var gatewayClass gatewayv1.GatewayClass
	if err := r.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, &gatewayClass); err != nil {
		if !apierrors.IsNotFound(err) {
			r.Logger.Error("failed to get GatewayClass", "error", err)
		}
		return nil
	}

	// 2. Check if ParametersRef is set
	if gatewayClass.Spec.ParametersRef == nil {
		return nil
	}

	// 3. Validate ParametersRef points to our CRD
	ref := gatewayClass.Spec.ParametersRef
	if string(ref.Group) != gatewayparamsv1alpha1.GroupName ||
		string(ref.Kind) != "GatewayClassParameters" {
		return nil // Not our parameters type
	}

	// 4. Fetch GatewayClassParameters
	var params gatewayparamsv1alpha1.GatewayClassParameters
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, &params); err != nil {
		if !apierrors.IsNotFound(err) {
			r.Logger.Error("failed to get GatewayClassParameters",
				"name", ref.Name, "error", err)
		}
		return nil
	}

	return &params
}

// getUserVCL returns user-provided VCL from GatewayClassParameters.
// It traverses: Gateway -> GatewayClass -> GatewayClassParameters -> ConfigMap
func (r *GatewayReconciler) getUserVCL(ctx context.Context, gateway *gatewayv1.Gateway) string {
	// 1. Get GatewayClass
	var gatewayClass gatewayv1.GatewayClass
	if err := r.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, &gatewayClass); err != nil {
		if !apierrors.IsNotFound(err) {
			r.Logger.Error("failed to get GatewayClass", "error", err)
		}
		return ""
	}

	// 2. Check if ParametersRef is set
	if gatewayClass.Spec.ParametersRef == nil {
		return ""
	}

	// 3. Validate ParametersRef points to our CRD
	ref := gatewayClass.Spec.ParametersRef
	if string(ref.Group) != gatewayparamsv1alpha1.GroupName ||
		string(ref.Kind) != "GatewayClassParameters" {
		return "" // Not our parameters type
	}

	// 4. Fetch GatewayClassParameters
	var params gatewayparamsv1alpha1.GatewayClassParameters
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, &params); err != nil {
		if !apierrors.IsNotFound(err) {
			r.Logger.Error("failed to get GatewayClassParameters",
				"name", ref.Name, "error", err)
		}
		return ""
	}

	// 5. If UserVCLConfigMapRef is not set, return empty
	if params.Spec.UserVCLConfigMapRef == nil {
		return ""
	}

	// 6. Fetch the ConfigMap containing user VCL
	cmRef := params.Spec.UserVCLConfigMapRef
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{
		Name:      cmRef.Name,
		Namespace: cmRef.Namespace,
	}, &cm); err != nil {
		r.Logger.Error("failed to get user VCL ConfigMap",
			"namespace", cmRef.Namespace, "name", cmRef.Name, "error", err)
		return ""
	}

	// 7. Return VCL from ConfigMap (default key is "user.vcl")
	key := cmRef.Key
	if key == "" {
		key = "user.vcl"
	}

	userVCL, ok := cm.Data[key]
	if !ok {
		r.Logger.Warn("user VCL ConfigMap missing expected key",
			"namespace", cmRef.Namespace, "name", cmRef.Name, "key", key)
		return ""
	}

	r.Logger.Debug("loaded user VCL from ConfigMap",
		"namespace", cmRef.Namespace, "name", cmRef.Name, "key", key)

	return userVCL
}

// generateVCL generates ghost preamble VCL and merges it with user VCL
func (r *GatewayReconciler) generateVCL(ctx context.Context, gateway *gatewayv1.Gateway) string {
	// Generate ghost preamble VCL
	generatedVCL := vcl.Generate(nil, vcl.GeneratorConfig{})

	// Get user VCL if configured
	userVCL := r.getUserVCL(ctx, gateway)

	// Merge generated and user VCL
	return vcl.Merge(generatedVCL, userVCL)
}

// parseImagePullSecrets parses the comma-separated ImagePullSecrets config
// and returns a slice of secret names for use in infrastructure hash computation
func (r *GatewayReconciler) parseImagePullSecrets() []string {
	if r.Config.ImagePullSecrets == "" {
		return nil
	}

	var secrets []string
	for _, name := range strings.Split(r.Config.ImagePullSecrets, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			secrets = append(secrets, name)
		}
	}
	return secrets
}

// buildLabels returns labels for resources owned by a Gateway.
func (r *GatewayReconciler) buildLabels(gateway *gatewayv1.Gateway) map[string]string {
	return map[string]string{
		LabelManagedBy:        ManagedByValue,
		LabelGatewayName:      gateway.Name,
		LabelGatewayNamespace: gateway.Namespace,
	}
}

// enqueueGatewaysForParams returns an EventHandler that enqueues all Gateways
// that use a GatewayClass referencing the changed GatewayClassParameters.
func (r *GatewayReconciler) enqueueGatewaysForParams() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		params, ok := obj.(*gatewayparamsv1alpha1.GatewayClassParameters)
		if !ok {
			return nil
		}

		// Find all GatewayClasses that reference this GatewayClassParameters
		var gatewayClasses gatewayv1.GatewayClassList
		if err := r.List(ctx, &gatewayClasses); err != nil {
			r.Logger.Error("failed to list GatewayClasses", "error", err)
			return nil
		}

		var requests []ctrl.Request
		for _, gc := range gatewayClasses.Items {
			// Check if this GatewayClass references our params
			if gc.Spec.ParametersRef == nil {
				continue
			}
			ref := gc.Spec.ParametersRef
			if string(ref.Group) != gatewayparamsv1alpha1.GroupName ||
				string(ref.Kind) != "GatewayClassParameters" ||
				ref.Name != params.Name {
				continue
			}

			// Find all Gateways using this GatewayClass
			var gateways gatewayv1.GatewayList
			if err := r.List(ctx, &gateways); err != nil {
				r.Logger.Error("failed to list Gateways", "error", err)
				continue
			}

			for _, gw := range gateways.Items {
				if string(gw.Spec.GatewayClassName) == gc.Name {
					requests = append(requests, ctrl.Request{
						NamespacedName: types.NamespacedName{
							Name:      gw.Name,
							Namespace: gw.Namespace,
						},
					})
				}
			}
		}

		if len(requests) > 0 {
			r.Logger.Info("GatewayClassParameters changed, enqueuing Gateways for reconciliation",
				"params", params.Name, "gateways", len(requests))
		}

		return requests
	})
}

// enqueueGatewaysForConfigMap returns an EventHandler that enqueues all Gateways
// that use a user VCL ConfigMap (via GatewayClass -> GatewayClassParameters).
// Lookup chain: ConfigMap -> GatewayClassParameters -> GatewayClass -> Gateway
func (r *GatewayReconciler) enqueueGatewaysForConfigMap() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		cm, ok := obj.(*corev1.ConfigMap)
		if !ok {
			return nil
		}

		// Find all GatewayClassParameters that reference this ConfigMap
		var paramsList gatewayparamsv1alpha1.GatewayClassParametersList
		if err := r.List(ctx, &paramsList); err != nil {
			r.Logger.Error("failed to list GatewayClassParameters", "error", err)
			return nil
		}

		var requests []ctrl.Request
		for _, params := range paramsList.Items {
			// Check if this params references our ConfigMap
			if params.Spec.UserVCLConfigMapRef == nil {
				continue
			}
			cmRef := params.Spec.UserVCLConfigMapRef
			if cmRef.Name != cm.Name || cmRef.Namespace != cm.Namespace {
				continue
			}

			// Find all GatewayClasses that reference this GatewayClassParameters
			var gatewayClasses gatewayv1.GatewayClassList
			if err := r.List(ctx, &gatewayClasses); err != nil {
				r.Logger.Error("failed to list GatewayClasses", "error", err)
				continue
			}

			for _, gc := range gatewayClasses.Items {
				// Check if this GatewayClass references our params
				if gc.Spec.ParametersRef == nil {
					continue
				}
				ref := gc.Spec.ParametersRef
				if string(ref.Group) != gatewayparamsv1alpha1.GroupName ||
					string(ref.Kind) != "GatewayClassParameters" ||
					ref.Name != params.Name {
					continue
				}

				// Find all Gateways using this GatewayClass
				var gateways gatewayv1.GatewayList
				if err := r.List(ctx, &gateways); err != nil {
					r.Logger.Error("failed to list Gateways", "error", err)
					continue
				}

				for _, gw := range gateways.Items {
					if string(gw.Spec.GatewayClassName) == gc.Name {
						requests = append(requests, ctrl.Request{
							NamespacedName: types.NamespacedName{
								Name:      gw.Name,
								Namespace: gw.Namespace,
							},
						})
					}
				}
			}
		}

		if len(requests) > 0 {
			r.Logger.Info("user VCL ConfigMap changed, enqueuing Gateways for reconciliation",
				"configmap", fmt.Sprintf("%s/%s", cm.Namespace, cm.Name), "gateways", len(requests))
		}

		return requests
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.Gateway{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.ClusterRoleBinding{}).
		Watches(
			&gatewayparamsv1alpha1.GatewayClassParameters{},
			r.enqueueGatewaysForParams(),
		).
		Watches(
			&corev1.ConfigMap{},
			r.enqueueGatewaysForConfigMap(),
		).
		Complete(r)
}

// ptr returns a pointer to the given value.
func ptr[T any](v T) *T {
	return &v
}
