// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !plan9

package main

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	tsapi "tailscale.com/k8s-operator/apis/v1alpha1"
)

type NodeConnectorReconciler struct {
	client.Client

	l *zap.SugaredLogger
}

func (a *NodeConnectorReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := a.l.With("Node", req.Name)
	logger.Debugf("starting reconcile")
	defer logger.Debugf("reconcile finished")

	node := &corev1.Node{}
	err := a.Get(ctx, req.NamespacedName, node)
	if apierrors.IsNotFound(err) {
		// We have nothing to do for the connector since we set the
		// owner reference. Kubernetes GC will take care of deleting
		// it.
		logger.Debugf("Node not found, assuming it was deleted")
		return reconcile.Result{}, nil
	} else if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get Node: %w", err)
	}

	// Get the connector for this node
	desirecConnector := a.desiredConnector(node)

	res, err := a.createOrUpdateConnector(ctx, node, desirecConnector)
	switch res {
	case controllerutil.OperationResultCreated:
		logger.Infof("Connector created")
	case controllerutil.OperationResultUpdated:
		logger.Infof("Connector updated")
	case controllerutil.OperationResultNone:
		if err != nil {
			logger.Warn("Connector couldn't be created or updated: %v", err)
		} else {
			logger.Debugf("Connector unchanged")
		}
	default:
	}

	return reconcile.Result{}, nil
}

func (a *NodeConnectorReconciler) desiredConnector(node *corev1.Node) *tsapi.Connector {
	podCIDRRoutes := []tsapi.Route{}
	for _, podCIDR := range node.Spec.PodCIDRs {
		podCIDRRoutes = append(podCIDRRoutes, tsapi.Route(podCIDR))
	}
	return &tsapi.Connector{
		ObjectMeta: metav1.ObjectMeta{
			Name: node.Name,
		},
		Spec: tsapi.ConnectorSpec{
			SubnetRouter: &tsapi.SubnetRouter{
				AdvertiseRoutes: podCIDRRoutes,
			},
		},
	}
}

func (a *NodeConnectorReconciler) createOrUpdateConnector(ctx context.Context, node *corev1.Node, desiredConnector *tsapi.Connector) (controllerutil.OperationResult, error) {
	cn := &tsapi.Connector{
		ObjectMeta: metav1.ObjectMeta{
			Name: desiredConnector.Name,
		},
	}
	return controllerutil.CreateOrUpdate(ctx, a.Client, cn, func() error {
		cn.Spec = desiredConnector.Spec
		return ctrl.SetControllerReference(node, cn, a.Scheme())
	})
}
