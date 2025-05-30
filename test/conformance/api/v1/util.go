/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"testing"

	"golang.org/x/sync/errgroup"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	pkgTest "knative.dev/pkg/test"
	"knative.dev/pkg/test/spoof"
	"knative.dev/serving/pkg/apis/serving"
	v1 "knative.dev/serving/pkg/apis/serving/v1"
	"knative.dev/serving/test"
	"knative.dev/serving/test/conformance/api/shared"
	v1test "knative.dev/serving/test/v1"
)

type tagExpectation struct {
	tag              string
	expectedResponse string
}

func checkForExpectedResponses(ctx context.Context, t testing.TB, clients *test.Clients, url *url.URL, expectedResponses ...string) error {
	client, err := pkgTest.NewSpoofingClient(ctx, clients.KubeClient, t.Logf, url.Hostname(), test.ServingFlags.ResolvableDomain, test.AddRootCAtoTransport(ctx, t.Logf, clients, test.ServingFlags.HTTPS))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url.String(), nil)
	if err != nil {
		return err
	}
	spoof.WithHeader(test.ServingFlags.RequestHeader())(req)
	_, err = client.Poll(req, spoof.MatchesAllOf(spoof.IsStatusOK, spoof.MatchesAllBodies(expectedResponses...)))
	return err
}

func validateDomains(t testing.TB, clients *test.Clients, serviceName string,
	baseExpected []string, tagExpectationPairs []tagExpectation,
) error {
	service, err := clients.ServingClient.Services.Get(context.Background(), serviceName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("could not query service traffic status: %w", err)
	}
	baseDomain := service.Status.URL.URL()
	subdomains := make([]*url.URL, len(tagExpectationPairs))
	tagToTraffic := make(map[string]*url.URL)

	for _, traffic := range service.Status.Traffic {
		tagToTraffic[traffic.Tag] = traffic.URL.URL()
	}

	for i, pair := range tagExpectationPairs {
		url, found := tagToTraffic[pair.tag]
		if !found {
			return fmt.Errorf("no subdomain found for tag %s in service status", pair.tag)
		}
		subdomains[i] = url
		delete(tagToTraffic, pair.tag)
	}

	if len(tagToTraffic) != 0 {
		return fmt.Errorf("unexpected tags were found in the service %v", tagToTraffic)
	}

	g, egCtx := errgroup.WithContext(context.Background())
	// We don't have a good way to check if the route is updated so we will wait until a subdomain has
	// started returning at least one expected result to key that we should validate percentage splits.
	// In order for tests to succeed reliably, we need to make sure that all domains succeed.
	g.Go(func() error {
		t.Log("Checking updated route", baseDomain)
		return checkForExpectedResponses(egCtx, t, clients, baseDomain, baseExpected...)
	})
	for i, s := range subdomains {
		g.Go(func() error {
			t.Log("Checking updated route tags", s)
			return checkForExpectedResponses(egCtx, t, clients, s, tagExpectationPairs[i].expectedResponse)
		})
	}
	if err := g.Wait(); err != nil {
		return fmt.Errorf("error with initial domain probing: %w", err)
	}

	g, egCtx = errgroup.WithContext(context.Background())
	g.Go(func() error {
		minBasePercentage := test.MinSplitPercentage
		if len(baseExpected) == 1 {
			minBasePercentage = test.MinDirectPercentage
		}
		min := int(math.Floor(test.ConcurrentRequests * minBasePercentage))
		return shared.CheckDistribution(egCtx, t, clients, baseDomain, test.ConcurrentRequests, min, baseExpected, test.ServingFlags.ResolvableDomain)
	})
	for i, subdomain := range subdomains {
		g.Go(func() error {
			min := int(math.Floor(test.ConcurrentRequests * test.MinDirectPercentage))
			return shared.CheckDistribution(egCtx, t, clients, subdomain, test.ConcurrentRequests, min, []string{tagExpectationPairs[i].expectedResponse}, test.ServingFlags.ResolvableDomain)
		})
	}
	if err := g.Wait(); err != nil {
		return fmt.Errorf("error checking routing distribution: %w", err)
	}
	return nil
}

