package controller

import (
	"context"
	"encoding/pem"
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
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

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
		patch := client.MergeFrom(gateway.DeepCopy())
		controllerutil.AddFinalizer(&gateway, FinalizerName)
		if err := r.Patch(ctx, &gateway, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("r.Patch (add finalizer): %w", err)
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

	// Clean up cluster-scoped resources (not handled by owner references)
	crbName := fmt.Sprintf("%s-%s-chaperone", gateway.Namespace, gateway.Name)
	crb := &rbacv1.ClusterRoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: crbName}, crb)
	if err == nil {
		if err := r.Delete(ctx, crb); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("r.Delete(ClusterRoleBinding): %w", err)
		}
		log.Info("deleted ClusterRoleBinding", "name", crbName)
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("r.Get(ClusterRoleBinding): %w", err)
	}

	// Remove finalizer to allow deletion
	if controllerutil.ContainsFinalizer(gateway, FinalizerName) {
		patch := client.MergeFrom(gateway.DeepCopy())
		controllerutil.RemoveFinalizer(gateway, FinalizerName)
		if err := r.Patch(ctx, gateway, patch); err != nil {
			// If the namespace is being deleted, we can't update the Gateway.
			// That's fine - the resource will be garbage collected with the namespace.
			if apierrors.IsNotFound(err) {
				log.Info("namespace being deleted, skipping finalizer removal")
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("r.Patch (remove finalizer): %w", err)
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

	// Collect TLS certificate data from HTTPS listeners
	tlsCertData := r.collectTLSCertData(ctx, gateway)
	hasTLS := len(tlsCertData) > 0

	// Compute infrastructure hash for pod restart detection
	infraConfig := InfrastructureConfig{
		GatewayImage:      r.Config.GatewayImage,
		VarnishdExtraArgs: varnishdExtraArgs,
		Logging:           logging,
		ImagePullSecrets:  imagePullSecrets,
		HasTLS:            hasTLS,
	}
	infraHash := infraConfig.ComputeHash()

	// Create resources in order (some depend on others existing)
	// ConfigMap must be created first so HTTPRoute controller can process routes immediately
	resources := []client.Object{
		r.buildVCLConfigMap(gateway, vclContent),
		r.buildAdminSecret(gateway),
	}
	// Add TLS bundle Secret if any HTTPS listeners with certs exist
	if hasTLS {
		resources = append(resources, r.buildTLSSecret(gateway, tlsCertData))
	}
	resources = append(resources,
		r.buildServiceAccount(gateway),
		r.buildClusterRoleBinding(gateway),
		r.buildDeployment(gateway, varnishdExtraArgs, logging, infraHash, hasTLS),
		r.buildService(gateway),
	)

	for _, desired := range resources {
		if err := r.reconcileResource(ctx, gateway, desired); err != nil {
			return err
		}
	}

	return nil
}

// reconcileResource creates or updates a single child resource.
func (r *GatewayReconciler) reconcileResource(ctx context.Context, gateway *gatewayv1.Gateway, desired client.Object) error {
	// Set owner reference only for namespace-scoped resources
	// Cluster-scoped resources (like ClusterRoleBinding) cannot have namespace-scoped owners
	if desired.GetNamespace() != "" {
		if err := controllerutil.SetControllerReference(gateway, desired, r.Scheme); err != nil {
			return fmt.Errorf("controllerutil.SetControllerReference: %w", err)
		}
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
			Name:       gateway.Name,
			Namespace:  gateway.Namespace,
			Generation: gateway.Generation,
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
	r.setListenerStatusesForPatch(ctx, patch, gateway)

	// Populate addresses from the Service's LoadBalancer status
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      gateway.Name,
		Namespace: gateway.Namespace,
	}, svc); err == nil {
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			if ingress.IP != "" {
				patch.Status.Addresses = append(patch.Status.Addresses, gatewayv1.GatewayStatusAddress{
					Type:  ptr(gatewayv1.IPAddressType),
					Value: ingress.IP,
				})
			}
			if ingress.Hostname != "" {
				patch.Status.Addresses = append(patch.Status.Addresses, gatewayv1.GatewayStatusAddress{
					Type:  ptr(gatewayv1.HostnameAddressType),
					Value: ingress.Hostname,
				})
			}
		}
	}

	// Apply the patch
	if err := r.Status().Patch(ctx, patch, client.Apply,
		client.FieldOwner("varnish-gateway-controller"),
		client.ForceOwnership); err != nil {
		return fmt.Errorf("r.Status().Patch: %w", err)
	}

	return nil
}

