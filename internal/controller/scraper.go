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
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/anthonysawah/gpu-cluster-toolkit/gpu-node-guardian/internal/dcgm"
)

const (
	// How often to scrape DCGM. In production this would come from config.
	scrapeInterval = 30 * time.Second

	// Annotations we set on each node based on scraped metrics.
	tempAnnotation       = "gpu.cluster.io/max-temp"
	xidErrorsAnnotation  = "gpu.cluster.io/xid-errors"
	lastScrapeAnnotation = "gpu.cluster.io/last-scrape"
)

// Scraper periodically pulls metrics from DCGM and updates node annotations.
// It implements controller-runtime's Runnable interface.
type Scraper struct {
	K8sClient client.Client
	DCGM      *dcgm.Client
	Interval  time.Duration
}

// Start runs the scrape loop until the context is cancelled.
// This satisfies controller-runtime's Runnable interface.
func (s *Scraper) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("dcgm-scraper")
	logger.Info("starting scrape loop", "interval", s.Interval)

	// Run an initial scrape immediately so we don't wait 30s for the first one.
	if err := s.scrapeOnce(ctx); err != nil {
		logger.Error(err, "initial scrape failed")
	}

	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("scrape loop exiting")
			return nil
		case <-ticker.C:
			if err := s.scrapeOnce(ctx); err != nil {
				logger.Error(err, "scrape failed")
				// keep going; transient errors are expected
			}
		}
	}
}

// scrapeOnce runs a single scrape and updates node annotations.
func (s *Scraper) scrapeOnce(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("dcgm-scraper")

	// Fetch metrics from DCGM exporter.
	healthByHost, err := s.DCGM.Scrape(ctx)
	if err != nil {
		return fmt.Errorf("scraping dcgm: %w", err)
	}

	logger.V(1).Info("scrape complete", "nodes", len(healthByHost))

	// For each node we got data for, update its annotations.
	for hostname, health := range healthByHost {
		if err := s.updateNodeAnnotations(ctx, hostname, health); err != nil {
			logger.Error(err, "updating annotations", "node", hostname)
			// keep going to other nodes
		}
	}

	return nil
}

// updateNodeAnnotations writes the scraped health data onto a node's annotations.
// This triggers Reconcile, which then makes the cordon decision.
func (s *Scraper) updateNodeAnnotations(ctx context.Context, hostname string, health *dcgm.NodeHealth) error {
	var node corev1.Node
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: hostname}, &node); err != nil {
		return fmt.Errorf("getting node %s: %w", hostname, err)
	}

	patch := client.MergeFrom(node.DeepCopy())
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}

	// Write the scraped values onto the node.
	node.Annotations[tempAnnotation] = fmt.Sprintf("%.1f", health.MaxTemp)
	node.Annotations[xidErrorsAnnotation] = strconv.Itoa(health.XIDErrors)
	node.Annotations[errorCountAnnotation] = strconv.Itoa(health.XIDErrors) // drives the existing reconcile logic
	node.Annotations[lastScrapeAnnotation] = time.Now().UTC().Format(time.RFC3339)

	if err := s.K8sClient.Patch(ctx, &node, patch); err != nil {
		return fmt.Errorf("patching node %s: %w", hostname, err)
	}

	return nil
}