// Validates service health and vended content match for a runLatest Service.
// The checks in this method should be able to be performed at any point in a
// runLatest Service's lifecycle so long as the service is in a "Ready" state.
func validateDataPlane(t testing.TB, clients *test.Clients, names test.ResourceNames, expectedText string) error {
	t.Log("Checking that the endpoint vends the expected text:", expectedText)
	_, err := pkgTest.CheckEndpointState(
		context.Background(),
		clients.KubeClient,
		t.Logf,
		names.URL,
		spoof.MatchesAllOf(spoof.IsStatusOK, spoof.MatchesBody(expectedText)),
		"WaitForEndpointToServeText",
		test.ServingFlags.ResolvableDomain,
		test.AddRootCAtoTransport(context.Background(), t.Logf, clients, test.ServingFlags.HTTPS),
		spoof.WithHeader(test.ServingFlags.RequestHeader()))
	if err != nil {
		return fmt.Errorf("the endpoint for Route %s at %s didn't serve the expected text %q: %w", names.Route, names.URL, expectedText, err)
	}

	return nil
}

// Validates the state of Configuration, Revision, and Route objects for a runLatest Service.
// The checks in this method should be able to be performed at any point in a
// runLatest Service's lifecycle so long as the service is in a "Ready" state.
func validateControlPlane(t *testing.T, clients *test.Clients, names test.ResourceNames, expectedGeneration string) error {
	t.Log("Checking to ensure Revision is in desired state with", "generation", expectedGeneration)
	if err := v1test.CheckRevisionState(clients.ServingClient, names.Revision, func(r *v1.Revision) (bool, error) {
		if ready, err := v1test.IsRevisionReady(r); !ready {
			return false, fmt.Errorf("revision %s did not become ready to serve traffic: %w", names.Revision, err)
		}
		images := append([]string{names.Image}, names.Sidecars...)
		for i, v := range r.Status.ContainerStatuses {
			if validDigest, err := shared.ValidateImageDigest(t, images[i], v.ImageDigest); !validDigest {
				return false, fmt.Errorf("imageDigest %s is not valid for imageName %s: %w", v.ImageDigest, images[i], err)
			}
		}
		return true, nil
	}); err != nil {
		return err
	}

	if err := v1test.CheckRevisionState(clients.ServingClient, names.Revision, v1test.IsRevisionAtExpectedGeneration(expectedGeneration)); err != nil {
		return fmt.Errorf("revision %s did not have an expected annotation with generation %s: %w", names.Revision, expectedGeneration, err)
	}

	t.Log("Checking to ensure Configuration is in desired state.")
	if err := v1test.CheckConfigurationState(clients.ServingClient, names.Config, func(c *v1.Configuration) (bool, error) {
		if c.Status.LatestCreatedRevisionName != names.Revision {
			return false, fmt.Errorf("Configuration(%s).LatestCreatedRevisionName = %q, want %q",
				names.Config, c.Status.LatestCreatedRevisionName, names.Revision)
		}
		if c.Status.LatestReadyRevisionName != names.Revision {
			return false, fmt.Errorf("Configuration(%s).LatestReadyRevisionName = %q, want %q",
				names.Config, c.Status.LatestReadyRevisionName, names.Revision)
		}
		return true, nil
	}); err != nil {
		return err
	}

	t.Log("Checking to ensure Route is in desired state with", "generation", expectedGeneration)
	if err := v1test.CheckRouteState(clients.ServingClient, names.Route, v1test.AllRouteTrafficAtRevision(names)); err != nil {
		return fmt.Errorf("the Route %s was not updated to route traffic to the Revision %s: %w", names.Route, names.Revision, err)
	}

	return nil
}