// setListenerStatusesForPatch sets listener statuses for SSA patch.
// Computes all listener status fields: conditions, SupportedKinds, and AttachedRoutes.
// AttachedRoutes is computed by listing HTTPRoutes attached to this Gateway.
func (r *GatewayReconciler) setListenerStatusesForPatch(ctx context.Context, patch *gatewayv1.Gateway, original *gatewayv1.Gateway) {
	// Build map of existing listener statuses to preserve condition times
	existingStatuses := make(map[gatewayv1.SectionName]gatewayv1.ListenerStatus)
	for _, ls := range original.Status.Listeners {
		existingStatuses[ls.Name] = ls
	}

	// List all accepted routes for this gateway to compute AttachedRoutes
	routes, err := listAcceptedRoutesForGateway(ctx, r.Client, original)
	if err != nil {
		r.Logger.Error("failed to list routes for AttachedRoutes computation", "error", err)
		// Continue with empty routes - AttachedRoutes will be 0
	}

	patch.Status.Listeners = make([]gatewayv1.ListenerStatus, 0, len(original.Spec.Listeners))

	for _, listener := range original.Spec.Listeners {
		existing, hasExisting := existingStatuses[listener.Name]

		// Preserve existing condition times if status unchanged
		acceptedTime := metav1.Now()
		programmedTime := metav1.Now()
		if hasExisting {
			for _, c := range existing.Conditions {
				if c.Type == string(gatewayv1.ListenerConditionAccepted) && c.Status == metav1.ConditionTrue {
					acceptedTime = c.LastTransitionTime
				}
				if c.Type == string(gatewayv1.ListenerConditionProgrammed) {
					programmedTime = c.LastTransitionTime
				}
			}
		}

		conditions := []metav1.Condition{
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
		}

		// Determine supported kinds and validate allowed route kinds
		supportedKinds, hasInvalidKinds := validateListenerRouteKinds(&listener)

		// Add ResolvedRefs condition for route kind validation
		if hasInvalidKinds {
			conditions = append(conditions, metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionResolvedRefs),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: original.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             string(gatewayv1.ListenerReasonInvalidRouteKinds),
				Message:            "One or more route kinds are not supported",
			})
		}

		// Add ResolvedRefs condition for HTTPS listeners
		if listener.Protocol == gatewayv1.HTTPSProtocolType {
			resolvedRefs := r.validateListenerTLSRefs(ctx, original, &listener)
			conditions = append(conditions, resolvedRefs)
		}

		// Add ResolvedRefs: True for non-HTTPS listeners that don't already have it
		// (e.g., HTTP listeners). The spec requires ResolvedRefs on all listeners.
		if !hasInvalidKinds && listener.Protocol != gatewayv1.HTTPSProtocolType {
			resolvedRefsTime := metav1.Now()
			if hasExisting {
				for _, c := range existing.Conditions {
					if c.Type == string(gatewayv1.ListenerConditionResolvedRefs) && c.Status == metav1.ConditionTrue {
						resolvedRefsTime = c.LastTransitionTime
					}
				}
			}
			conditions = append(conditions, metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionResolvedRefs),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: original.Generation,
				LastTransitionTime: resolvedRefsTime,
				Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
				Message:            "References resolved",
			})
		}

		// If any ResolvedRefs condition is False, override Programmed to False.
		// The Gateway API spec requires Programmed: False when refs are unresolved.
		for _, c := range conditions {
			if c.Type == string(gatewayv1.ListenerConditionResolvedRefs) && c.Status == metav1.ConditionFalse {
				for i := range conditions {
					if conditions[i].Type == string(gatewayv1.ListenerConditionProgrammed) {
						conditions[i].Status = metav1.ConditionFalse
						conditions[i].Reason = string(gatewayv1.ListenerReasonInvalid)
						conditions[i].Message = "Listener has unresolved references"
						break
					}
				}
				break
			}
		}

		// Compute AttachedRoutes for this listener
		attachedRoutes := countRoutesForListener(routes, listener, original)

		patch.Status.Listeners = append(patch.Status.Listeners, gatewayv1.ListenerStatus{
			Name:           listener.Name,
			SupportedKinds: supportedKinds,
			AttachedRoutes: attachedRoutes,
			Conditions:     conditions,
		})
	}
}

