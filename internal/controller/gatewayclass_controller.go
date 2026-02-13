package controller

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/varnish/gateway/internal/status"
)

// GatewayClassReconciler reconciles GatewayClass objects.
type GatewayClassReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config Config
	Logger *slog.Logger
}

// Reconcile handles GatewayClass reconciliation.
func (r *GatewayClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Logger.With("gatewayClass", req.Name)
	log.Debug("reconciling GatewayClass")

	var gc gatewayv1.GatewayClass
	if err := r.Get(ctx, req.NamespacedName, &gc); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("r.Get(%s): %w", req.Name, err)
	}

	// Only manage GatewayClasses that reference our controller
	if string(gc.Spec.ControllerName) != ControllerName {
		return ctrl.Result{}, nil
	}

	status.SetGatewayClassAccepted(&gc, true, string(gatewayv1.GatewayClassReasonAccepted), "GatewayClass is accepted")

	if err := r.Status().Update(ctx, &gc); err != nil {
		return ctrl.Result{}, fmt.Errorf("Status().Update(%s): %w", req.Name, err)
	}

	log.Info("GatewayClass accepted")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.GatewayClass{}).
		Complete(r)
}