// Validates labels on Revision, Configuration, and Route objects when created by a Service
// see spec here: https://github.com/knative/serving/blob/main/docs/spec/spec.md#revision
func validateLabelsPropagation(t testing.TB, objects v1test.ResourceObjects, names test.ResourceNames) error {
	t.Log("Validate Labels on Revision Object")
	revision := objects.Revision

	if revision.Labels["serving.knative.dev/configuration"] != names.Config {
		return fmt.Errorf("expect Configuration name in Revision label %q but got %q ", names.Config, revision.Labels["serving.knative.dev/configuration"])
	}
	if revision.Labels["serving.knative.dev/service"] != names.Service {
		return fmt.Errorf("expect Service name in Revision label %q but got %q ", names.Service, revision.Labels["serving.knative.dev/service"])
	}

	t.Log("Validate Labels on Configuration Object")
	config := objects.Config
	if config.Labels["serving.knative.dev/service"] != names.Service {
		return fmt.Errorf("expect Service name in Configuration label %q but got %q ", names.Service, config.Labels["serving.knative.dev/service"])
	}

	t.Log("Validate Labels on Route Object")
	route := objects.Route
	if route.Labels["serving.knative.dev/service"] != names.Service {
		return fmt.Errorf("expect Service name in Route label %q but got %q ", names.Service, route.Labels["serving.knative.dev/service"])
	}
	return nil
}

func validateK8sServiceLabels(t *testing.T, clients *test.Clients, names test.ResourceNames, extraKeys ...string) error {
	t.Log("Validate Labels on Kubernetes Service Object")
	k8sService, err := clients.KubeClient.CoreV1().Services(test.ServingFlags.TestNamespace).Get(context.Background(), names.Service, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get k8s service %v: %w", names.Service, err)
	}
	labels := k8sService.GetLabels()
	if labels["serving.knative.dev/service"] != names.Service {
		return fmt.Errorf("expected Service name in K8s Service label %q but got %q ", names.Service, labels["serving.knative.dev/service"])
	}
	if labels["serving.knative.dev/route"] != names.Service {
		return fmt.Errorf("expected Route name in K8s Service label %q but got %q ", names.Service, labels["serving.knative.dev/route"])
	}
	for _, extraKey := range extraKeys {
		if got := labels[extraKey]; got == "" {
			return fmt.Errorf("expected %s label to be set, but was empty", extraKey)
		}
	}
	return nil
}

func validateAnnotations(objs *v1test.ResourceObjects, extraKeys ...string) error {
	// This checks whether the annotations are set on the resources that
	// expect them to have.
	// List of issues listing annotations that we check: #1642.

	anns := objs.Service.GetAnnotations()
	for _, a := range append([]string{serving.CreatorAnnotation, serving.UpdaterAnnotation}, extraKeys...) {
		if got := anns[a]; got == "" {
			return fmt.Errorf("service expected %s annotation to be set, but was empty", a)
		}
	}
	anns = objs.Route.GetAnnotations()
	for _, a := range append([]string{serving.CreatorAnnotation, serving.UpdaterAnnotation}, extraKeys...) {
		if got := anns[a]; got == "" {
			return fmt.Errorf("route expected %s annotation to be set, but was empty", a)
		}
	}
	anns = objs.Config.GetAnnotations()
	for _, a := range append([]string{serving.CreatorAnnotation, serving.UpdaterAnnotation}, extraKeys...) {
		if got := anns[a]; got == "" {
			return fmt.Errorf("config expected %s annotation to be set, but was empty", a)
		}
	}
	return nil
}

func validateK8sServiceAnnotations(t *testing.T, clients *test.Clients, names test.ResourceNames, extraKeys ...string) error {
	t.Log("Validate Annotations on Kubernetes Service Object")
	k8sService, err := clients.KubeClient.CoreV1().Services(test.ServingFlags.TestNamespace).Get(context.Background(), names.Service, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get k8s service %v: %w", names.Service, err)
	}
	annotations := k8sService.GetAnnotations()
	for _, extraKey := range append([]string{serving.CreatorAnnotation, serving.UpdaterAnnotation}, extraKeys...) {
		if got := annotations[extraKey]; got == "" {
			return fmt.Errorf("expected %s annotation to be set, but was empty", extraKey)
		}
	}
	return nil
}

func validateReleaseServiceShape(objs *v1test.ResourceObjects) error {
	// Traffic should be routed to the lastest created revision.
	if got, want := objs.Service.Status.Traffic[0].RevisionName, objs.Config.Status.LatestReadyRevisionName; got != want {
		return fmt.Errorf("Status.Traffic[0].RevisionsName = %s, want: %s", got, want)
	}
	return nil
}