// validateListenerRouteKinds checks if the listener's AllowedRoutes.Kinds contains
// unsupported route kinds. Returns the filtered list of supported kinds and whether
// any invalid kinds were specified.
func validateListenerRouteKinds(listener *gatewayv1.Listener) ([]gatewayv1.RouteGroupKind, bool) {
	defaultKinds := []gatewayv1.RouteGroupKind{
		{
			Group: ptr(gatewayv1.Group("gateway.networking.k8s.io")),
			Kind:  "HTTPRoute",
		},
	}

	// If no kinds are explicitly specified, use the default based on protocol
	if listener.AllowedRoutes == nil || len(listener.AllowedRoutes.Kinds) == 0 {
		return defaultKinds, false
	}

	// Filter to only supported kinds
	var supported []gatewayv1.RouteGroupKind
	hasInvalid := false

	for _, kind := range listener.AllowedRoutes.Kinds {
		group := gatewayv1.Group("gateway.networking.k8s.io")
		if kind.Group != nil {
			group = *kind.Group
		}
		if group == "gateway.networking.k8s.io" && kind.Kind == "HTTPRoute" {
			supported = append(supported, gatewayv1.RouteGroupKind{
				Group: ptr(group),
				Kind:  kind.Kind,
			})
		} else {
			hasInvalid = true
		}
	}

	if supported == nil {
		supported = []gatewayv1.RouteGroupKind{}
	}
	return supported, hasInvalid
}

