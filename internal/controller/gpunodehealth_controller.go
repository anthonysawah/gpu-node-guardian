/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"fmt"
	"strconv"

	"github.com/anthonysawah/gpu-cluster-toolkit/gpu-node-guardian/internal/dcgm"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// Annotation read from Node objects to determine GPU error count.
	// In production this would come from DCGM exporter, not a manual annotation.
	errorCountAnnotation = "gpu.cluster.io/error-count"

	// Self-applied annotation marking nodes we cordoned.
	// Used for idempotency: don't uncordon nodes we didn't cordon.
	cordonedByUsAnnotation = "gpu-node-guardian/cordoned"

	// Cordon threshold: error count above this triggers cordon.
	errorCountThreshold = 5
)

// GPUNodeHealthReconciler reconciles GPU node health.
type GPUNodeHealthReconciler struct {
	client.Client
	Recorder record.EventRecorder
	DCGM     *dcgm.Client
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is the main control loop.
// Triggered when a Node object changes.
func (r *GPUNodeHealthReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("node", req.Name)

	// Fetch the Node.
	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		if apierrors.IsNotFound(err) {
			// Node was deleted; nothing to do.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting node: %w", err)
	}

	// Read the error-count annotation. Absent or unparseable means 0.
	errorCount := 0
	if val, ok := node.Annotations[errorCountAnnotation]; ok {
		parsed, err := strconv.Atoi(val)
		if err != nil {
			logger.Info("invalid error-count annotation, treating as 0", "value", val)
		} else {
			errorCount = parsed
		}
	}

	// Did we cordon this node?
	_, weCordoned := node.Annotations[cordonedByUsAnnotation]

	logger.V(1).Info("reconcile",
		"errorCount", errorCount,
		"unschedulable", node.Spec.Unschedulable,
		"weCordoned", weCordoned,
	)

	// Decision matrix:
	// 1. errorCount > threshold AND not cordoned -> cordon
	// 2. errorCount <= threshold AND we cordoned it -> uncordon
	// 3. anything else -> no-op

	switch {
	case errorCount > errorCountThreshold && !node.Spec.Unschedulable:
		return r.cordonNode(ctx, &node, errorCount)

	case errorCount <= errorCountThreshold && weCordoned:
		return r.uncordonNode(ctx, &node, errorCount)

	default:
		return ctrl.Result{}, nil
	}
}

// cordonNode marks the node unschedulable and records the action.
func (r *GPUNodeHealthReconciler) cordonNode(ctx context.Context, node *corev1.Node, errorCount int) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("node", node.Name)

	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = true
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[cordonedByUsAnnotation] = "true"

	if err := r.Patch(ctx, node, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("cordoning node: %w", err)
	}

	logger.Info("cordoned node due to GPU errors", "errorCount", errorCount)
	r.Recorder.Eventf(node, corev1.EventTypeWarning, "NodeCordoned",
		"Cordoned due to GPU error count %d (threshold %d)", errorCount, errorCountThreshold)

	return ctrl.Result{}, nil
}

// uncordonNode marks the node schedulable again and removes our marker.
func (r *GPUNodeHealthReconciler) uncordonNode(ctx context.Context, node *corev1.Node, errorCount int) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("node", node.Name)

	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = false
	delete(node.Annotations, cordonedByUsAnnotation)

	if err := r.Patch(ctx, node, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("uncordoning node: %w", err)
	}

	logger.Info("uncordoned node, errors recovered", "errorCount", errorCount)
	r.Recorder.Eventf(node, corev1.EventTypeNormal, "NodeUncordoned",
		"Uncordoned, GPU error count recovered to %d", errorCount)

	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the manager and tells it
// to watch Node objects.
func (r *GPUNodeHealthReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("gpu-node-guardian")
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Named("gpunodehealth").
		Complete(r)
}