// validateListenerTLSRefs validates TLS certificate references for an HTTPS listener.
// Returns a ResolvedRefs condition reflecting the validation result.
func (r *GatewayReconciler) validateListenerTLSRefs(ctx context.Context, gateway *gatewayv1.Gateway, listener *gatewayv1.Listener) metav1.Condition {
	now := metav1.Now()

	if listener.TLS == nil || listener.TLS.Mode == nil || *listener.TLS.Mode != gatewayv1.TLSModeTerminate {
		return metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gateway.Generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
			Message:            "Refs resolved",
		}
	}

	if len(listener.TLS.CertificateRefs) == 0 {
		return metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gateway.Generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonInvalidCertificateRef),
			Message:            "HTTPS listener has no certificateRefs",
		}
	}

	for _, certRef := range listener.TLS.CertificateRefs {
		// Check for cross-namespace refs — require ReferenceGrant
		if certRef.Namespace != nil && string(*certRef.Namespace) != gateway.Namespace {
			allowed, err := IsReferenceAllowed(ctx, r.Client, CrossNamespaceRef{
				FromGroup:     "gateway.networking.k8s.io",
				FromKind:      "Gateway",
				FromNamespace: gateway.Namespace,
				ToGroup:       "",
				ToKind:        "Secret",
				ToNamespace:   string(*certRef.Namespace),
				ToName:        string(certRef.Name),
			})
			if err != nil {
				r.Logger.Error("failed to check ReferenceGrant",
					"secretNamespace", string(*certRef.Namespace),
					"secretName", string(certRef.Name),
					"error", err)
				return metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonRefNotPermitted),
					Message:            fmt.Sprintf("Failed to validate cross-namespace certificateRef %s/%s: %v", string(*certRef.Namespace), string(certRef.Name), err),
				}
			}
			if !allowed {
				return metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonRefNotPermitted),
					Message:            fmt.Sprintf("Cross-namespace certificateRef %s/%s not allowed by any ReferenceGrant", string(*certRef.Namespace), string(certRef.Name)),
				}
			}
		}

		// Validate group/kind
		if certRef.Group != nil && *certRef.Group != "" {
			return metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionResolvedRefs),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: gateway.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.ListenerReasonInvalidCertificateRef),
				Message:            fmt.Sprintf("Unsupported certificateRef group: %s", string(*certRef.Group)),
			}
		}
		if certRef.Kind != nil && *certRef.Kind != "Secret" {
			return metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionResolvedRefs),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: gateway.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.ListenerReasonInvalidCertificateRef),
				Message:            fmt.Sprintf("Unsupported certificateRef kind: %s", string(*certRef.Kind)),
			}
		}

		// Resolve Secret namespace (default to gateway namespace)
		secretNamespace := gateway.Namespace
		if certRef.Namespace != nil && string(*certRef.Namespace) != "" {
			secretNamespace = string(*certRef.Namespace)
		}

		// Fetch the Secret to check it exists
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{
			Name:      string(certRef.Name),
			Namespace: secretNamespace,
		}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonInvalidCertificateRef),
					Message:            fmt.Sprintf("Secret %s/%s not found", secretNamespace, string(certRef.Name)),
				}
			}
			return metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionResolvedRefs),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: gateway.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.ListenerReasonInvalidCertificateRef),
				Message:            fmt.Sprintf("Failed to get Secret %s/%s: %v", secretNamespace, string(certRef.Name), err),
			}
		}

		// Validate Secret type
		if secret.Type != corev1.SecretTypeTLS {
			return metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionResolvedRefs),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: gateway.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.ListenerReasonInvalidCertificateRef),
				Message:            fmt.Sprintf("Secret %s/%s has type %s, expected kubernetes.io/tls", secretNamespace, string(certRef.Name), secret.Type),
			}
		}

		// Validate tls.crt and tls.key fields exist and are non-empty
		if len(secret.Data["tls.crt"]) == 0 || len(secret.Data["tls.key"]) == 0 {
			return metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionResolvedRefs),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: gateway.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.ListenerReasonInvalidCertificateRef),
				Message:            fmt.Sprintf("Secret %s/%s missing tls.crt or tls.key data", secretNamespace, string(certRef.Name)),
			}
		}

		// Validate tls.crt contains valid PEM data
		if block, _ := pem.Decode(secret.Data["tls.crt"]); block == nil {
			return metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionResolvedRefs),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: gateway.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.ListenerReasonInvalidCertificateRef),
				Message:            fmt.Sprintf("Secret %s/%s tls.crt does not contain valid PEM data", secretNamespace, string(certRef.Name)),
			}
		}
	}

	return metav1.Condition{
		Type:               string(gatewayv1.ListenerConditionResolvedRefs),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gateway.Generation,
		LastTransitionTime: now,
		Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
		Message:            "All TLS certificate references resolved",
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

// collectTLSCertData iterates Gateway listeners and collects TLS certificate data
// from referenced Secrets. Returns a map of {secret-name}.pem → combined PEM data
// and a sorted list of referenced Secret names for the infrastructure hash.
func (r *GatewayReconciler) collectTLSCertData(ctx context.Context, gateway *gatewayv1.Gateway) map[string][]byte {
	certData := make(map[string][]byte)

	for _, listener := range gateway.Spec.Listeners {
		if listener.Protocol != gatewayv1.HTTPSProtocolType {
			continue
		}
		if listener.TLS == nil || listener.TLS.Mode == nil || *listener.TLS.Mode != gatewayv1.TLSModeTerminate {
			continue
		}

		for _, certRef := range listener.TLS.CertificateRefs {
			// Only support core/v1 Secrets
			if certRef.Group != nil && *certRef.Group != "" {
				continue
			}
			if certRef.Kind != nil && *certRef.Kind != "Secret" {
				continue
			}

			secretName := string(certRef.Name)
			secretNamespace := gateway.Namespace
			if certRef.Namespace != nil && string(*certRef.Namespace) != "" {
				secretNamespace = string(*certRef.Namespace)
			}

			// For cross-namespace refs, verify ReferenceGrant allows access
			if secretNamespace != gateway.Namespace {
				allowed, err := IsReferenceAllowed(ctx, r.Client, CrossNamespaceRef{
					FromGroup:     "gateway.networking.k8s.io",
					FromKind:      "Gateway",
					FromNamespace: gateway.Namespace,
					ToGroup:       "",
					ToKind:        "Secret",
					ToNamespace:   secretNamespace,
					ToName:        secretName,
				})
				if err != nil {
					r.Logger.Error("failed to check ReferenceGrant for TLS Secret",
						"secretNamespace", secretNamespace,
						"secretName", secretName,
						"error", err)
					continue
				}
				if !allowed {
					r.Logger.Warn("cross-namespace TLS certificateRef not allowed by ReferenceGrant, skipping",
						"secretNamespace", secretNamespace,
						"secretName", secretName,
						"gatewayNamespace", gateway.Namespace)
					continue
				}
			}

			// Use namespace-qualified PEM key for cross-namespace secrets to avoid collisions
			pemKey := secretName + ".pem"
			if secretNamespace != gateway.Namespace {
				pemKey = secretNamespace + "-" + secretName + ".pem"
			}

			// Avoid duplicates
			if _, exists := certData[pemKey]; exists {
				continue
			}

			var secret corev1.Secret
			if err := r.Get(ctx, types.NamespacedName{
				Name:      secretName,
				Namespace: secretNamespace,
			}, &secret); err != nil {
				if apierrors.IsNotFound(err) {
					r.Logger.Warn("TLS Secret not found",
						"secret", secretName, "namespace", secretNamespace)
				} else {
					r.Logger.Error("failed to get TLS Secret",
						"secret", secretName, "error", err)
				}
				continue
			}

			// Validate Secret type
			if secret.Type != corev1.SecretTypeTLS {
				r.Logger.Warn("TLS Secret has wrong type, expected kubernetes.io/tls",
					"secret", secretName, "type", secret.Type)
				continue
			}

			// Extract cert and key, concatenate into combined PEM
			cert := secret.Data["tls.crt"]
			key := secret.Data["tls.key"]
			if len(cert) == 0 || len(key) == 0 {
				r.Logger.Warn("TLS Secret missing tls.crt or tls.key",
					"secret", secretName)
				continue
			}

			// Combined PEM: cert first, then key
			combined := make([]byte, 0, len(cert)+len(key)+1)
			combined = append(combined, cert...)
			if cert[len(cert)-1] != '\n' {
				combined = append(combined, '\n')
			}
			combined = append(combined, key...)
			certData[pemKey] = combined
		}
	}

	return certData
}

// buildLabels returns labels for resources owned by a Gateway.
func (r *GatewayReconciler) buildLabels(gateway *gatewayv1.Gateway) map[string]string {
	return map[string]string{
		LabelManagedBy:        ManagedByValue,
		LabelGatewayName:      gateway.Name,
		LabelGatewayNamespace: gateway.Namespace,
	}
}

// gatewayClassNamesForParams returns GatewayClass names that reference any of the
// provided GatewayClassParameters names.
func (r *GatewayReconciler) gatewayClassNamesForParams(ctx context.Context, paramsNames map[string]struct{}) (map[string]struct{}, error) {
	classNames := make(map[string]struct{})
	if len(paramsNames) == 0 {
		return classNames, nil
	}

	var gatewayClasses gatewayv1.GatewayClassList
	if err := r.List(ctx, &gatewayClasses); err != nil {
		return nil, fmt.Errorf("r.List(GatewayClassList): %w", err)
	}

	for _, gc := range gatewayClasses.Items {
		if gc.Spec.ParametersRef == nil {
			continue
		}
		ref := gc.Spec.ParametersRef
		if string(ref.Group) != gatewayparamsv1alpha1.GroupName || string(ref.Kind) != "GatewayClassParameters" {
			continue
		}
		if _, ok := paramsNames[ref.Name]; !ok {
			continue
		}
		classNames[gc.Name] = struct{}{}
	}

	return classNames, nil
}

// gatewayRequestsForClassNames returns reconcile requests for all Gateways whose
// GatewayClassName is one of classNames.
func (r *GatewayReconciler) gatewayRequestsForClassNames(ctx context.Context, classNames map[string]struct{}) ([]ctrl.Request, error) {
	if len(classNames) == 0 {
		return nil, nil
	}

	var gateways gatewayv1.GatewayList
	if err := r.List(ctx, &gateways); err != nil {
		return nil, fmt.Errorf("r.List(GatewayList): %w", err)
	}

	requests := make([]ctrl.Request, 0, len(gateways.Items))
	for _, gw := range gateways.Items {
		if _, ok := classNames[string(gw.Spec.GatewayClassName)]; !ok {
			continue
		}
		requests = append(requests, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      gw.Name,
				Namespace: gw.Namespace,
			},
		})
	}

	return requests, nil
}

// enqueueGatewaysForParams returns an EventHandler that enqueues all Gateways
// that use a GatewayClass referencing the changed GatewayClassParameters.
func (r *GatewayReconciler) enqueueGatewaysForParams() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		params, ok := obj.(*gatewayparamsv1alpha1.GatewayClassParameters)
		if !ok {
			return nil
		}

		classNames, err := r.gatewayClassNamesForParams(ctx, map[string]struct{}{params.Name: {}})
		if err != nil {
			r.Logger.Error("failed to resolve GatewayClasses for GatewayClassParameters", "error", err)
			return nil
		}
		requests, err := r.gatewayRequestsForClassNames(ctx, classNames)
		if err != nil {
			r.Logger.Error("failed to list Gateways for GatewayClassParameters change", "error", err)
			return nil
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

		matchingParams := make(map[string]struct{})
		for _, params := range paramsList.Items {
			// Check if this params references our ConfigMap
			if params.Spec.UserVCLConfigMapRef == nil {
				continue
			}
			cmRef := params.Spec.UserVCLConfigMapRef
			if cmRef.Name != cm.Name || cmRef.Namespace != cm.Namespace {
				continue
			}
			matchingParams[params.Name] = struct{}{}
		}

		classNames, err := r.gatewayClassNamesForParams(ctx, matchingParams)
		if err != nil {
			r.Logger.Error("failed to resolve GatewayClasses for user VCL ConfigMap", "error", err)
			return nil
		}
		requests, err := r.gatewayRequestsForClassNames(ctx, classNames)
		if err != nil {
			r.Logger.Error("failed to list Gateways for user VCL ConfigMap change", "error", err)
			return nil
		}

		if len(requests) > 0 {
			r.Logger.Info("user VCL ConfigMap changed, enqueuing Gateways for reconciliation",
				"configmap", fmt.Sprintf("%s/%s", cm.Namespace, cm.Name), "gateways", len(requests))
		}

		return requests
	})
}

// enqueueGatewaysForTLSSecret returns an EventHandler that enqueues all Gateways
// that reference a changed TLS Secret in their listener.tls.certificateRefs.
// This watches the source TLS Secrets (user-created / cert-manager managed),
// not the bundle Secret we create (which is already watched via Owns).
func (r *GatewayReconciler) enqueueGatewaysForTLSSecret() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		secret, ok := obj.(*corev1.Secret)
		if !ok {
			return nil
		}

		// Only care about TLS secrets
		if secret.Type != corev1.SecretTypeTLS {
			return nil
		}

		// Skip secrets we own (our TLS bundle) — those are handled by Owns()
		if secret.Labels[LabelManagedBy] == ManagedByValue {
			return nil
		}

		// Find all Gateways that reference this Secret (including cross-namespace)
		var gateways gatewayv1.GatewayList
		if err := r.List(ctx, &gateways); err != nil {
			r.Logger.Error("failed to list Gateways", "error", err)
			return nil
		}

		var requests []ctrl.Request
		for _, gw := range gateways.Items {
			if string(gw.Spec.GatewayClassName) != r.Config.GatewayClassName {
				continue
			}
			if r.gatewayReferencesSecret(&gw, secret.Name, secret.Namespace) {
				requests = append(requests, ctrl.Request{
					NamespacedName: types.NamespacedName{
						Name:      gw.Name,
						Namespace: gw.Namespace,
					},
				})
			}
		}

		if len(requests) > 0 {
			r.Logger.Info("TLS Secret changed, enqueuing Gateways for reconciliation",
				"secret", fmt.Sprintf("%s/%s", secret.Namespace, secret.Name),
				"gateways", len(requests))
		}

		return requests
	})
}

// gatewayReferencesSecret checks if a Gateway references the named Secret
// in any of its listener TLS certificateRefs (same-namespace or cross-namespace).
func (r *GatewayReconciler) gatewayReferencesSecret(gateway *gatewayv1.Gateway, secretName, secretNamespace string) bool {
	for _, listener := range gateway.Spec.Listeners {
		if listener.TLS == nil {
			continue
		}
		for _, certRef := range listener.TLS.CertificateRefs {
			if string(certRef.Name) != secretName {
				continue
			}
			// Determine the effective namespace of the ref
			refNS := gateway.Namespace
			if certRef.Namespace != nil && string(*certRef.Namespace) != "" {
				refNS = string(*certRef.Namespace)
			}
			if refNS == secretNamespace {
				return true
			}
		}
	}
	return false
}

// enqueueGatewaysForReferenceGrant returns an EventHandler that enqueues
// Gateways when a ReferenceGrant changes, if the grant could affect
// cross-namespace TLS certificate references.
func (r *GatewayReconciler) enqueueGatewaysForReferenceGrant() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		grant, ok := obj.(*gatewayv1beta1.ReferenceGrant)
		if !ok {
			return nil
		}

		// Check if any To entry targets Secrets (core group, kind Secret)
		hasSecretTo := false
		for _, to := range grant.Spec.To {
			if (string(to.Group) == "" || string(to.Group) == "core") && string(to.Kind) == "Secret" {
				hasSecretTo = true
				break
			}
		}
		if !hasSecretTo {
			return nil
		}

		// Collect namespaces from From entries that allow Gateways
		fromNamespaces := make(map[string]bool)
		for _, from := range grant.Spec.From {
			if string(from.Group) == "gateway.networking.k8s.io" && string(from.Kind) == "Gateway" {
				fromNamespaces[string(from.Namespace)] = true
			}
		}
		if len(fromNamespaces) == 0 {
			return nil
		}

		// Find Gateways in those namespaces with cross-namespace cert refs
		// pointing to the grant's namespace
		var requests []ctrl.Request
		for ns := range fromNamespaces {
			var gateways gatewayv1.GatewayList
			if err := r.List(ctx, &gateways, client.InNamespace(ns)); err != nil {
				r.Logger.Error("failed to list Gateways for ReferenceGrant", "namespace", ns, "error", err)
				continue
			}
			for _, gw := range gateways.Items {
				if string(gw.Spec.GatewayClassName) != r.Config.GatewayClassName {
					continue
				}
				if gatewayHasCrossNSCertRefTo(&gw, grant.Namespace) {
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
			r.Logger.Info("ReferenceGrant changed, enqueuing Gateways for reconciliation",
				"grant", fmt.Sprintf("%s/%s", grant.Namespace, grant.Name),
				"gateways", len(requests))
		}

		return requests
	})
}

// gatewayHasCrossNSCertRefTo checks if a Gateway has any listener with a
// cross-namespace certificateRef pointing to the given target namespace.
func gatewayHasCrossNSCertRefTo(gateway *gatewayv1.Gateway, targetNamespace string) bool {
	for _, listener := range gateway.Spec.Listeners {
		if listener.TLS == nil {
			continue
		}
		for _, certRef := range listener.TLS.CertificateRefs {
			if certRef.Namespace != nil &&
				string(*certRef.Namespace) != gateway.Namespace &&
				string(*certRef.Namespace) == targetNamespace {
				return true
			}
		}
	}
	return false
}

// enqueueGatewaysForHTTPRoute returns an EventHandler that enqueues Gateways
// referenced by a changed HTTPRoute. This allows the Gateway controller to
// update AttachedRoutes counts when routes are created, updated, or deleted.
func (r *GatewayReconciler) enqueueGatewaysForHTTPRoute() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		route, ok := obj.(*gatewayv1.HTTPRoute)
		if !ok {
			return nil
		}

		var requests []ctrl.Request
		seen := make(map[types.NamespacedName]bool)

		for _, parentRef := range route.Spec.ParentRefs {
			// Skip non-Gateway refs
			if parentRef.Kind != nil && *parentRef.Kind != "Gateway" {
				continue
			}
			if parentRef.Group != nil && *parentRef.Group != gatewayv1.Group(gatewayv1.GroupName) {
				continue
			}

			// Determine namespace
			namespace := route.Namespace
			if parentRef.Namespace != nil {
				namespace = string(*parentRef.Namespace)
			}

			nn := types.NamespacedName{
				Name:      string(parentRef.Name),
				Namespace: namespace,
			}
			if seen[nn] {
				continue
			}
			seen[nn] = true

			// Verify the Gateway uses our GatewayClass before enqueuing
			var gw gatewayv1.Gateway
			if err := r.Get(ctx, nn, &gw); err != nil {
				continue
			}
			if string(gw.Spec.GatewayClassName) != r.Config.GatewayClassName {
				continue
			}

			requests = append(requests, ctrl.Request{NamespacedName: nn})
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
		// Note: ClusterRoleBinding is cluster-scoped, so it cannot be owned by namespace-scoped Gateway
		// We manage its lifecycle manually in reconcileResources without owner references
		Watches(
			&gatewayv1.HTTPRoute{},
			r.enqueueGatewaysForHTTPRoute(),
			// No GenerationChangedPredicate — we need to reconcile on any HTTPRoute
			// change (create/delete/update) to update AttachedRoutes counts.
		).
		Watches(
			&gatewayparamsv1alpha1.GatewayClassParameters{},
			r.enqueueGatewaysForParams(),
		).
		Watches(
			&corev1.ConfigMap{},
			r.enqueueGatewaysForConfigMap(),
		).
		Watches(
			&corev1.Secret{},
			r.enqueueGatewaysForTLSSecret(),
		).
		Watches(
			&gatewayv1beta1.ReferenceGrant{},
			r.enqueueGatewaysForReferenceGrant(),
		).
		Complete(r)
}

// ptr returns a pointer to the given value.
func ptr[T any](v T) *T {
	return &v
}
